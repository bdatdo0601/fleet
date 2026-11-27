// Package cli implements the `fleet` command-line interface.
package cli

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"golang.org/x/term"

	"github.com/bdatdo0601/fleet/internal/client"
	"github.com/bdatdo0601/fleet/internal/codes"
	"github.com/bdatdo0601/fleet/internal/core"
	"github.com/bdatdo0601/fleet/internal/ddns"
	"github.com/bdatdo0601/fleet/internal/hooks"
	"github.com/bdatdo0601/fleet/internal/mcp"
	"github.com/bdatdo0601/fleet/internal/mcpinstall"
	"github.com/bdatdo0601/fleet/internal/memory"
	"github.com/bdatdo0601/fleet/internal/paths"
	"github.com/bdatdo0601/fleet/internal/pki"
	"github.com/bdatdo0601/fleet/internal/registry"
	"github.com/bdatdo0601/fleet/internal/render"
	"github.com/bdatdo0601/fleet/internal/server"
	"github.com/bdatdo0601/fleet/internal/system"
	"github.com/bdatdo0601/fleet/internal/usage"
	"github.com/bdatdo0601/fleet/internal/version"
)

// IO holds overridable streams and prompt hooks for CLI testing.
type IO struct {
	Stdin         io.Reader
	Stdout        io.Writer
	Stderr        io.Writer
	AskPassphrase func(newPass bool) (string, error)
	AskSecret     func(prompt string) (string, error)
	ReadOPRef     func(ref string) (string, error)
	ConfirmFn     func(question string) error
	br            *bufio.Reader
	brSrc         io.Reader
}

func (cio *IO) stdinReader() *bufio.Reader {
	if cio.br == nil || cio.brSrc != cio.Stdin {
		cio.brSrc = cio.Stdin
		cio.br = bufio.NewReader(cio.Stdin)
	}
	return cio.br
}

func defaultIO() *IO {
	return &IO{
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	}
}

func (cio *IO) printf(format string, args ...any) {
	_, _ = fmt.Fprintf(cio.Stdout, format, args...)
}

func (cio *IO) info(msg string) {
	cio.printf("==> %s\n", msg)
}

func (cio *IO) confirm(question string) error {
	if cio.ConfirmFn != nil {
		return cio.ConfirmFn(question)
	}
	cio.printf("%s [y/N] ", question)
	line, _ := cio.stdinReader().ReadString('\n')
	ans := strings.ToLower(strings.TrimSpace(line))
	if ans != "y" && ans != "yes" {
		return fmt.Errorf("cancelled")
	}
	return nil
}

func (cio *IO) askPassphrase(newPass bool) (string, error) {
	if cio.AskPassphrase != nil {
		return cio.AskPassphrase(newPass)
	}
	readPass := func(prompt string) (string, error) {
		_, _ = fmt.Fprint(cio.Stderr, prompt)
		if f, ok := cio.Stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
			b, err := term.ReadPassword(int(f.Fd()))
			_, _ = fmt.Fprintln(cio.Stderr)
			if err != nil {
				return "", err
			}
			return string(b), nil
		}
		ln, err := cio.stdinReader().ReadString('\n')
		if err != nil && len(ln) == 0 {
			return "", err
		}
		return strings.TrimRight(ln, "\r\n"), nil
	}
	pw, err := readPass("CA passphrase: ")
	if err != nil {
		return "", err
	}
	if newPass {
		if len(pw) < 12 {
			return "", fmt.Errorf("use at least 12 characters")
		}
		pw2, err := readPass("Repeat passphrase: ")
		if err != nil {
			return "", err
		}
		if pw2 != pw {
			return "", fmt.Errorf("passphrases don't match")
		}
	}
	return pw, nil
}

func (cio *IO) askSecret(prompt string) (string, error) {
	if cio.AskSecret != nil {
		return cio.AskSecret(prompt)
	}
	_, _ = fmt.Fprint(cio.Stderr, prompt)
	if f, ok := cio.Stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		b, err := term.ReadPassword(int(f.Fd()))
		_, _ = fmt.Fprintln(cio.Stderr)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	}
	ln, err := cio.stdinReader().ReadString('\n')
	if err != nil && len(ln) == 0 {
		return "", err
	}
	return strings.TrimSpace(ln), nil
}

func (cio *IO) readOPRef(ref string) (string, error) {
	if cio.ReadOPRef != nil {
		return cio.ReadOPRef(ref)
	}
	if _, err := exec.LookPath("op"); err != nil {
		return "", fmt.Errorf("1Password CLI (`op`) not found: install with `brew install 1password-cli`")
	}
	out, err := exec.Command("op", "read", ref).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
			return "", fmt.Errorf("op read %s: %s", ref, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return "", fmt.Errorf("op read %s: %w", ref, err)
	}
	val := strings.TrimSpace(string(out))
	if val == "" {
		return "", fmt.Errorf("op read %s returned empty secret", ref)
	}
	return val, nil
}

func (cio *IO) paste(prompt, endMarker string) (string, error) {
	if f, ok := cio.Stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		cio.printf("%s\n", prompt)
	}
	text, err := codes.ReadBlock(endMarker, cio.stdinReader())
	if err != nil {
		return "", err
	}
	if !strings.Contains(text, endMarker) {
		return "", fmt.Errorf("input ended before the end of the block")
	}
	return text, nil
}

// parseFlagsInterspersed parses flags anywhere among positional arguments, matching Python argparse.
func parseFlagsInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(io.Discard)
	isBool := func(name string) (known bool, b bool) {
		f := fs.Lookup(name)
		if f == nil {
			return false, false
		}
		if bf, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && bf.IsBoolFlag() {
			return true, true
		}
		return true, false
	}
	var flagArgs, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if strings.HasPrefix(a, "-") && len(a) > 1 {
			trimmed := strings.TrimPrefix(strings.TrimPrefix(a, "--"), "-")
			if trimmed == "h" || trimmed == "help" {
				return nil, flag.ErrHelp
			}
			if found := strings.Contains(trimmed, "="); found {
				flagArgs = append(flagArgs, a)
				continue
			}
			known, b := isBool(trimmed)
			if !known {
				return nil, fmt.Errorf("unknown flag: %s", a)
			}
			flagArgs = append(flagArgs, a)
			if !b {
				if i+1 >= len(args) {
					return nil, fmt.Errorf("flag needs an argument: %s", a)
				}
				i++
				flagArgs = append(flagArgs, args[i])
			}
			continue
		}
		pos = append(pos, a)
	}
	if err := fs.Parse(flagArgs); err != nil {
		return nil, err
	}
	return pos, nil
}

func currentUsername() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	if s := os.Getenv("USER"); s != "" {
		return s
	}
	return "root"
}

func requireNotRoot(cmd string) error {
	if system.IsRoot() {
		return fmt.Errorf("run `fleet %s` as your normal user (it uses sudo where needed)", cmd)
	}
	return nil
}

func installNodeFiles(keyPEM *string, ca, cert, config string, node client.NodeConfig) error {
	etc := paths.NebulaEtc()
	if keyPEM != nil {
		if _, err := system.WriteRootFile(filepath.Join(etc, "host.key"), *keyPEM, 0o600); err != nil {
			return err
		}
	}
	if _, err := system.WriteRootFile(filepath.Join(etc, "ca.crt"), ca, 0o644); err != nil {
		return err
	}
	if _, err := system.WriteRootFile(filepath.Join(etc, "host.crt"), cert, 0o644); err != nil {
		return err
	}
	if _, err := system.WriteRootFile(filepath.Join(etc, "config.yml"), config, 0o644); err != nil {
		return err
	}
	nodeBytes, err := json.MarshalIndent(node, "", "  ")
	if err != nil {
		return err
	}
	_, err = system.WriteRootFile(paths.NodeFile(), string(nodeBytes)+"\n", 0o644)
	return err
}

func (cio *IO) checkLighthouse() {
	cio.info(fmt.Sprintf("checking the lighthouse (%s) ...", paths.LighthouseIP))
	err := exec.Command("ping", "-c", "3", "-t", "15", paths.LighthouseIP).Run()
	if err == nil {
		cio.printf("    reachable\n")
	} else {
		cio.printf("    not reachable yet; see `fleet status` and /opt/homebrew/var/log/nebula.log\n")
	}
}

func ago(ts *string) string {
	if ts == nil || *ts == "" {
		return "never"
	}
	t, err := time.Parse(time.RFC3339, *ts)
	if err != nil {
		return *ts
	}
	secs := max(int(time.Since(t).Seconds()), 0)
	for _, unit := range []struct {
		label string
		size  int
	}{{"d", 86400}, {"h", 3600}, {"m", 60}} {
		if secs >= unit.size {
			return fmt.Sprintf("%d%s ago", secs/unit.size, unit.label)
		}
	}
	return "just now"
}

func printTable(w io.Writer, rows [][]string) {
	if len(rows) == 0 {
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, r := range rows {
		_, _ = fmt.Fprintln(tw, strings.Join(r, "\t"))
	}
	_ = tw.Flush()
}

func withPassphrase(cio *IO, path string, payload map[string]any, attempts int, out any) error {
	if attempts <= 0 {
		attempts = 3
	}
	api, err := client.ResolveAPI("")
	if err != nil {
		return err
	}
	for attempt := 1; attempt <= attempts; attempt++ {
		pw, err := cio.askPassphrase(false)
		if err != nil {
			return err
		}
		body := make(map[string]any, len(payload)+1)
		maps.Copy(body, payload)
		body["passphrase"] = pw
		err = client.Call(api, "POST", path, body, 30*time.Second, nil, out)
		if err == nil {
			return nil
		}
		var apiErr *client.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == 401 && attempt < attempts {
			cio.printf("wrong CA passphrase, try again\n")
			continue
		}
		return err
	}
	return fmt.Errorf("passphrase attempts exhausted")
}

func printMobile(cio *IO, resp core.ApproveResult) {
	cio.printf("\n--- Certificate (paste into the app) ---\n%s\n", strings.TrimSpace(resp.Cert))
	cio.printf("\n--- CA (paste into the app) ---\n%s\n", strings.TrimSpace(resp.CA))
	if resp.Lighthouse != nil {
		cio.printf("\n--- Hosts ---\n  %s  lighthouse: yes  address: %s\n", resp.Lighthouse.IP, resp.Lighthouse.Address)
		cio.printf("\n--- Relays ---\n  %s  (add under Relays in Mobile Nebula for bidirectional relay)\n\n", resp.Lighthouse.IP)
	}
	cio.info(fmt.Sprintf("%s -> %s, cert expires %s", resp.Name, resp.IP, resp.NotAfter))
}

func ensureInstalledSelf() (string, error) {
	target := paths.InstalledBinaryPath()
	exe, err := os.Executable()
	if err != nil {
		return target, err
	}
	if curReal, err1 := filepath.EvalSymlinks(exe); err1 == nil {
		if tgtReal, err2 := filepath.EvalSymlinks(target); err2 == nil && curReal == tgtReal {
			_ = system.EnsureSymlink(target, paths.SymlinkPath())
			system.FirewallAllow(target)
			return target, nil
		}
	}
	_, err = system.InstallBinaryAtomic(exe, target, paths.SymlinkPath())
	return target, err
}

func detectRepoDir() string {
	if v := os.Getenv("FLEET_REPO"); v != "" {
		return v
	}
	if cwd, err := os.Getwd(); err == nil {
		if _, err := os.Stat(filepath.Join(cwd, "go.mod")); err == nil {
			return cwd
		}
	}
	return ""
}

// subcommands -----------------------------------------------------------------

func generateTempHostKeypair() (string, string, error) {
	tmp, err := os.MkdirTemp("", "fleet-init-*")
	if err != nil {
		return "", "", err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	keyFile := filepath.Join(tmp, "host.key")
	pubFile := filepath.Join(tmp, "host.pub")
	if err := pki.Keygen(keyFile, pubFile); err != nil {
		return "", "", err
	}
	keyBytes, err := os.ReadFile(keyFile)
	if err != nil {
		return "", "", err
	}
	pubBytes, err := os.ReadFile(pubFile)
	if err != nil {
		return "", "", err
	}
	return string(keyBytes), string(pubBytes), nil
}

func activateNodeRuntime(cio *IO, owner, root, syncWorkDir string, serve bool) (string, error) {
	if serve {
		cio.info("installing binary & launchd services (fleet serve, fleet sync) ...")
	}
	instBin, err := ensureInstalledSelf()
	if err != nil {
		return "", err
	}
	var syncWatch []string
	if serve {
		if err := system.InstallServeDaemon(instBin, owner, root, syncWorkDir); err != nil {
			return "", err
		}
		if root != "" {
			syncWatch = append(syncWatch, filepath.Join(root, core.NebulaRestartReqFile))
		}
	}
	if err := system.InstallSyncDaemon(instBin, syncWorkDir, false, syncWatch...); err != nil {
		return "", err
	}
	if serve {
		cio.info("starting nebula ...")
	}
	if err := system.StartNebula(); err != nil {
		return "", err
	}
	if mcpUpdated, err := mcpinstall.Install(mcpinstall.InstallOpts{}); err == nil && len(mcpUpdated) > 0 {
		cio.info(fmt.Sprintf("configured agent skills & hooks (%d file(s))", len(mcpUpdated)))
	}
	if _, rErr := installAgentRelayLaunchAgent(paths.LauncherPath(), mcpinstall.DetectHarnesses("", false)); rErr != nil {
		cio.printf("warning: agent relay LaunchAgent: %v\n", rErr)
	}
	return instBin, nil
}

func finishInit(cio *IO, fleet *core.Fleet, keyStr, root, host string, port int, dnsCfg *ddns.Route53) error {
	mini, err := fleet.Registry.Lighthouse()
	if err != nil || mini == nil {
		return fmt.Errorf("lighthouse missing from registry after init")
	}
	cfgPtr, err := fleet.ConfigFor(mini)
	if err != nil || cfgPtr == nil {
		return err
	}
	caPEM, err := fleet.CAPem()
	if err != nil {
		return err
	}
	owner := currentUsername()
	repo := detectRepoDir()
	if err := installNodeFiles(&keyStr, caPEM, mini.Cert, *cfgPtr, client.NodeConfig{
		Name:     mini.Name,
		API:      fmt.Sprintf("http://127.0.0.1:%d", paths.APIPort()),
		Owner:    owner,
		Repo:     repo,
		StateDir: root,
	}); err != nil {
		return err
	}
	workDir := cmp.Or(repo, root)
	instBin, err := activateNodeRuntime(cio, owner, root, workDir, true)
	if err != nil {
		return err
	}
	if dnsCfg != nil {
		if err := ddnsActivate(cio, fleet, *dnsCfg, instBin, root, workDir); err != nil {
			cio.printf("warning: DNS not set up: %v\n         fix it, then run: fleet ddns setup --zone-id %s\n", err, dnsCfg.ZoneID)
		}
	}
	caID, _ := fleet.CAID()
	cio.printf(`
Lighthouse ready.
  CA id:        %s   (join codes show this id; check that it matches)
  Public entry: %s:%d/udp  -> forward this UDP port on the router to this Mac
  Back up:      %s/ca.key and the passphrase (e.g. 1Password)

Next, register your laptop. On the laptop:   fleet join laptop
                       Then on this Mac:     fleet approve --group admin
`, caID, host, port, root)
	return nil
}

func cmdInit(args []string, cio *IO) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	host := fs.String("host", "", "public DDNS name of the home connection")
	port := fs.Int("port", 443, "UDP port forwarded to this Mac (default 443)")
	lanIP := fs.String("lan-ip", "", "this Mac's reserved LAN IP")
	zoneID := fs.String("route53-zone", "", "keep --host pointed at this network via Route 53")
	awsProf := fs.String("aws-profile", "fleet-ddns", "AWS profile for DNS updates")
	pos, err := parseFlagsInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 || *host == "" {
		return fmt.Errorf("usage: fleet init --host HOST [--port 443] [--lan-ip IP] [--route53-zone ZONE_ID]")
	}
	if err := requireNotRoot("init"); err != nil {
		return err
	}
	root := paths.StateDir()
	if _, err := os.Stat(filepath.Join(root, "fleet.db")); err == nil {
		return fmt.Errorf("%s/fleet.db exists: this machine is already the lighthouse", root)
	}
	if err := system.EnsureNebula(); err != nil {
		return err
	}
	var dnsCfg *ddns.Route53
	if *zoneID != "" {
		cfg, err := ddnsPreflight(*host, *zoneID, *awsProf)
		if err != nil {
			return err
		}
		dnsCfg = &cfg
	}

	cio.info("choose the CA passphrase. You'll type it whenever you approve a device. Store it in your password manager.")
	passphrase, err := cio.askPassphrase(true)
	if err != nil {
		return err
	}
	keyStr, pubStr, err := generateTempHostKeypair()
	if err != nil {
		return err
	}
	fleet, err := core.Initialize(core.InitOpts{
		Root:             root,
		Host:             *host,
		Port:             *port,
		LanIP:            *lanIP,
		Passphrase:       passphrase,
		LighthousePubkey: pubStr,
	})
	if err != nil {
		return err
	}
	defer func() { _ = fleet.Close() }()

	cio.info("CA, registry and lighthouse certificate created in " + root)
	return finishInit(cio, fleet, keyStr, root, *host, *port, dnsCfg)
}

func writePendingJoinRequest(name string, cio *IO) (string, string, string, error) {
	pend := paths.PendingDir()
	if err := os.MkdirAll(pend, 0o700); err != nil {
		return "", "", "", err
	}
	keyPath := filepath.Join(pend, name+".key")
	pubPath := filepath.Join(pend, name+".pub")
	if _, err := os.Stat(keyPath); os.IsNotExist(err) {
		if err := pki.Keygen(keyPath, pubPath); err != nil {
			return "", "", "", err
		}
		_ = os.Chmod(keyPath, 0o600)
	}
	pubRaw, err := os.ReadFile(pubPath)
	if err != nil {
		return "", "", "", err
	}
	pubkey, err := pki.NormalizePubkey(string(pubRaw))
	if err != nil {
		return "", "", "", err
	}
	reqBlock, err := codes.Encode(codes.Request, map[string]any{"v": 1, "name": name, "pubkey": pubkey})
	if err != nil {
		return "", "", "", err
	}
	cio.printf("\n%s\n\n", reqBlock)
	cio.info("request id: " + codes.ShortID(pubkey))
	cio.printf("    On an admin device (or the lighthouse) run `fleet approve` and paste the block above.\n")
	cio.printf("    Re-running `fleet join` prints the same request until it's approved.\n\n")
	return keyPath, pubPath, pubkey, nil
}

func finishJoin(cio *IO, keyPath, pubPath, pubkey string) error {
	pasted, err := cio.paste("Paste the FLEET JOIN RESPONSE block:", "END "+codes.Response)
	if err != nil {
		return err
	}
	var resp struct {
		Name   string `json:"name"`
		IP     string `json:"ip"`
		Kind   string `json:"kind"`
		CA     string `json:"ca"`
		Cert   string `json:"cert"`
		Config string `json:"config"`
		API    string `json:"api"`
		CAID   string `json:"ca_id"`
	}
	if err := codes.Decode(codes.Response, pasted, &resp); err != nil {
		return err
	}
	if resp.Kind != "mac" {
		return fmt.Errorf("that response is for a mobile device")
	}
	certInfo, err := pki.Info(resp.Cert)
	if err != nil {
		return err
	}
	pubHex, err := pki.PubkeyHex(pubkey)
	if err != nil {
		return err
	}
	if certInfo.PublicKey != pubHex {
		return fmt.Errorf("that response is for a different request (public key mismatch)")
	}
	if err := pki.Verify(resp.CA, resp.Cert); err != nil {
		return err
	}
	cio.info(fmt.Sprintf("approved as %s (%s), CA id %s", resp.Name, resp.IP, resp.CAID))

	keyBytes, err := os.ReadFile(keyPath)
	if err != nil {
		return err
	}
	keyStr := string(keyBytes)
	if err := installNodeFiles(&keyStr, resp.CA, resp.Cert, resp.Config, client.NodeConfig{
		Name:  resp.Name,
		API:   resp.API,
		Owner: currentUsername(),
	}); err != nil {
		return err
	}
	if _, err := activateNodeRuntime(cio, "", "", "", false); err != nil {
		return err
	}
	_ = os.Remove(keyPath)
	_ = os.Remove(pubPath)
	cio.checkLighthouse()
	cio.printf("\nJoined. Config and revocations now sync automatically every 5 minutes.\n")
	return nil
}

func cmdJoin(args []string, cio *IO) error {
	fs := flag.NewFlagSet("join", flag.ContinueOnError)
	force := fs.Bool("force", false, "re-join even if already joined")
	pos, err := parseFlagsInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("usage: fleet join NAME [--force]")
	}
	name := pos[0]
	if err := requireNotRoot("join"); err != nil {
		return err
	}
	if _, err := os.Stat(paths.NodeFile()); err == nil && !*force {
		if n, err := client.LoadNode(); err == nil {
			return fmt.Errorf("already joined as %s (use --force to re-join)", n.Name)
		}
		return fmt.Errorf("already joined (use --force to re-join)")
	}
	if err := system.EnsureNebula(); err != nil {
		return err
	}
	keyPath, pubPath, pubkey, err := writePendingJoinRequest(name, cio)
	if err != nil {
		return err
	}
	return finishJoin(cio, keyPath, pubPath, pubkey)
}

func cmdApprove(args []string, cio *IO) error {
	fs := flag.NewFlagSet("approve", flag.ContinueOnError)
	group := fs.String("group", "managed", "device group (admin|managed)")
	nameOverride := fs.String("name", "", "override the requested name")
	replace := fs.Bool("replace", false, "re-key an existing device, keeping its IP")
	pos, err := parseFlagsInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(pos, " "))
	}
	if !paths.ValidGroup(*group) {
		return fmt.Errorf("--group must be admin or managed")
	}
	pasted, err := cio.paste("Paste the FLEET JOIN REQUEST block:", "END "+codes.Request)
	if err != nil {
		return err
	}
	var req struct {
		Name   string `json:"name"`
		Pubkey string `json:"pubkey"`
	}
	if err := codes.Decode(codes.Request, pasted, &req); err != nil {
		return err
	}
	name := req.Name
	if *nameOverride != "" {
		name = *nameOverride
	}
	repNote := ""
	if *replace {
		repNote = "  (replacing existing cert)"
	}
	cio.printf("\n  device:     %s\n  request id: %s  (must match the joining machine)\n", name, codes.ShortID(req.Pubkey))
	cio.printf("  group:      %s%s\n", *group, repNote)
	if err := cio.confirm("Approve?"); err != nil {
		return err
	}
	var resp core.ApproveResult
	if err := withPassphrase(cio, "/v1/approve", map[string]any{
		"name":    name,
		"pubkey":  req.Pubkey,
		"kind":    "mac",
		"group":   *group,
		"replace": *replace,
	}, 3, &resp); err != nil {
		return err
	}
	block, err := codes.Encode(codes.Response, map[string]any{
		"name":   resp.Name,
		"ip":     resp.IP,
		"kind":   resp.Kind,
		"ca":     resp.CA,
		"cert":   resp.Cert,
		"config": resp.Config,
		"api":    resp.API,
		"ca_id":  resp.CAID,
	})
	if err != nil {
		return err
	}
	cio.printf("\n%s\n\n", block)
	cio.info(fmt.Sprintf("%s -> %s. Paste the block above into the waiting `fleet join`.", name, resp.IP))
	return nil
}

func cmdApproveMobile(args []string, cio *IO) error {
	fs := flag.NewFlagSet("approve-mobile", flag.ContinueOnError)
	group := fs.String("group", "admin", "device group (admin|managed)")
	replace := fs.Bool("replace", false, "re-key existing mobile device")
	pos, err := parseFlagsInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("usage: fleet approve-mobile NAME [--group admin|managed] [--replace]")
	}
	if !paths.ValidGroup(*group) {
		return fmt.Errorf("--group must be admin or managed")
	}
	name := pos[0]
	pem, err := cio.paste("Paste the public key from the Mobile Nebula app:", "END NEBULA X25519 PUBLIC KEY")
	if err != nil {
		return err
	}
	pubkey, err := pki.NormalizePubkey(pem)
	if err != nil {
		return err
	}
	cio.printf("\n  device: %s (mobile)\n  group:  %s\n  key id: %s\n", name, *group, codes.ShortID(pubkey))
	if err := cio.confirm("Approve?"); err != nil {
		return err
	}
	var resp core.ApproveResult
	if err := withPassphrase(cio, "/v1/approve", map[string]any{
		"name":    name,
		"pubkey":  pubkey,
		"kind":    "mobile",
		"group":   *group,
		"replace": *replace,
	}, 3, &resp); err != nil {
		return err
	}
	printMobile(cio, resp)
	return nil
}

func cmdLS(args []string, cio *IO) error {
	if len(args) > 0 {
		return fmt.Errorf("usage: fleet ls")
	}
	api, err := client.ResolveAPI("")
	if err != nil {
		return err
	}
	var data struct {
		Devices []core.DeviceItem `json:"devices"`
		CAID    string            `json:"ca_id"`
	}
	if err := client.Call(api, "GET", "/v1/devices", nil, 30*time.Second, nil, &data); err != nil {
		return err
	}
	rows := [][]string{{"NAME", "IP", "KIND", "GROUP", "LAST SEEN", "EXPIRES", "CLIENT", "STATUS"}}
	for _, d := range data.Devices {
		status := "active"
		if d.RevokedAt != nil && *d.RevokedAt != "" {
			status = "revoked"
		}
		clientVer := "-"
		if d.ClientVersion != nil && *d.ClientVersion != "" {
			clientVer = *d.ClientVersion
		}
		exp := d.NotAfter
		if len(exp) > 10 {
			exp = exp[:10]
		}
		rows = append(rows, []string{
			d.Name, d.IP, d.Kind, d.Group, ago(d.LastSeen), exp, clientVer, status,
		})
	}
	printTable(cio.Stdout, rows)
	cio.printf("\nCA id %s\n", data.CAID)
	return nil
}

func cmdRevoke(args []string, cio *IO) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: fleet revoke NAME")
	}
	name := args[0]
	api, err := client.ResolveAPI("")
	if err != nil {
		return err
	}
	if err := cio.confirm(fmt.Sprintf("Revoke %s? It loses access everywhere within ~5 minutes.", name)); err != nil {
		return err
	}
	if err := client.Call(api, "POST", "/v1/revoke", map[string]string{"name": name}, 30*time.Second, nil, nil); err != nil {
		return err
	}
	cio.info(fmt.Sprintf("revoked %s. Also remove its SSH key from authorized_keys on machines it could reach.", name))
	return nil
}

func cmdSet(args []string, cio *IO) error {
	fs := flag.NewFlagSet("set", flag.ContinueOnError)
	group := fs.String("group", "", "new group (admin|managed)")
	pos, err := parseFlagsInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || !paths.ValidGroup(*group) {
		return fmt.Errorf("usage: fleet set NAME --group admin|managed")
	}
	name := pos[0]
	var resp core.ApproveResult
	if err := withPassphrase(cio, "/v1/set-group", map[string]any{
		"name":  name,
		"group": *group,
	}, 3, &resp); err != nil {
		return err
	}
	if resp.Kind == "mobile" {
		printMobile(cio, resp)
		cio.info("replace the certificate in the app; the old one is now revoked")
	} else {
		cio.info(fmt.Sprintf("%s is now %s; it picks up its new certificate on its next sync (<= 5 min)", name, *group))
	}
	return nil
}

func persistUpdatedNodeIfNeeded(node client.NodeConfig) {
	b, err := json.MarshalIndent(node, "", "  ")
	if err != nil {
		return
	}
	_, _ = system.WriteRootFile(paths.NodeFile(), string(b)+"\n", 0o644)
}

func backfillSyncNode(node client.NodeConfig, isLighthouse bool) client.NodeConfig {
	dirty := false
	if node.Repo == "" && isLighthouse {
		if r := detectRepoDir(); r != "" {
			node.Repo = r
			dirty = true
		}
	}
	if node.Owner == "" && node.Repo != "" {
		if o, err := system.PathOwner(node.Repo); err == nil && o != "root" {
			node.Owner = o
			dirty = true
		}
	}
	if node.StateDir == "" && isLighthouse && node.Owner != "" {
		node.StateDir = paths.StateDirForOwner(node.Owner)
		dirty = true
	}
	if dirty {
		persistUpdatedNodeIfNeeded(node)
	}
	return node
}

func reexecSyncUpdated(cio *IO, stamp, syncWorkDir string, watchPaths ...string) {
	targetBin := paths.InstalledBinaryPath()
	_ = system.InstallSyncDaemon(targetBin, syncWorkDir, true, watchPaths...)
	if err := syscall.Exec(targetBin, []string{targetBin, "sync", "--no-update"}, os.Environ()); err != nil {
		cio.printf("%s re-exec warning: %v\n", stamp, err)
	}
}

func waitSyncPayload(cio *IO, api, stamp string, waitSec int) (core.SyncResult, bool, error) {
	syncPath := fmt.Sprintf("/v1/sync?os=%s&arch=%s", runtime.GOOS, runtime.GOARCH)
	deadline := time.Now().Add(time.Duration(waitSec) * time.Second)
	var payload core.SyncResult
	for {
		err := client.Call(api, "GET", syncPath, nil, 15*time.Second, nil, &payload)
		if err == nil {
			return payload, true, nil
		}
		if apiErr, ok := errors.AsType[*client.APIError](err); ok && apiErr.StatusCode == 403 {
			return core.SyncResult{}, false, fmt.Errorf("the lighthouse refused this device: %w", err)
		}
		if waitSec <= 0 || !time.Now().Before(deadline) {
			cio.printf("%s lighthouse unreachable, keeping current config: %v\n", stamp, err)
			return core.SyncResult{}, false, nil
		}
		time.Sleep(1 * time.Second)
	}
}

func applySyncedFiles(cio *IO, stamp string, payload core.SyncResult) error {
	etc := paths.NebulaEtc()
	var oldCfg string
	if raw, err := os.ReadFile(filepath.Join(etc, "config.yml")); err == nil {
		oldCfg = string(raw)
	}
	var changed []string
	cfgChanged := false
	cfgContent := ""
	if payload.Config != nil {
		cfgContent = *payload.Config
	}
	for _, f := range []struct {
		name    string
		content string
	}{
		{"ca.crt", payload.CA},
		{"host.crt", payload.Cert},
		{"config.yml", cfgContent},
	} {
		if f.content == "" && f.name == "config.yml" {
			continue
		}
		wrote, err := system.WriteRootFile(filepath.Join(etc, f.name), f.content, 0o644)
		if err != nil {
			return err
		}
		if wrote {
			changed = append(changed, f.name)
			if f.name == "config.yml" {
				cfgChanged = true
			}
		}
	}
	if neb, err := exec.LookPath("nebula"); err == nil {
		system.FirewallAllow(neb) // brew upgrades move Cellar path
	}
	actionNote := ""
	if label := system.NebulaLabel(""); len(changed) > 0 && label != "" && system.LaunchdLoaded(label) {
		needRestart := cfgChanged && render.ConfigChangeRequiresRestart(oldCfg, cfgContent)
		if !needRestart {
			if err := system.Reload(label); err == nil {
				actionNote = " (reloaded nebula)"
			} else {
				system.Restart(label)
				actionNote = " (restarted nebula)"
			}
		} else {
			system.Restart(label)
			actionNote = " (restarted nebula)"
		}
	}
	if len(changed) > 0 {
		cio.printf("%s synced %s: updated %s%s\n", stamp, payload.Name, strings.Join(changed, ", "), actionNote)
	} else {
		cio.printf("%s synced %s: no changes\n", stamp, payload.Name)
	}
	return nil
}

func handlePendingNebulaRestart(cio *IO, stamp string, node client.NodeConfig) bool {
	if node.StateDir == "" {
		return false
	}
	req, err := core.PendingNebulaRestart(node.StateDir)
	if err != nil {
		cio.printf("%s nebula restart check warning: %v\n", stamp, err)
		return false
	}
	if req == nil {
		return false
	}
	label := system.NebulaLabel("")
	var rErr error
	if label == "" || !system.LaunchdLoaded(label) {
		rErr = fmt.Errorf("nebula launchd service is not loaded")
	} else {
		rErr = system.RestartService(label)
	}
	st := core.NebulaRestartStatus{
		HandledID:   req.ID,
		RestartedAt: registry.NowISO(),
		Reason:      req.Reason,
		By:          req.By,
		OK:          rErr == nil,
	}
	if rErr != nil {
		st.Error = rErr.Error()
		cio.printf("%s nebula restart failed: %v\n", stamp, rErr)
	} else {
		cio.printf("%s restarted %s (reason: %s, by: %s)\n", stamp, label, req.Reason, req.By)
	}
	uid, gid := system.OwnerIDs(node.Owner)
	if recErr := core.RecordNebulaRestartStatus(node.StateDir, st, uid, gid); recErr != nil {
		cio.printf("%s nebula restart status warning: %v\n", stamp, recErr)
	}
	return true
}

func harvestSyncedOwnerMemories(cio *IO, stamp string, node client.NodeConfig) {
	if node.Owner == "" {
		return
	}
	if hRep, hErr := memory.HarvestForOwner(node.Owner, node.API); hErr != nil {
		cio.printf("%s memory harvest warning: %v\n", stamp, hErr)
	} else if hRep.Created+hRep.Updated > 0 {
		cio.printf("%s harvested memories: +%d created, ~%d updated\n", stamp, hRep.Created, hRep.Updated)
	}
}

func warnStaleMCPServers(cio *IO, prefix, binPath string) {
	for _, s := range mcpinstall.FindStaleMCPServers(binPath) {
		cio.printf("%s: %s\n", prefix, mcpinstall.FormatStaleWarning(s))
	}
}

func syncOwnerPre(cio *IO, stamp string, node client.NodeConfig) {
	if node.Owner == "" {
		return
	}
	if mcpUpdated, err := mcpinstall.SyncOwnerConfigs(node.Owner); err == nil && len(mcpUpdated) > 0 {
		names := make([]string, len(mcpUpdated))
		for i, p := range mcpUpdated {
			names[i] = filepath.Base(p)
		}
		cio.printf("%s updated MCP/hook configs: %s\n", stamp, strings.Join(names, ", "))
	}
}

func syncOwnerPost(cio *IO, stamp string, node client.NodeConfig) {
	if node.Owner == "" {
		return
	}
	for _, item := range []struct {
		label string
		name  string
	}{
		{paths.SlackerWorkerLabel, "slacker-worker"},
		{paths.AgentRelayLabel, "agent-relay"},
	} {
		if restarted, rErr := system.RestartStaleUserAgent(node.Owner, item.label, paths.InstalledBinaryPath()); rErr != nil {
			cio.printf("%s %s restart warning: %v\n", stamp, item.name, rErr)
		} else if restarted {
			cio.printf("%s restarted %s (binary updated)\n", stamp, item.label)
		}
	}
	if ownerHome := paths.HomeForUser(node.Owner); ownerHome != "" {
		syncItems := usage.CollectAll(context.Background(), usage.CollectOpts{
			Home:      ownerHome,
			Device:    node.Name,
			SkipCodex: true,
		})
		_ = usage.ReportToLighthouse(node.API, syncItems)
	}
}

func syncMacResolverForPayload(cio *IO, stamp string, payload core.SyncResult) {
	if resChanged, resAction, rErr := system.SyncMacResolver(paths.ResolverDir, payload.DNSDomain, paths.LighthouseIP); rErr != nil {
		cio.printf("%s resolver sync warning: %v\n", stamp, rErr)
	} else if resChanged && resAction != "" {
		cio.printf("%s updated %s\n", stamp, resAction)
	}
}

func cmdSync(args []string, cio *IO) error {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	noUpdate := fs.Bool("no-update", false, "skip binary/git self-update")
	waitSec := fs.Int("wait", 0, "retry until the lighthouse answers")
	rebuild := fs.Bool("rebuild", false, "(lighthouse) force rebuild & republish binaries")
	pos, err := parseFlagsInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(pos, " "))
	}
	if !system.IsRoot() {
		return fmt.Errorf("fleet sync runs as root (launchd does this every 5 minutes; or: sudo fleet sync)")
	}
	node, err := client.LoadNode()
	if err != nil {
		return err
	}
	stamp := time.Now().Format("2006-01-02T15:04:05")
	isLighthouse := strings.HasPrefix(node.API, "http://127.0.0.1")
	node = backfillSyncNode(node, isLighthouse)

	if isLighthouse && handlePendingNebulaRestart(cio, stamp, node) {
		return nil
	}

	var lhWatch []string
	if isLighthouse && node.StateDir != "" {
		lhWatch = append(lhWatch, filepath.Join(node.StateDir, core.NebulaRestartReqFile))
	}

	if !*noUpdate && isLighthouse && node.Repo != "" {
		updated, err := system.SelfUpdateLighthouse(system.LighthouseUpdateOpts{
			RepoDir:     node.Repo,
			StateDir:    node.StateDir,
			Owner:       node.Owner,
			TargetBin:   paths.InstalledBinaryPath(),
			SymlinkPath: paths.SymlinkPath(),
			LocalAPI:    node.API,
			Rebuild:     *rebuild,
		})
		if err != nil {
			cio.printf("%s lighthouse build/update warning: %v\n", stamp, err)
		} else if updated {
			cio.printf("%s code updated and binaries published\n", stamp)
			reexecSyncUpdated(cio, stamp, node.Repo, lhWatch...)
		}
	}

	syncOwnerPre(cio, stamp, node)

	payload, ok, err := waitSyncPayload(cio, node.API, stamp, *waitSec)
	if err != nil || !ok {
		return err
	}

	if !*noUpdate && !isLighthouse && payload.BinarySHA256 != "" {
		updated, err := system.SelfUpdateClient(system.ClientUpdateOpts{
			APIURL:      node.API,
			ExpectedSHA: payload.BinarySHA256,
			TargetBin:   paths.InstalledBinaryPath(),
			SymlinkPath: paths.SymlinkPath(),
		})
		if err != nil {
			cio.printf("%s binary update warning: %v\n", stamp, err)
		} else if updated {
			cio.printf("%s updated binary to %s\n", stamp, payload.BinaryVersion)
			reexecSyncUpdated(cio, stamp, "")
		}
	}

	if err := applySyncedFiles(cio, stamp, payload); err != nil {
		return err
	}
	syncMacResolverForPayload(cio, stamp, payload)
	harvestSyncedOwnerMemories(cio, stamp, node)
	syncOwnerPost(cio, stamp, node)
	warnStaleMCPServers(cio, stamp+" warning", paths.InstalledBinaryPath())
	return nil
}

func cmdStatus(args []string, cio *IO) error {
	if len(args) > 0 {
		return fmt.Errorf("usage: fleet status")
	}
	node, err := client.LoadNode()
	if err != nil {
		return err
	}
	cio.printf("device:      %s\n", node.Name)
	cio.printf("api:         %s\n", node.API)
	cio.printf("version:     %s\n", version.String())
	nebLabel := system.NebulaLabel("")
	if nebLabel == "" {
		nebLabel = "nebula (not installed)"
	}
	isLighthouse := strings.HasPrefix(node.API, "http://127.0.0.1")
	for _, label := range []string{nebLabel, paths.SyncLabel, paths.ServeLabel, paths.DDNSLabel} {
		if (label == paths.ServeLabel || label == paths.DDNSLabel) && !isLighthouse {
			continue
		}
		st := "NOT loaded"
		if system.LaunchdLoaded(label) {
			st = "loaded"
		}
		cio.printf("%-44s %s\n", label+":", st)
	}
	if isLighthouse {
		for _, label := range []string{paths.SlackerLabel, paths.EdgeLabel} {
			if _, err := os.Stat(filepath.Join(system.LaunchDaemonsDir, label+".plist")); err == nil {
				st := "NOT loaded"
				if system.LaunchdLoaded(label) {
					st = "loaded"
				}
				cio.printf("%-44s %s\n", label+":", st)
			}
		}
	}
	if _, err := os.Stat(system.LaunchAgentPath(paths.SlackerWorkerLabel)); err == nil {
		st := "NOT loaded"
		if system.UserAgentLoaded(paths.SlackerWorkerLabel) {
			st = "loaded"
		}
		cio.printf("%-44s %s\n", paths.SlackerWorkerLabel+":", st)
	}
	if _, err := os.Stat(system.LaunchAgentPath(paths.AgentRelayLabel)); err == nil {
		st := "NOT loaded"
		if system.UserAgentLoaded(paths.AgentRelayLabel) {
			st = "loaded"
		}
		cio.printf("%-44s %s\n", paths.AgentRelayLabel+":", st)
	}
	var syncRes core.SyncResult
	if err := client.Call(node.API, "GET", "/v1/sync", nil, 10*time.Second, nil, &syncRes); err != nil {
		cio.printf("lighthouse:  %v\n", err)
	} else {
		cio.printf("lighthouse:  reachable, this device is %s\n", syncRes.IP)
		if syncRes.DNSDomain != "" {
			cio.printf("https:       https://%s/\n", syncRes.DNSDomain)
		}
	}
	warnStaleMCPServers(cio, "mcp warning", "")
	return nil
}

// agents & mcp ----------------------------------------------------------------

func installAgentRelayLaunchAgent(binPath string, harnesses []string) (bool, error) {
	if !slices.Contains(harnesses, "claude") && !slices.Contains(harnesses, "codex") {
		return false, nil
	}
	home, _ := os.UserHomeDir()
	err := system.InstallUserAgent(system.AgentOpts{
		Label:     paths.AgentRelayLabel,
		Args:      []string{binPath, "agents", "relay", "--harness", "claude,codex"},
		LogPath:   filepath.Join(paths.AgentRelayStateDir(), "relay.log"),
		WorkDir:   home,
		KeepAlive: true,
	})
	return err == nil, err
}

func cmdMCPInstall(args []string, cio *IO) error {
	fs := flag.NewFlagSet("mcp install", flag.ContinueOnError)
	all := fs.Bool("all", false, "write configs for all harnesses")
	if _, err := parseFlagsInterspersed(fs, args); err != nil {
		return err
	}
	if err := requireNotRoot("mcp install"); err != nil {
		return err
	}
	updated, err := mcpinstall.Install(mcpinstall.InstallOpts{ForceAll: *all})
	if err != nil {
		return err
	}
	for _, p := range updated {
		cio.printf("updated %s\n", p)
	}
	harnesses := mcpinstall.DetectHarnesses("", *all)
	if installed, rErr := installAgentRelayLaunchAgent(paths.LauncherPath(), harnesses); rErr != nil {
		return rErr
	} else if installed {
		cio.printf("installed %s (claude,codex relay)\n", paths.AgentRelayLabel)
	} else if len(updated) == 0 {
		cio.printf("already up to date\n")
	}
	return nil
}

func cmdMCPUninstall(cio *IO) error {
	if err := requireNotRoot("mcp uninstall"); err != nil {
		return err
	}
	removed, err := mcpinstall.Uninstall("")
	if err != nil {
		return err
	}
	relayPlist := system.LaunchAgentPath(paths.AgentRelayLabel)
	_, statErr := os.Stat(relayPlist)
	if err := system.UninstallUserAgent(paths.AgentRelayLabel); err != nil {
		return err
	}
	if statErr == nil {
		removed = append(removed, relayPlist)
	}
	if len(removed) == 0 {
		cio.printf("no fleet skills/hook entries found\n")
		return nil
	}
	for _, p := range removed {
		cio.printf("updated %s\n", p)
	}
	return nil
}

func cmdMCPStatus(cio *IO) error {
	st := mcpinstall.Status("")
	staleByHarness := map[string][]mcpinstall.StaleMCPServer{}
	for _, s := range mcpinstall.FindStaleMCPServers("") {
		staleByHarness[s.Harness] = append(staleByHarness[s.Harness], s)
	}
	for _, h := range paths.Harnesses {
		val := "not configured"
		if st[h] {
			val = "configured"
		}
		cio.printf("%-14s %s\n", h+":", val)
		for _, s := range staleByHarness[h] {
			cio.printf("  warning: %s\n", mcpinstall.FormatStaleWarning(s))
		}
	}
	if st["claude"] || st["codex"] || system.UserAgentLoaded(paths.AgentRelayLabel) {
		rState := "NOT loaded"
		if system.UserAgentLoaded(paths.AgentRelayLabel) {
			rState = "loaded"
		}
		cio.printf("%-14s %s (%s)\n", "agent-relay:", rState, paths.AgentRelayLabel)
	}
	return nil
}

func cmdMCPServe(args []string, cio *IO) error {
	fs := flag.NewFlagSet("mcp serve", flag.ContinueOnError)
	harness := fs.String("harness", "", "harness name")
	if _, err := parseFlagsInterspersed(fs, args); err != nil {
		return err
	}
	if !paths.ValidHarness(*harness) {
		return fmt.Errorf("usage: fleet mcp serve --harness claude|codex|antigravity")
	}
	_, err := mcp.ServeStdio(mcp.ServeOpts{
		Harness: *harness,
		Stdin:   cio.Stdin,
		Stdout:  cio.Stdout,
	})
	return err
}

func cmdMCP(args []string, cio *IO) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: fleet mcp install|uninstall|status|serve")
	}
	switch args[0] {
	case "install":
		return cmdMCPInstall(args[1:], cio)
	case "uninstall":
		return cmdMCPUninstall(cio)
	case "status":
		return cmdMCPStatus(cio)
	case "serve":
		return cmdMCPServe(args[1:], cio)
	default:
		return fmt.Errorf("unknown mcp subcommand %q", args[0])
	}
}

func cmdAgentHook(args []string, cio *IO) error {
	fs := flag.NewFlagSet("agent-hook", flag.ContinueOnError)
	harness := fs.String("harness", "", "harness name")
	if _, err := parseFlagsInterspersed(fs, args); err != nil {
		return err
	}
	if !paths.ValidHarness(*harness) {
		return fmt.Errorf("usage: fleet agent-hook --harness claude|codex|antigravity")
	}
	_, err := hooks.RunHook(*harness, cio.Stdin, cio.Stdout, "")
	return err
}

// ddns ------------------------------------------------------------------------

func ddnsPreflight(name, zoneID, profile string) (ddns.Route53, error) {
	zid, err := ddns.NormalizeZoneID(zoneID)
	if err != nil {
		return ddns.Route53{}, err
	}
	if _, err := exec.LookPath("aws"); err != nil {
		return ddns.Route53{}, fmt.Errorf("aws CLI not found: brew install awscli")
	}
	out, _ := exec.Command("aws", "configure", "list-profiles").Output()
	profiles := strings.Fields(string(out))
	found := slices.Contains(profiles, profile)
	if !found {
		return ddns.Route53{}, fmt.Errorf(
			"no AWS profile %q. Create an IAM user whose only policy is the output of\n  fleet ddns policy --zone-id %s --name %s\nthen store its access key with: aws configure --profile %s",
			profile, zid, name, profile,
		)
	}
	ns, err := ddns.FindNameservers(name, nil)
	if err != nil {
		return ddns.Route53{}, err
	}
	return ddns.Route53{
		Name:        strings.TrimRight(name, "."),
		ZoneID:      zid,
		Profile:     profile,
		Nameservers: ns,
	}, nil
}

func ddnsActivate(cio *IO, fleet *core.Fleet, cfg ddns.Route53, binPath, stateDir, workDir string) error {
	before, ip, changed, err := ddns.Sync(cfg, nil, nil, "", nil)
	if err != nil {
		return err
	}
	if err := fleet.SetDDNS(cfg); err != nil {
		return err
	}
	if err := system.InstallDDNSDaemon(binPath, currentUsername(), stateDir, workDir); err != nil {
		return err
	}
	state := fmt.Sprintf("already %s", ip)
	if changed {
		prev := "none"
		if before != nil {
			prev = *before
		}
		state = fmt.Sprintf("updated %s -> %s", prev, ip)
	}
	cio.info(fmt.Sprintf("DNS %s: %s; re-checked every 5 minutes", cfg.Name, state))
	return nil
}

func loadDDNSConfig() (ddns.Route53, error) {
	fleet, err := core.Open("")
	if err != nil {
		return ddns.Route53{}, err
	}
	defer func() { _ = fleet.Close() }()
	cfg, err := fleet.DDNSConfig()
	if err != nil {
		return ddns.Route53{}, err
	}
	if cfg == nil {
		return ddns.Route53{}, fmt.Errorf("DNS updates aren't configured (fleet ddns setup)")
	}
	return *cfg, nil
}

func cmdDDNSSetup(args []string, cio *IO) error {
	fs := flag.NewFlagSet("ddns setup", flag.ContinueOnError)
	zoneID := fs.String("zone-id", "", "Route 53 hosted zone id")
	profile := fs.String("profile", "fleet-ddns", "AWS profile")
	nameFlag := fs.String("name", "", "record to manage")
	if _, err := parseFlagsInterspersed(fs, args); err != nil {
		return err
	}
	if *zoneID == "" {
		return fmt.Errorf("usage: fleet ddns setup --zone-id Z... [--profile fleet-ddns] [--name HOST]")
	}
	if err := requireNotRoot("ddns setup"); err != nil {
		return err
	}
	fleet, err := core.Open("")
	if err != nil {
		return err
	}
	defer func() { _ = fleet.Close() }()
	settings, err := fleet.Settings()
	if err != nil {
		return err
	}
	host := settings["lighthouse_host"]
	name := cmp.Or(*nameFlag, host)
	if name == "" {
		return fmt.Errorf("run on the lighthouse after `fleet init`, or pass --name")
	}
	if host != "" && strings.TrimRight(name, ".") != host {
		cio.printf("warning: devices connect to %s, not %s\n", host, name)
	}
	cfg, err := ddnsPreflight(name, *zoneID, *profile)
	if err != nil {
		return err
	}
	instBin, err := ensureInstalledSelf()
	if err != nil {
		return err
	}
	return ddnsActivate(cio, fleet, cfg, instBin, paths.StateDir(), "")
}

func cmdDDNSRun(cio *IO) error {
	cfg, err := loadDDNSConfig()
	if err != nil {
		return err
	}
	stamp := time.Now().Format("2006-01-02T15:04:05")
	before, ip, changed, err := ddns.Sync(cfg, nil, nil, "", nil)
	if err != nil {
		return fmt.Errorf("%s %w", stamp, err)
	}
	if changed {
		prev := "none"
		if before != nil {
			prev = *before
		}
		cio.printf("%s %s: %s -> %s\n", stamp, cfg.Name, prev, ip)
	}
	return nil
}

func cmdDDNSStatus(cio *IO) error {
	cfg, err := loadDDNSConfig()
	if err != nil {
		return err
	}
	pubIP, err := ddns.PublicIPv4(nil)
	if err != nil {
		return err
	}
	cur, err := ddns.CurrentRecord(cfg, nil)
	if err != nil {
		return err
	}
	curStr := "no record"
	if cur != nil {
		curStr = *cur
	}
	ns0 := ""
	if len(cfg.Nameservers) > 0 {
		ns0 = cfg.Nameservers[0]
	}
	cio.printf("record:      %s (zone %s, profile %s)\n", cfg.Name, cfg.ZoneID, cfg.Profile)
	cio.printf("public IP:   %s\n", pubIP)
	cio.printf("DNS answer:  %s  (from %s)\n", curStr, ns0)
	loaded := "NOT loaded"
	if system.LaunchdLoaded(paths.DDNSLabel) {
		loaded = "loaded"
	}
	cio.printf("%-44s %s\n", paths.DDNSLabel+":", loaded)
	return nil
}

func cmdDDNSPolicy(args []string, cio *IO) error {
	fs := flag.NewFlagSet("ddns policy", flag.ContinueOnError)
	zoneID := fs.String("zone-id", "", "Route 53 hosted zone id")
	name := fs.String("name", "", "DNS record name")
	if _, err := parseFlagsInterspersed(fs, args); err != nil {
		return err
	}
	if *zoneID == "" || *name == "" {
		return fmt.Errorf("usage: fleet ddns policy --zone-id Z... --name HOST")
	}
	p, err := ddns.Policy(*zoneID, *name)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(cio.Stdout)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(p)
}

func cmdDDNS(args []string, cio *IO) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: fleet ddns setup|run|status|policy")
	}
	switch args[0] {
	case "setup":
		return cmdDDNSSetup(args[1:], cio)
	case "run":
		return cmdDDNSRun(cio)
	case "status":
		return cmdDDNSStatus(cio)
	case "policy":
		return cmdDDNSPolicy(args[1:], cio)
	default:
		return fmt.Errorf("unknown ddns subcommand %q", args[0])
	}
}

func usageText() string {
	return `usage: fleet <command> [options]

Commands:
  init            (lighthouse, once) create CA + registry, start services
  join            (new Mac) request to join, then paste the approval
  approve         (admin) approve a pasted join request
  approve-mobile  (admin) sign a phone/tablet key from Mobile Nebula
  ls              (admin) list devices
  revoke          (admin) revoke a device
  set             (admin) change a device's group (re-signs its cert)
  sync            (root, launchd) pull config/certs/revocations and binary updates
  serve           (lighthouse, launchd) run the API
  edge            (lighthouse, root launchd) run HTTPS :443 proxy & private mesh DNS :53
  ddns            (lighthouse) keep the public DNS name pointed at home
  tls             (lighthouse/admin) manage Let's Encrypt HTTPS cert & private mesh DNS
  status          show this machine's fleet status
  agents          list, inspect, message, spawn, or kill AI agent sessions across the fleet
  usage           show quota and usage status across claude, codex, and antigravity
  session         manage detached Claude, Antigravity (agy), and Codex sessions
  slacker         lighthouse Slack Socket Mode dispatcher & Mac task workers
  assistant       always-on lighthouse personal assistant, Obsidian skills & approvals
  memory          manage shared fleet agent memory and LLM curation
  mcp             manage skills, lifecycle hooks, and agent relay for claude, codex, and antigravity
  agent-hook      (internal) handle a harness lifecycle hook
  version         print fleet version
`
}

func dispatchCommand(cmd string, rest []string, cio *IO) (bool, error) {
	if cmd == "version" || cmd == "--version" || cmd == "-V" {
		cio.printf("%s\n", version.String())
		return true, nil
	}
	if cmd == "serve" {
		return true, server.Serve(paths.APIPort(), "0.0.0.0")
	}
	handlers := map[string]func([]string, *IO) error{
		"init":           cmdInit,
		"join":           cmdJoin,
		"approve":        cmdApprove,
		"approve-mobile": cmdApproveMobile,
		"ls":             cmdLS,
		"revoke":         cmdRevoke,
		"set":            cmdSet,
		"sync":           cmdSync,
		"status":         cmdStatus,
		"ddns":           cmdDDNS,
		"tls":            cmdTLS,
		"edge":           cmdEdge,
		"agents":         cmdAgents,
		"usage":          cmdUsage,
		"statusline":     cmdStatusLine,
		"session":        cmdSession,
		"slacker":        cmdSlacker,
		"assistant":      cmdAssistant,
		"memory":         cmdMemory,
		"mcp":            cmdMCP,
		"agent-hook":     cmdAgentHook,
	}
	fn, ok := handlers[cmd]
	if !ok {
		return false, nil
	}
	return true, fn(rest, cio)
}

// Run dispatches CLI args using cio and returns an exit code (0 on success).
func Run(args []string, cio *IO) int {
	if cio == nil {
		cio = defaultIO()
	}
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		_, _ = io.WriteString(cio.Stdout, usageText())
		return 0
	}
	ok, err := dispatchCommand(args[0], args[1:], cio)
	if !ok {
		_, _ = fmt.Fprintf(cio.Stderr, "error: unknown command %q\n\n%s", args[0], usageText())
		return 2
	}
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, _ = io.WriteString(cio.Stdout, usageText())
			return 0
		}
		_, _ = fmt.Fprintf(cio.Stderr, "error: %v\n", err)
		return 1
	}
	return 0
}

// Main is the binary entry point returning the process exit code.
func Main(args []string) int {
	return Run(args, defaultIO())
}
