package server

import (
	"cmp"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/bdatdo0601/fleet/internal/agents"
	"github.com/bdatdo0601/fleet/internal/core"
	"github.com/bdatdo0601/fleet/internal/ddns"
	"github.com/bdatdo0601/fleet/internal/memory"
	"github.com/bdatdo0601/fleet/internal/paths"
	"github.com/bdatdo0601/fleet/internal/registry"
	"github.com/bdatdo0601/fleet/internal/tlsdns"
	"github.com/bdatdo0601/fleet/internal/version"
)

const logTailMaxBytes = 128 * 1024

func buildAgentSummary(fleet *core.Fleet) (map[string]int, error) {
	sessions, err := fleet.Agents.ListSessions("", "", "", "", true)
	if err != nil {
		return nil, err
	}
	summary := map[string]int{
		"total":              len(sessions),
		"live":               0,
		"working":            0,
		"waiting":            0,
		"waiting_permission": 0,
		"waiting_question":   0,
		"idle":               0,
		"listening":          0,
		"stale":              0,
	}
	for _, s := range sessions {
		if !s.Live {
			summary["stale"]++
			continue
		}
		summary["live"]++
		if s.Listening {
			summary["listening"]++
		}
		switch s.State {
		case "working":
			summary["working"]++
		case "idle":
			summary["idle"]++
		case "waiting_permission":
			summary["waiting"]++
			summary["waiting_permission"]++
		case "waiting_question":
			summary["waiting"]++
			summary["waiting_question"]++
		}
	}
	return summary, nil
}

func buildDeviceSummary(fleet *core.Fleet, manifest *core.BinaryManifest) (map[string]int, error) {
	devs, err := fleet.List()
	if err != nil {
		return nil, err
	}
	expectedClient := ""
	if manifest != nil && manifest.Commit != "" {
		short := manifest.Commit
		if len(short) > 7 {
			short = short[:7]
		}
		expectedClient = "go/" + short
	}
	cutoff := time.Now().UTC().Add(30 * 24 * time.Hour)
	summary := map[string]int{
		"total":         len(devs),
		"active":        0,
		"revoked":       0,
		"admins":        0,
		"managed":       0,
		"mobiles":       0,
		"expiring_soon": 0,
		"outdated":      0,
	}
	for _, d := range devs {
		if d.RevokedAt != nil && *d.RevokedAt != "" {
			summary["revoked"]++
			continue
		}
		summary["active"]++
		switch d.Group {
		case "admin", "lighthouse":
			summary["admins"]++
		case "managed":
			summary["managed"]++
		}
		if d.Kind == "mobile" {
			summary["mobiles"]++
		} else if expectedClient != "" && (d.ClientVersion == nil || *d.ClientVersion != expectedClient) {
			summary["outdated"]++
		}
		if exp, pErr := time.Parse(time.RFC3339, d.NotAfter); pErr == nil && !exp.After(cutoff) {
			summary["expiring_soon"]++
		}
	}
	return summary, nil
}

func buildDDNSSummary(cfg *ddns.Route53) map[string]any {
	if cfg == nil {
		return map[string]any{"configured": false}
	}
	ns := cfg.Nameservers
	if ns == nil {
		ns = []string{}
	}
	return map[string]any{
		"configured":  true,
		"name":        cfg.Name,
		"zone_id":     cfg.ZoneID,
		"profile":     cfg.Profile,
		"nameservers": ns,
	}
}

func buildOverviewPayload(fleet *core.Fleet, c *registry.Device, startedAt time.Time, tlsMgr *tlsdns.Manager) (map[string]any, error) {
	agentSummary, err := buildAgentSummary(fleet)
	if err != nil {
		return nil, err
	}
	var memStats memory.Stats
	if fleet.Memory != nil {
		memStats, err = fleet.Memory.Stats()
		if err != nil {
			return nil, err
		}
	}
	var curatorStatus memory.CuratorStatus
	if fleet.Curator != nil {
		curatorStatus = fleet.Curator.Status()
	}
	usageList, uErr := fleet.Agents.ListUsage()
	if uErr != nil {
		usageList = []agents.HarnessUsage{}
	}
	isAdmin := core.IsAdmin(c)
	serverInfo := map[string]any{
		"version":        version.Version,
		"commit":         version.ShortCommit(),
		"built_at":       version.BuiltAt,
		"os":             runtime.GOOS,
		"arch":           runtime.GOARCH,
		"started_at":     startedAt.Format(time.RFC3339),
		"uptime_seconds": max(int(time.Since(startedAt).Seconds()), 0),
		"api_port":       paths.APIPort(),
		"https_port":     paths.HTTPSPort(),
		"dns_port":       paths.DNSPort(),
		"lighthouse_ip":  paths.LighthouseIP,
		"mesh_cidr":      paths.MeshCIDR,
	}
	if tlsCfg, tErr := fleet.TLSConfig(); tErr == nil && tlsCfg != nil && tlsCfg.Domain != "" {
		serverInfo["tls_domain"] = tlsCfg.Domain
	}
	payload := map[string]any{
		"caller": map[string]any{
			"name":     c.Name,
			"ip":       c.IP,
			"kind":     c.Kind,
			"group":    c.Grp,
			"is_admin": isAdmin,
		},
		"server":  serverInfo,
		"agents":  agentSummary,
		"usage":   usageList,
		"memory":  memStats,
		"curator": curatorStatus,
	}
	if !isAdmin {
		return payload, nil
	}

	var manifest *core.BinaryManifest
	if m, mErr := fleet.ReadBinaryManifest(); mErr == nil {
		manifest = m
	} else if !errors.Is(mErr, os.ErrNotExist) {
		return nil, mErr
	}
	devSummary, err := buildDeviceSummary(fleet, manifest)
	if err != nil {
		return nil, err
	}
	caInfo, err := fleet.CAInfo()
	if err != nil {
		return nil, err
	}
	settings, err := fleet.Settings()
	if err != nil {
		return nil, err
	}
	ddnsCfg, err := fleet.DDNSConfig()
	if err != nil {
		return nil, err
	}
	slkSummary := buildSlackerSummary(fleet)
	payload["devices"] = devSummary
	payload["ca"] = caInfo
	payload["settings"] = settings
	payload["manifest"] = manifest
	payload["ddns"] = buildDDNSSummary(ddnsCfg)
	if tlsSummary, err := buildTLSSummary(fleet, tlsMgr); err == nil {
		payload["tls"] = tlsSummary
	}
	payload["slacker"] = slkSummary
	payload["assistant"] = buildAssistantSummary(fleet)
	return payload, nil
}

func resolveLogPath(fleet *core.Fleet, name string) (string, string, error) {
	n := strings.ToLower(strings.TrimSpace(cmp.Or(name, "serve")))
	switch n {
	case "serve":
		return n, filepath.Join(fleet.Root, "serve.log"), nil
	case "edge":
		return n, "/var/log/fleet-edge.log", nil
	case "ddns":
		return n, filepath.Join(fleet.Root, "ddns.log"), nil
	case "slacker":
		return n, filepath.Join(fleet.Root, "slacker.log"), nil
	case "sync":
		return n, "/var/log/fleet-sync.log", nil
	case "nebula":
		return n, "/opt/homebrew/var/log/nebula.log", nil
	default:
		return "", "", &core.FleetError{Msg: "unknown log name (expected serve, edge, ddns, slacker, sync, or nebula)", Status: http.StatusBadRequest}
	}
}

func tailLogFile(name, path string, maxLines int) (map[string]any, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{
			"name":   name,
			"path":   path,
			"exists": false,
			"size":   int64(0),
			"lines":  []string{},
		}, nil
	}
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return map[string]any{
				"name":       name,
				"path":       path,
				"exists":     true,
				"size":       info.Size(),
				"updated_at": info.ModTime().UTC().Format(time.RFC3339),
				"lines":      []string{},
				"error":      err.Error(),
			}, nil
		}
		return nil, err
	}
	defer func() { _ = f.Close() }()

	offset := max(info.Size()-logTailMaxBytes, 0)
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return nil, err
		}
	}
	raw, err := io.ReadAll(io.LimitReader(f, logTailMaxBytes+1))
	if err != nil {
		return nil, err
	}
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	if offset > 0 {
		if _, after, ok := strings.Cut(text, "\n"); ok {
			text = after
		}
	}
	text = strings.TrimSuffix(text, "\n")
	lines := []string{}
	if text != "" {
		lines = strings.Split(text, "\n")
	}
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return map[string]any{
		"name":       name,
		"path":       path,
		"exists":     true,
		"size":       info.Size(),
		"updated_at": info.ModTime().UTC().Format(time.RFC3339),
		"lines":      lines,
	}, nil
}

func registerAdminRoutes(
	mux *http.ServeMux,
	fleet *core.Fleet,
	startedAt time.Time,
	watchdog *NebulaWatchdog,
	tlsMgr *tlsdns.Manager,
	route func(bool, func(http.ResponseWriter, *http.Request, *registry.Device) error) http.HandlerFunc,
) {
	mux.HandleFunc("GET /v1/overview", route(false, func(w http.ResponseWriter, _ *http.Request, c *registry.Device) error {
		payload, err := buildOverviewPayload(fleet, c, startedAt, tlsMgr)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, payload)
		return nil
	}))

	mux.HandleFunc("POST /v1/join/decode", route(true, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		var req struct {
			Block string `json:"block"`
		}
		if err := readJSONBody(r, &req); err != nil {
			return err
		}
		decoded, err := fleet.DecodeJoinBlock(req.Block)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, decoded)
		return nil
	}))

	mux.HandleFunc("GET /v1/devices/{name}/config", route(true, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		name := r.PathValue("name")
		cfg, err := fleet.DeviceConfig(name)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]string{"name": name, "config": cfg})
		return nil
	}))

	mux.HandleFunc("GET /v1/revocations", route(true, func(w http.ResponseWriter, _ *http.Request, _ *registry.Device) error {
		list, err := fleet.RevokedCerts()
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{"revocations": list})
		return nil
	}))

	registerDDNSRoutes(mux, fleet, route)
	registerTLSRoutes(mux, fleet, tlsMgr, route)
	registerNebulaRoutes(mux, fleet, watchdog, route)

	mux.HandleFunc("GET /v1/logs", route(true, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		name, path, err := resolveLogPath(fleet, r.URL.Query().Get("name"))
		if err != nil {
			return err
		}
		lines := queryInt(r, "lines")
		if lines <= 0 {
			lines = 150
		}
		lines = min(lines, 500)
		payload, err := tailLogFile(name, path, lines)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, payload)
		return nil
	}))
}

func registerDDNSRoutes(
	mux *http.ServeMux,
	fleet *core.Fleet,
	route func(bool, func(http.ResponseWriter, *http.Request, *registry.Device) error) http.HandlerFunc,
) {
	mux.HandleFunc("GET /v1/ddns", route(true, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		cfg, err := fleet.DDNSConfig()
		if err != nil {
			return err
		}
		res := buildDDNSSummary(cfg)
		if cfg != nil && queryBool(r, "live") {
			attachLiveDDNSStatus(res, *cfg)
		}
		writeJSON(w, http.StatusOK, res)
		return nil
	}))

	mux.HandleFunc("POST /v1/ddns/sync", route(true, func(w http.ResponseWriter, _ *http.Request, _ *registry.Device) error {
		cfg, err := fleet.DDNSConfig()
		if err != nil {
			return err
		}
		if cfg == nil {
			return &core.FleetError{Msg: "DDNS is not configured", Status: http.StatusBadRequest}
		}
		before, ip, changed, err := ddns.Sync(*cfg, nil, nil, "", nil)
		if err != nil {
			return &core.FleetError{Msg: err.Error(), Status: http.StatusBadGateway}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"before":  before,
			"after":   ip,
			"changed": changed,
		})
		return nil
	}))

	mux.HandleFunc("GET /v1/ddns/policy", route(true, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		q := r.URL.Query()
		zoneID := q.Get("zone_id")
		name := strings.TrimSpace(q.Get("name"))
		if name == "" {
			return &core.FleetError{Msg: "name is required", Status: http.StatusBadRequest}
		}
		pol, err := ddns.Policy(zoneID, name)
		if err != nil {
			return &core.FleetError{Msg: err.Error(), Status: http.StatusBadRequest}
		}
		writeJSON(w, http.StatusOK, pol)
		return nil
	}))
}

func registerNebulaRoutes(
	mux *http.ServeMux,
	fleet *core.Fleet,
	watchdog *NebulaWatchdog,
	route func(bool, func(http.ResponseWriter, *http.Request, *registry.Device) error) http.HandlerFunc,
) {
	mux.HandleFunc("GET /v1/nebula/status", route(true, func(w http.ResponseWriter, _ *http.Request, _ *registry.Device) error {
		health, err := core.ReadNebulaHealth(fleet.Root)
		if err != nil {
			return err
		}
		if watchdog != nil {
			wd := watchdog.State()
			health.Watchdog = &wd
		}
		writeJSON(w, http.StatusOK, health)
		return nil
	}))

	mux.HandleFunc("POST /v1/nebula/restart", route(true, func(w http.ResponseWriter, r *http.Request, caller *registry.Device) error {
		var req struct {
			Reason string `json:"reason"`
		}
		if err := readJSONBody(r, &req); err != nil {
			return err
		}
		reason := cmp.Or(strings.TrimSpace(req.Reason), "manual")
		queued, err := core.RequestNebulaRestart(fleet.Root, caller.Name, reason)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"status":  "queued",
			"request": queued,
		})
		return nil
	}))
}

func attachLiveDDNSStatus(res map[string]any, cfg ddns.Route53) {
	var errs []string
	pubIP, pErr := ddns.PublicIPv4(nil)
	if pErr != nil {
		errs = append(errs, pErr.Error())
	} else {
		res["public_ip"] = pubIP
	}
	rec, rErr := ddns.CurrentRecord(cfg, nil)
	if rErr != nil {
		errs = append(errs, rErr.Error())
	} else if rec != nil {
		res["dns_record"] = *rec
	}
	res["in_sync"] = pErr == nil && rErr == nil && rec != nil && *rec == pubIP
	if len(errs) > 0 {
		res["live_error"] = strings.Join(errs, "; ")
	}
}
