package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/bdatdo0601/fleet/internal/client"
	"github.com/bdatdo0601/fleet/internal/core"
	"github.com/bdatdo0601/fleet/internal/hooks"
	"github.com/bdatdo0601/fleet/internal/mcp"
	"github.com/bdatdo0601/fleet/internal/mcpinstall"
	"github.com/bdatdo0601/fleet/internal/memory"
	"github.com/bdatdo0601/fleet/internal/paths"
	"github.com/bdatdo0601/fleet/internal/registry"
	"github.com/bdatdo0601/fleet/internal/render"
	"github.com/bdatdo0601/fleet/internal/skills"
	"github.com/bdatdo0601/fleet/internal/system"
	"github.com/bdatdo0601/fleet/internal/usage"
	"github.com/bdatdo0601/fleet/internal/version"
)

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
			} else if rErr := system.RestartService(label); rErr != nil {
				cio.printf("%s warning: restart %s: %v\n", stamp, label, rErr)
			} else {
				actionNote = " (restarted nebula)"
			}
		} else if rErr := system.RestartService(label); rErr != nil {
			cio.printf("%s warning: restart %s: %v\n", stamp, label, rErr)
		} else {
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

func syncOwnerDynamicSkills(cio *IO, stamp string, node client.NodeConfig, synced []core.SyncedSkill) {
	if node.Owner == "" {
		return
	}
	dyn := make([]skills.Skill, 0, len(synced))
	for _, s := range synced {
		dyn = append(dyn, skills.Skill{Name: s.Name, Content: []byte(s.Content)})
	}
	if changed, err := mcpinstall.SyncDynamicSkills(node.Owner, dyn); err != nil {
		cio.printf("%s synced skills warning: %v\n", stamp, err)
	} else if len(changed) > 0 {
		cio.printf("%s synced %d vault skill update(s)\n", stamp, len(changed))
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
		{paths.SandboxAgentLabel, "sandbox-agent"},
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
	syncOwnerDynamicSkills(cio, stamp, node, payload.SyncedSkills)
	syncMacResolverForPayload(cio, stamp, payload)
	harvestSyncedOwnerMemories(cio, stamp, node)
	syncOwnerPost(cio, stamp, node)
	warnStaleMCPServers(cio, stamp+" warning", paths.InstalledBinaryPath())
	return nil
}

func cmdStatus(args []string, cio *IO) error {
	if err := parseFlagsNoPositional("status", args); err != nil {
		return err
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
	for _, label := range []string{paths.SlackerWorkerLabel, paths.AgentRelayLabel, paths.SandboxAgentLabel} {
		if _, err := os.Stat(system.LaunchAgentPath(label)); err == nil {
			st := "NOT loaded"
			if system.UserAgentLoaded(label) {
				st = "loaded"
			}
			cio.printf("%-44s %s\n", label+":", st)
		}
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
	pos, err := parseFlagsInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(pos, " "))
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

func cmdMCPUninstall(args []string, cio *IO) error {
	if err := parseFlagsNoPositional("mcp uninstall", args); err != nil {
		return err
	}
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

func cmdMCPStatus(args []string, cio *IO) error {
	if err := parseFlagsNoPositional("mcp status", args); err != nil {
		return err
	}
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
	profile := fs.String("profile", "core", "MCP tool profile (core|personal)")
	pos, err := parseFlagsInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(pos, " "))
	}
	if !paths.ValidHarness(*harness) {
		return fmt.Errorf("usage: fleet mcp serve --harness claude|codex|antigravity [--profile core|personal]")
	}
	_, err = mcp.ServeStdio(mcp.ServeOpts{
		Harness: *harness,
		Profile: *profile,
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
		return cmdMCPUninstall(args[1:], cio)
	case "status":
		return cmdMCPStatus(args[1:], cio)
	case "serve":
		return cmdMCPServe(args[1:], cio)
	default:
		return fmt.Errorf("unknown mcp subcommand %q", args[0])
	}
}

func cmdAgentHook(args []string, cio *IO) error {
	fs := flag.NewFlagSet("agent-hook", flag.ContinueOnError)
	harness := fs.String("harness", "", "harness name")
	pos, err := parseFlagsInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(pos, " "))
	}
	if !paths.ValidHarness(*harness) {
		return fmt.Errorf("usage: fleet agent-hook --harness claude|codex|antigravity")
	}
	_, err = hooks.RunHook(*harness, cio.Stdin, cio.Stdout, "")
	return err
}
