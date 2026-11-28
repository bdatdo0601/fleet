package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/bdatdo0601/fleet/internal/core"
	"github.com/bdatdo0601/fleet/internal/paths"
	"github.com/bdatdo0601/fleet/internal/registry"
	"github.com/bdatdo0601/fleet/internal/tlsdns"
)

const (
	edgeDaemonCacheTTL     = 5 * time.Second
	edgeDaemonProbeTimeout = 2 * time.Second
)

var edgeDaemonCache struct {
	mu        sync.Mutex
	checkedAt time.Time
	loaded    bool
}

func isEdgeDaemonLoaded() bool {
	edgeDaemonCache.mu.Lock()
	defer edgeDaemonCache.mu.Unlock()
	if !edgeDaemonCache.checkedAt.IsZero() && time.Since(edgeDaemonCache.checkedAt) < edgeDaemonCacheTTL {
		return edgeDaemonCache.loaded
	}
	loaded := false
	if _, err := os.Stat(filepath.Join("/Library/LaunchDaemons", paths.EdgeLabel+".plist")); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), edgeDaemonProbeTimeout)
		defer cancel()
		loaded = exec.CommandContext(ctx, "launchctl", "print", "system/"+paths.EdgeLabel).Run() == nil
	}
	edgeDaemonCache.checkedAt = time.Now()
	edgeDaemonCache.loaded = loaded
	return loaded
}

func buildTLSSummary(fleet *core.Fleet, tlsMgr *tlsdns.Manager) (map[string]any, error) {
	cfg, err := fleet.TLSConfig()
	if err != nil {
		return nil, err
	}
	edgeLoaded := isEdgeDaemonLoaded()
	if cfg == nil {
		return map[string]any{
			"configured":  false,
			"edge_loaded": edgeLoaded,
			"https_port":  paths.HTTPSPort(),
			"dns_port":    paths.DNSPort(),
		}, nil
	}
	ns := cfg.Nameservers
	if ns == nil {
		ns = []string{}
	}
	cert := tlsdns.InspectCert(paths.TLSDir(fleet.Root), cfg.Domain, time.Now().UTC())
	res := map[string]any{
		"configured":  true,
		"domain":      cfg.Domain,
		"url":         fmt.Sprintf("https://%s/", cfg.Domain),
		"zone_id":     cfg.ZoneID,
		"profile":     cfg.Profile,
		"email":       cfg.Email,
		"nameservers": ns,
		"public_a":    cfg.PublicA,
		"private_dns": cfg.PrivateDNS,
		"staging":     cfg.Staging,
		"https_port":  paths.HTTPSPort(),
		"dns_port":    paths.DNSPort(),
		"edge_loaded": edgeLoaded,
		"cert":        cert,
	}
	if tlsMgr != nil {
		res["manager"] = tlsMgr.State()
	}
	return res, nil
}

func probeLocalMeshDNS(domain string, port int) (string, error) {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	conn, err := net.DialTimeout("udp4", addr, 2*time.Second)
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))

	qname, err := dnsmessage.NewName(strings.TrimSuffix(domain, ".") + ".")
	if err != nil {
		return "", err
	}
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 0x4242, RecursionDesired: false})
	if err := b.StartQuestions(); err != nil {
		return "", err
	}
	if err := b.Question(dnsmessage.Question{Name: qname, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}); err != nil {
		return "", err
	}
	pkt, err := b.Finish()
	if err != nil {
		return "", err
	}
	if _, err := conn.Write(pkt); err != nil {
		return "", err
	}
	buf := make([]byte, 512)
	n, err := conn.Read(buf)
	if err != nil {
		return "", err
	}
	var p dnsmessage.Parser
	hdr, err := p.Start(buf[:n])
	if err != nil {
		return "", err
	}
	if hdr.RCode != dnsmessage.RCodeSuccess {
		return "", fmt.Errorf("DNS rcode %v", hdr.RCode)
	}
	_ = p.SkipAllQuestions()
	for {
		ah, err := p.AnswerHeader()
		if err != nil {
			break
		}
		if ah.Type == dnsmessage.TypeA {
			a, err := p.AResource()
			if err == nil {
				return net.IP(a.A[:]).String(), nil
			}
		} else {
			_ = p.SkipAnswer()
		}
	}
	return "", fmt.Errorf("no A record returned for %s", domain)
}

func attachLiveTLSStatus(res map[string]any, cfg tlsdns.Config) {
	var errs []string
	if cfg.PublicA {
		rec, err := tlsdns.CheckStaticA(cfg, nil)
		switch {
		case err != nil:
			errs = append(errs, "public A: "+err.Error())
		case rec != nil:
			res["public_a_record"] = *rec
			res["public_a_in_sync"] = *rec == paths.LighthouseIP
		default:
			res["public_a_in_sync"] = false
		}
	}
	if cfg.PrivateDNS {
		ans, err := probeLocalMeshDNS(cfg.Domain, paths.DNSPort())
		if err != nil {
			errs = append(errs, "mesh DNS: "+err.Error())
			res["private_dns_ok"] = false
		} else {
			res["private_dns_answer"] = ans
			res["private_dns_ok"] = ans == paths.LighthouseIP
		}
	}
	if len(errs) > 0 {
		res["live_error"] = strings.Join(errs, "; ")
	}
}

func registerTLSRoutes(
	mux *http.ServeMux,
	fleet *core.Fleet,
	tlsMgr *tlsdns.Manager,
	route func(bool, func(http.ResponseWriter, *http.Request, *registry.Device) error) http.HandlerFunc,
) {
	mux.HandleFunc("GET /v1/dns/records", route(true, func(w http.ResponseWriter, _ *http.Request, _ *registry.Device) error {
		snap, err := fleet.DNSRecords()
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, snap)
		return nil
	}))

	mux.HandleFunc("GET /v1/tls", route(true, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		res, err := buildTLSSummary(fleet, tlsMgr)
		if err != nil {
			return err
		}
		if queryBool(r, "live") {
			if cfg, cErr := fleet.TLSConfig(); cErr == nil && cfg != nil {
				attachLiveTLSStatus(res, *cfg)
			}
		}
		writeJSON(w, http.StatusOK, res)
		return nil
	}))

	mux.HandleFunc("POST /v1/tls/renew", route(true, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		var req struct {
			Force bool `json:"force"`
		}
		if err := readJSONBody(r, &req); err != nil {
			return err
		}
		mgr := tlsMgr
		if mgr == nil {
			mgr = tlsdns.NewManager(fleet.Root, fleet.TLSConfig, tlsdns.ACMEDeps{})
		}
		res, err := mgr.RenewNow(r.Context(), req.Force)
		if err != nil {
			status := http.StatusBadGateway
			if errors.Is(err, tlsdns.ErrRenewalInProgress) {
				status = http.StatusConflict
			}
			return &core.FleetError{Msg: err.Error(), Status: status}
		}
		writeJSON(w, http.StatusOK, res)
		return nil
	}))

	mux.HandleFunc("GET /v1/tls/policy", route(true, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		q := r.URL.Query()
		zoneID := q.Get("zone_id")
		domain := strings.TrimSpace(q.Get("domain"))
		if domain == "" {
			return &core.FleetError{Msg: "domain is required", Status: http.StatusBadRequest}
		}
		ddnsName := strings.TrimSpace(q.Get("ddns_name"))
		if ddnsName == "" {
			if cfg, err := fleet.DDNSConfig(); err == nil && cfg != nil {
				ddnsName = cfg.Name
			}
		}
		includePubA := true
		if raw := strings.TrimSpace(q.Get("public_a")); raw != "" {
			includePubA = raw == "1" || strings.EqualFold(raw, "true") || strings.EqualFold(raw, "yes")
		}
		pol, err := tlsdns.Policy(tlsdns.PolicyOpts{
			ZoneID:         zoneID,
			Domain:         domain,
			DDNSName:       ddnsName,
			IncludePublicA: includePubA,
		})
		if err != nil {
			return &core.FleetError{Msg: err.Error(), Status: http.StatusBadRequest}
		}
		writeJSON(w, http.StatusOK, pol)
		return nil
	}))
}
