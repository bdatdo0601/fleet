package cli

import (
	"cmp"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"github.com/bdatdo0601/fleet/internal/client"
	"github.com/bdatdo0601/fleet/internal/codes"
	"github.com/bdatdo0601/fleet/internal/core"
	"github.com/bdatdo0601/fleet/internal/ddns"
	"github.com/bdatdo0601/fleet/internal/mcpinstall"
	"github.com/bdatdo0601/fleet/internal/paths"
	"github.com/bdatdo0601/fleet/internal/pki"
	"github.com/bdatdo0601/fleet/internal/system"
)

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
		if apiErr, ok := errors.AsType[*client.APIError](err); ok && apiErr.StatusCode == 401 && attempt < attempts {
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
	if !paths.NamePattern.MatchString(name) {
		return fmt.Errorf("invalid device name %q (must match %s)", name, paths.NamePattern.String())
	}
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
	profile := fs.String("profile", "personal", "device trust profile (personal|work)")
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
	if !paths.ValidProfile(*profile) {
		return fmt.Errorf("--profile must be personal or work")
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
	cio.printf("  group:      %s%s\n  profile:    %s\n", *group, repNote, *profile)
	if err := cio.confirm("Approve?"); err != nil {
		return err
	}
	var resp core.ApproveResult
	if err := withPassphrase(cio, "/v1/approve", map[string]any{
		"name":    name,
		"pubkey":  req.Pubkey,
		"kind":    "mac",
		"group":   *group,
		"profile": *profile,
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
	profile := fs.String("profile", "personal", "device trust profile (personal|work)")
	replace := fs.Bool("replace", false, "re-key existing mobile device")
	pos, err := parseFlagsInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("usage: fleet approve-mobile NAME [--group admin|managed] [--profile personal|work] [--replace]")
	}
	if !paths.ValidGroup(*group) {
		return fmt.Errorf("--group must be admin or managed")
	}
	if !paths.ValidProfile(*profile) {
		return fmt.Errorf("--profile must be personal or work")
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
	cio.printf("\n  device:  %s (mobile)\n  group:   %s\n  profile: %s\n  key id:  %s\n", name, *group, *profile, codes.ShortID(pubkey))
	if err := cio.confirm("Approve?"); err != nil {
		return err
	}
	var resp core.ApproveResult
	if err := withPassphrase(cio, "/v1/approve", map[string]any{
		"name":    name,
		"pubkey":  pubkey,
		"kind":    "mobile",
		"group":   *group,
		"profile": *profile,
		"replace": *replace,
	}, 3, &resp); err != nil {
		return err
	}
	printMobile(cio, resp)
	return nil
}

func cmdLS(args []string, cio *IO) error {
	if err := parseFlagsNoPositional("ls", args); err != nil {
		return err
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
	rows := [][]string{{"NAME", "IP", "KIND", "GROUP", "PROFILE", "LAST SEEN", "EXPIRES", "CLIENT", "STATUS"}}
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
			d.Name, d.IP, d.Kind, d.Group, cmp.Or(d.Profile, "personal"), ago(d.LastSeen), exp, clientVer, status,
		})
	}
	printTable(cio.Stdout, rows)
	cio.printf("\nCA id %s\n", data.CAID)
	return nil
}

func cmdRevoke(args []string, cio *IO) error {
	fs := flag.NewFlagSet("revoke", flag.ContinueOnError)
	pos, err := parseFlagsInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("usage: fleet revoke NAME")
	}
	name := pos[0]
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

func cmdSetProfile(args []string, cio *IO) error {
	fs := flag.NewFlagSet("set-profile", flag.ContinueOnError)
	pos, err := parseFlagsInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 || !paths.ValidProfile(strings.ToLower(pos[1])) {
		return fmt.Errorf("usage: fleet set-profile NAME personal|work")
	}
	name := pos[0]
	profile := strings.ToLower(pos[1])
	api, err := client.ResolveAPI("")
	if err != nil {
		return err
	}
	var resp struct {
		Name    string `json:"name"`
		Profile string `json:"profile"`
	}
	if err := client.Call(api, "POST", "/v1/set-profile", map[string]string{
		"name":    name,
		"profile": profile,
	}, 15*time.Second, nil, &resp); err != nil {
		return err
	}
	cio.info(fmt.Sprintf("%s trust profile set to %s", resp.Name, resp.Profile))
	return nil
}
