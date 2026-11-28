package cli

import (
	"cmp"
	"flag"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/bdatdo0601/fleet/internal/core"
	"github.com/bdatdo0601/fleet/internal/ddns"
	"github.com/bdatdo0601/fleet/internal/paths"
	"github.com/bdatdo0601/fleet/internal/system"
)

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
	pos, err := parseFlagsInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 || *zoneID == "" {
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

func cmdDDNSRun(args []string, cio *IO) error {
	if err := parseFlagsNoPositional("ddns run", args); err != nil {
		return err
	}
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

func cmdDDNSStatus(args []string, cio *IO) error {
	if err := parseFlagsNoPositional("ddns status", args); err != nil {
		return err
	}
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
	pos, err := parseFlagsInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 || *zoneID == "" || *name == "" {
		return fmt.Errorf("usage: fleet ddns policy --zone-id Z... --name HOST")
	}
	p, err := ddns.Policy(*zoneID, *name)
	if err != nil {
		return err
	}
	return writeJSONOut(cio.Stdout, p)
}

func cmdDDNS(args []string, cio *IO) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: fleet ddns setup|run|status|policy")
	}
	switch args[0] {
	case "setup":
		return cmdDDNSSetup(args[1:], cio)
	case "run":
		return cmdDDNSRun(args[1:], cio)
	case "status":
		return cmdDDNSStatus(args[1:], cio)
	case "policy":
		return cmdDDNSPolicy(args[1:], cio)
	default:
		return fmt.Errorf("unknown ddns subcommand %q", args[0])
	}
}
