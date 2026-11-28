package cli

import (
	"cmp"
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/bdatdo0601/fleet/internal/client"
	"github.com/bdatdo0601/fleet/internal/core"
	"github.com/bdatdo0601/fleet/internal/ddns"
	"github.com/bdatdo0601/fleet/internal/paths"
	"github.com/bdatdo0601/fleet/internal/system"
	"github.com/bdatdo0601/fleet/internal/tlsdns"
)

func cmdEdge(args []string, cio *IO) error {
	fs := flag.NewFlagSet("edge", flag.ContinueOnError)
	stateDir := fs.String("state-dir", "", "lighthouse fleet state directory (~/.fleet)")
	apiURL := fs.String("api", fmt.Sprintf("http://127.0.0.1:%d", paths.APIPort()), "upstream fleet serve URL")
	httpsPort := fs.Int("https-port", paths.HTTPSPort(), "HTTPS listen port")
	dnsPort := fs.Int("dns-port", paths.DNSPort(), "DNS listen port")
	pos, err := parseFlagsInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(pos, " "))
	}
	resolvedState := strings.TrimSpace(*stateDir)
	if resolvedState == "" {
		if node, err := client.LoadNode(); err == nil && node.StateDir != "" {
			resolvedState = node.StateDir
		}
	}
	if resolvedState == "" {
		resolvedState = paths.StateDir()
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return tlsdns.RunEdge(ctx, tlsdns.EdgeOpts{
		StateDir:    resolvedState,
		UpstreamURL: *apiURL,
		HTTPSAddr:   fmt.Sprintf("0.0.0.0:%d", *httpsPort),
		DNSPort:     *dnsPort,
		Logf: func(format string, a ...any) {
			cio.printf(format+"\n", a...)
		},
	})
}

func cmdTLS(args []string, cio *IO) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: fleet tls policy|setup|status|renew|disable")
	}
	switch args[0] {
	case "policy":
		return cmdTLSPolicy(args[1:], cio)
	case "setup":
		return cmdTLSSetup(args[1:], cio)
	case "status":
		return cmdTLSStatus(args[1:], cio)
	case "renew":
		return cmdTLSRenew(args[1:], cio)
	case "disable":
		return cmdTLSDisable(args[1:], cio)
	default:
		return fmt.Errorf("unknown tls subcommand %q", args[0])
	}
}

func cmdTLSPolicy(args []string, cio *IO) error {
	fs := flag.NewFlagSet("tls policy", flag.ContinueOnError)
	zoneID := fs.String("zone-id", "", "Route 53 hosted zone id")
	domain := fs.String("domain", "", "TLS/DNS domain (e.g. fleet.example.com)")
	ddnsName := fs.String("ddns-name", "", "optional DDNS A record name to include")
	noPublicA := fs.Bool("no-public-a", false, "omit static A record permission on <domain>")
	pos, err := parseFlagsInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(pos, " "))
	}
	if *zoneID == "" || *domain == "" {
		return fmt.Errorf("usage: fleet tls policy --zone-id Z... --domain HOST [--ddns-name HOST] [--no-public-a]")
	}
	ddnsHost := strings.TrimSpace(*ddnsName)
	if ddnsHost == "" {
		if fleet, err := core.Open(""); err == nil {
			if cfg, dErr := fleet.DDNSConfig(); dErr == nil && cfg != nil {
				ddnsHost = cfg.Name
			}
			_ = fleet.Close()
		}
	}
	pol, err := tlsdns.Policy(tlsdns.PolicyOpts{
		ZoneID:         *zoneID,
		Domain:         *domain,
		DDNSName:       ddnsHost,
		IncludePublicA: !*noPublicA,
	})
	if err != nil {
		return err
	}
	return writeJSONOut(cio.Stdout, pol)
}

func tlsPreflight(domain, zoneID, profile, email string, publicA, privateDNS, staging bool) (tlsdns.Config, error) {
	normDomain, err := tlsdns.NormalizeDomain(domain)
	if err != nil {
		return tlsdns.Config{}, err
	}
	zid, err := ddns.NormalizeZoneID(zoneID)
	if err != nil {
		return tlsdns.Config{}, err
	}
	if _, err := exec.LookPath("aws"); err != nil {
		return tlsdns.Config{}, fmt.Errorf("aws CLI not found (brew install awscli)")
	}
	out, _ := exec.Command("aws", "configure", "list-profiles").Output()
	if !slices.Contains(strings.Fields(string(out)), profile) {
		return tlsdns.Config{}, fmt.Errorf(
			"no AWS profile %q. Create or update the IAM user policy with:\n  fleet tls policy --zone-id %s --domain %s\nthen configure credentials: aws configure --profile %s",
			profile, zid, normDomain, profile,
		)
	}
	ns, err := ddns.FindNameservers(normDomain, nil)
	if err != nil {
		return tlsdns.Config{}, err
	}
	return tlsdns.Config{
		Domain:      normDomain,
		ZoneID:      zid,
		Profile:     profile,
		Email:       strings.TrimSpace(email),
		Nameservers: ns,
		PublicA:     publicA,
		PrivateDNS:  privateDNS,
		Staging:     staging,
	}, nil
}

func refreshLighthouseConfigAndNebula(cio *IO, fleet *core.Fleet) {
	lh, err := fleet.Registry.Lighthouse()
	if err != nil || lh == nil {
		return
	}
	cfgPtr, err := fleet.ConfigFor(lh)
	if err != nil || cfgPtr == nil {
		return
	}
	etc := paths.NebulaEtc()
	wrote, err := system.WriteRootFile(filepath.Join(etc, "config.yml"), *cfgPtr, 0o644)
	if err != nil {
		cio.printf("warning: update lighthouse nebula config: %v\n", err)
		return
	}
	if wrote {
		if label := system.NebulaLabel(""); label != "" && system.LaunchdLoaded(label) {
			if rErr := system.Reload(label); rErr != nil {
				if rsErr := system.RestartService(label); rsErr != nil {
					cio.printf("warning: restart %s: %v\n", label, rsErr)
				}
			}
		}
		cio.info("updated lighthouse Nebula firewall (TCP 443, UDP/TCP 53)")
	}
}

func resolveTLSZoneAndProfile(fleet *core.Fleet, zoneID, profile string) (string, string, error) {
	zid := strings.TrimSpace(zoneID)
	prof := strings.TrimSpace(profile)
	if ddnsCfg, dErr := fleet.DDNSConfig(); dErr == nil && ddnsCfg != nil {
		zid = cmp.Or(zid, ddnsCfg.ZoneID)
		prof = cmp.Or(prof, ddnsCfg.Profile)
	}
	prof = cmp.Or(prof, "fleet-ddns")
	if zid == "" {
		return "", "", fmt.Errorf("--zone-id is required (no Route 53 DDNS zone configured yet)")
	}
	return zid, prof, nil
}

func installAndStartTLSDaemons(cio *IO, fleet *core.Fleet, cfg tlsdns.Config) error {
	if _, err := tlsdns.EnsureEdgeToken(fleet.Root); err != nil {
		return err
	}
	instBin, err := ensureInstalledSelf()
	if err != nil {
		return err
	}
	cio.info("installing & starting fleet edge (HTTPS :443 + mesh DNS :53) ...")
	if err := system.InstallEdgeDaemon(instBin, fleet.Root, fleet.Root); err != nil {
		return err
	}
	if system.LaunchdLoaded(paths.ServeLabel) {
		if err := system.RestartService(paths.ServeLabel); err != nil {
			return err
		}
	} else if err := system.InstallServeDaemon(instBin, currentUsername(), fleet.Root, fleet.Root); err != nil {
		return err
	}
	refreshLighthouseConfigAndNebula(cio, fleet)
	if cfg.PrivateDNS {
		if _, _, err := system.SyncMacResolver(paths.ResolverDir, cfg.Domain, paths.LighthouseIP); err != nil {
			cio.printf("warning: configure local /etc/resolver/%s: %v\n", cfg.Domain, err)
		} else {
			cio.info(fmt.Sprintf("configured macOS split-DNS /etc/resolver/%s -> %s", cfg.Domain, paths.LighthouseIP))
		}
	}
	return nil
}

func cmdTLSSetup(args []string, cio *IO) error {
	fs := flag.NewFlagSet("tls setup", flag.ContinueOnError)
	domain := fs.String("domain", "", "HTTPS/DNS domain (e.g. fleet.example.com)")
	zoneID := fs.String("zone-id", "", "Route 53 hosted zone id (defaults to DDNS zone if configured)")
	profile := fs.String("profile", "", "AWS profile (defaults to DDNS profile or fleet-ddns)")
	email := fs.String("email", "", "ACME account contact email (optional)")
	publicA := fs.Bool("public-a", true, "maintain Route 53 A record pointing to 10.213.0.1")
	privateDNS := fs.Bool("private-dns", true, "serve private split-DNS inside Nebula on port 53")
	staging := fs.Bool("staging", false, "use Let's Encrypt staging CA (for testing)")
	pos, err := parseFlagsInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(pos, " "))
	}
	if strings.TrimSpace(*domain) == "" {
		return fmt.Errorf("usage: fleet tls setup --domain HOST [--zone-id Z...] [--profile fleet-ddns] [--email EMAIL] [--public-a=true] [--private-dns=true] [--staging]")
	}
	if err := requireNotRoot("tls setup"); err != nil {
		return err
	}

	fleet, err := core.Open("")
	if err != nil {
		return err
	}
	defer func() { _ = fleet.Close() }()

	zid, prof, err := resolveTLSZoneAndProfile(fleet, *zoneID, *profile)
	if err != nil {
		return err
	}
	cfg, err := tlsPreflight(*domain, zid, prof, *email, *publicA, *privateDNS, *staging)
	if err != nil {
		return err
	}
	if err := fleet.SetTLS(cfg); err != nil {
		return err
	}
	if cfg.PublicA {
		cio.info(fmt.Sprintf("setting Route 53 A record %s -> %s ...", cfg.Domain, paths.LighthouseIP))
		if _, err := tlsdns.UpsertStaticA(cfg, paths.LighthouseIP, "", nil); err != nil {
			return fmt.Errorf("%w\nEnsure IAM permissions include:\n  fleet tls policy --zone-id %s --domain %s", err, cfg.ZoneID, cfg.Domain)
		}
	}

	cio.info(fmt.Sprintf("issuing Let's Encrypt certificate for %s via Route 53 DNS-01 ...", cfg.Domain))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	renewed, status, err := tlsdns.EnsureCert(ctx, paths.TLSDir(fleet.Root), cfg, false, tlsdns.ACMEDeps{
		Logf: func(format string, a ...any) { cio.printf(format+"\n", a...) },
	})
	if err != nil {
		return fmt.Errorf("%w\nEnsure IAM permissions include:\n  fleet tls policy --zone-id %s --domain %s", err, cfg.ZoneID, cfg.Domain)
	}
	if renewed {
		cio.info(fmt.Sprintf("certificate issued (expires %s, %d days remaining)", status.NotAfter, status.DaysRemaining))
	} else {
		cio.info(fmt.Sprintf("existing certificate is valid (%d days remaining)", status.DaysRemaining))
	}

	if err := installAndStartTLSDaemons(cio, fleet, cfg); err != nil {
		return err
	}
	cio.printf("\nFleet Console HTTPS: https://%s/\n", cfg.Domain)
	cio.printf("  Macs: run `sudo fleet sync` (or wait <= 5m) to configure /etc/resolver/%s.\n", cfg.Domain)
	cio.printf("  iPhone/iPad: connect Mobile Nebula (optionally set DNS %s / match domain %s) and open https://%s/ in Safari -> Share -> Add to Home Screen.\n",
		paths.LighthouseIP, cfg.Domain, cfg.Domain)
	return nil
}

func cmdTLSStatus(args []string, cio *IO) error {
	if len(args) > 0 {
		return fmt.Errorf("usage: fleet tls status")
	}
	node, err := client.LoadNode()
	if err != nil {
		return err
	}
	var res map[string]any
	if err := client.Call(node.API, "GET", "/v1/tls?live=true", nil, 15*time.Second, nil, &res); err != nil {
		return err
	}
	configured, _ := res["configured"].(bool)
	if !configured {
		cio.printf("TLS:          not configured (run `fleet tls setup --domain ...` on the lighthouse)\n")
		return nil
	}
	domain, _ := res["domain"].(string)
	urlStr, _ := res["url"].(string)
	zoneID, _ := res["zone_id"].(string)
	profile, _ := res["profile"].(string)
	pubA, _ := res["public_a"].(bool)
	privDNS, _ := res["private_dns"].(bool)
	edgeLoaded, _ := res["edge_loaded"].(bool)

	cio.printf("url:          %s\n", urlStr)
	cio.printf("zone/profile: %s (%s)\n", zoneID, profile)
	cio.printf("edge daemon:  %v\n", edgeLoaded)
	cio.printf("public A:     %v", pubA)
	if rec, ok := res["public_a_record"].(string); ok {
		inSync, _ := res["public_a_in_sync"].(bool)
		cio.printf(" (DNS answer: %s, in_sync=%v)", rec, inSync)
	}
	cio.printf("\n")
	cio.printf("private DNS:  %v", privDNS)
	if ans, ok := res["private_dns_answer"].(string); ok {
		okDNS, _ := res["private_dns_ok"].(bool)
		cio.printf(" (local query: %s, ok=%v)", ans, okDNS)
	}
	cio.printf("\n")

	if certMap, ok := res["cert"].(map[string]any); ok {
		exists, _ := certMap["exists"].(bool)
		if exists {
			issuer, _ := certMap["issuer"].(string)
			notAfter, _ := certMap["not_after"].(string)
			days, _ := certMap["days_remaining"].(float64)
			cio.printf("certificate:  %s, expires %s (%.0f days left)\n", issuer, notAfter, days)
		} else {
			errMsg, _ := certMap["error"].(string)
			cio.printf("certificate:  missing (%s)\n", cmp.Or(errMsg, "none"))
		}
	}
	if _, err := os.Stat(filepath.Join(paths.ResolverDir, domain)); err == nil {
		cio.printf("mac resolver: /etc/resolver/%s present\n", domain)
	} else {
		cio.printf("mac resolver: /etc/resolver/%s missing (run `sudo fleet sync`)\n", domain)
	}
	if liveErr, ok := res["live_error"].(string); ok && liveErr != "" {
		cio.printf("warning:      %s\n", liveErr)
	}
	return nil
}

func cmdTLSRenew(args []string, cio *IO) error {
	fs := flag.NewFlagSet("tls renew", flag.ContinueOnError)
	force := fs.Bool("force", false, "force certificate renewal even if >30 days remain")
	pos, err := parseFlagsInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(pos, " "))
	}
	node, err := client.LoadNode()
	if err != nil {
		return err
	}
	var res tlsdns.RenewResult
	if err := client.Call(node.API, "POST", "/v1/tls/renew", map[string]bool{"force": *force}, 5*time.Minute, nil, &res); err != nil {
		return err
	}
	if res.Renewed {
		cio.info(fmt.Sprintf("certificate renewed for %s (expires %s, %d days remaining)", res.Cert.Domain, res.Cert.NotAfter, res.Cert.DaysRemaining))
	} else {
		cio.info(fmt.Sprintf("certificate for %s is healthy (%d days remaining)", res.Cert.Domain, res.Cert.DaysRemaining))
	}
	return nil
}

func cmdTLSDisable(args []string, cio *IO) error {
	if len(args) > 0 {
		return fmt.Errorf("usage: fleet tls disable")
	}
	if err := requireNotRoot("tls disable"); err != nil {
		return err
	}
	fleet, err := core.Open("")
	if err != nil {
		return err
	}
	defer func() { _ = fleet.Close() }()

	if err := fleet.ClearTLS(); err != nil {
		return err
	}
	if err := system.UninstallDaemon(paths.EdgeLabel); err != nil {
		return err
	}
	if system.LaunchdLoaded(paths.ServeLabel) {
		if err := system.RestartService(paths.ServeLabel); err != nil {
			cio.printf("warning: restart %s: %v\n", paths.ServeLabel, err)
		}
	}
	if _, _, err := system.SyncMacResolver(paths.ResolverDir, "", ""); err != nil {
		cio.printf("warning: cleanup /etc/resolver: %v\n", err)
	}
	cio.info("disabled HTTPS edge proxy and private mesh DNS")
	return nil
}
