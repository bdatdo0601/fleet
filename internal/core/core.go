// Package core implements lighthouse-side operations on the device registry and agent hub.
package core

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/bdatdo0601/fleet/internal/agents"
	"github.com/bdatdo0601/fleet/internal/codes"
	"github.com/bdatdo0601/fleet/internal/ddns"
	"github.com/bdatdo0601/fleet/internal/memory"
	"github.com/bdatdo0601/fleet/internal/paths"
	"github.com/bdatdo0601/fleet/internal/pki"
	"github.com/bdatdo0601/fleet/internal/registry"
	"github.com/bdatdo0601/fleet/internal/render"
	"github.com/bdatdo0601/fleet/internal/slacker"
)

// WrongPassphraseMsg matches the Python error message inspected by `fleet` passphrase retry logic.
const WrongPassphraseMsg = "wrong CA passphrase (the one set during `fleet init`)"

// FleetError represents a caller-level error with an HTTP status code.
type FleetError struct {
	Msg    string
	Status int
}

func (e *FleetError) Error() string { return e.Msg }

// HTTPStatus returns the HTTP status code associated with this error.
func (e *FleetError) HTTPStatus() int { return e.Status }

func newFleetErr(msg string, status int) *FleetError {
	if status == 0 {
		status = 400
	}
	return &FleetError{Msg: msg, Status: status}
}

// BinaryEntry describes one compiled platform artifact published in manifest.json.
type BinaryEntry struct {
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// BinaryManifest describes compiled binaries published by the lighthouse in <state_dir>/bin/manifest.json.
type BinaryManifest struct {
	Version  string                 `json:"version"`
	Commit   string                 `json:"commit"`
	BuiltAt  string                 `json:"built_at,omitempty"`
	Binaries map[string]BinaryEntry `json:"binaries"`
}

// SyncResult is returned by GET /v1/sync.
type SyncResult struct {
	Name          string  `json:"name"`
	IP            string  `json:"ip"`
	Kind          string  `json:"kind"`
	CA            string  `json:"ca"`
	Cert          string  `json:"cert"`
	Config        *string `json:"config"`
	BinarySHA256  string  `json:"binary_sha256,omitempty"`
	BinaryVersion string  `json:"binary_version,omitempty"`
}

// LighthouseInfo describes the lighthouse endpoint for mobile devices.
type LighthouseInfo struct {
	IP      string `json:"ip"`
	Address string `json:"address"`
}

// ApproveResult is returned by POST /v1/approve and POST /v1/set-group.
type ApproveResult struct {
	Name          string          `json:"name"`
	IP            string          `json:"ip"`
	Kind          string          `json:"kind"`
	Group         string          `json:"group"`
	CA            string          `json:"ca"`
	Cert          string          `json:"cert"`
	CAID          string          `json:"ca_id"`
	NotAfter      string          `json:"not_after"`
	Config        *string         `json:"config,omitempty"`
	API           string          `json:"api,omitempty"`
	ResponseBlock string          `json:"response_block,omitempty"`
	Lighthouse    *LighthouseInfo `json:"lighthouse,omitempty"`
}

// CAInfo holds metadata about the fleet's Certificate Authority.
type CAInfo struct {
	CAID        string `json:"ca_id"`
	Fingerprint string `json:"fingerprint"`
	NotAfter    string `json:"not_after"`
}

// DecodedJoinBlock represents a parsed join request block or Mobile Nebula public key.
type DecodedJoinBlock struct {
	Kind            string `json:"kind"`
	Name            string `json:"name,omitempty"`
	Pubkey          string `json:"pubkey"`
	RequestID       string `json:"request_id"`
	Existing        bool   `json:"existing"`
	ExistingIP      string `json:"existing_ip,omitempty"`
	ExistingGroup   string `json:"existing_group,omitempty"`
	ExistingRevoked bool   `json:"existing_revoked,omitempty"`
}

// DeviceItem is one entry in GET /v1/devices.
type DeviceItem struct {
	Name          string  `json:"name"`
	IP            string  `json:"ip"`
	Kind          string  `json:"kind"`
	Group         string  `json:"group"`
	NotAfter      string  `json:"not_after"`
	LastSeen      *string `json:"last_seen"`
	RevokedAt     *string `json:"revoked_at"`
	ClientVersion *string `json:"client_version"`
}

// InitOpts configures Fleet.Initialize.
type InitOpts struct {
	Root             string
	Host             string
	Port             int
	LanIP            string
	Passphrase       string
	LighthousePubkey string
}

// ApproveOpts configures Fleet.Approve.
type ApproveOpts struct {
	Name       string `json:"name"`
	Pubkey     string `json:"pubkey"`
	Kind       string `json:"kind"`
	Group      string `json:"group"`
	Passphrase string `json:"passphrase"`
	Replace    bool   `json:"replace"`
}

// Fleet coordinates CA keys, the device registry, the agent session hub, shared memory, and Slacker.
type Fleet struct {
	Root              string
	CAKey             string
	CACrt             string
	SlackerConfigPath string
	Registry          *registry.Registry
	Agents            *agents.Hub
	Memory            *memory.Store
	Curator           *memory.Curator
	Slacker           *slacker.Store
	SlackClient       *slacker.SlackClient
	approveMu         sync.Mutex
	IdentifyOverride  func(peerIP string) *registry.Device // optional hook for tests
}

// Open opens an existing fleet state directory (or creates the DBs if missing).
func Open(root string) (*Fleet, error) {
	if root == "" {
		root = paths.StateDir()
	}
	reg, err := registry.Open(filepath.Join(root, "fleet.db"))
	if err != nil {
		return nil, err
	}
	hub, err := agents.Open(filepath.Join(root, "agents.db"))
	if err != nil {
		_ = reg.Close()
		return nil, err
	}
	mem, err := memory.Open(filepath.Join(root, "memory.db"))
	if err != nil {
		_ = hub.Close()
		_ = reg.Close()
		return nil, err
	}
	slk, err := slacker.Open(filepath.Join(root, "slacker.db"))
	if err != nil {
		_ = mem.Close()
		_ = hub.Close()
		_ = reg.Close()
		return nil, err
	}
	curator := memory.NewCurator(mem, filepath.Join(root, "curator.json"))
	f := &Fleet{
		Root:              root,
		CAKey:             filepath.Join(root, "ca.key"),
		CACrt:             filepath.Join(root, "ca.crt"),
		SlackerConfigPath: filepath.Join(root, "slacker.json"),
		Registry:          reg,
		Agents:            hub,
		Memory:            mem,
		Curator:           curator,
		Slacker:           slk,
	}
	f.backfillPathsOnce()
	return f, nil
}

func (f *Fleet) backfillPathsOnce() {
	if f.Registry == nil || f.Agents == nil {
		return
	}
	if v, ok, err := f.Registry.Setting("paths_backfilled"); err != nil || (ok && v == "1") {
		return
	}
	if cfg, err := slacker.LoadConfig(f.SlackerConfigPath); err == nil {
		for dev, dc := range cfg.Devices {
			if dc.WorkspaceDir != "" {
				_ = f.Agents.RecordPath(dev, dc.WorkspaceDir, "", "slacker_config", false)
			}
		}
		for _, ch := range cfg.Channels {
			for _, dev := range cfg.EffectiveChannelDevices(ch) {
				if resolved, rErr := cfg.ResolveRepoPath(dev, ch.RepoPath); rErr == nil && resolved != "" {
					_ = f.Agents.RecordPath(dev, resolved, agents.RepoNameFromPath(resolved), "slacker_config", false)
				}
			}
		}
	}
	if f.Slacker != nil {
		if jobs, err := f.Slacker.ListJobs("", "", 500); err == nil {
			for _, j := range jobs {
				if j.RepoPath == "" || j.Reaction == "kill" || j.Reaction == "kill-all" {
					continue
				}
				_ = f.Agents.RecordPath(j.TargetDevice, j.RepoPath, agents.RepoNameFromPath(j.RepoPath), "slacker_job", true)
			}
		}
	}
	_ = f.Registry.SetSetting("paths_backfilled", "1")
}

// Initialize creates the CA, registry, and lighthouse entry in opts.Root.
func Initialize(opts InitOpts) (*Fleet, error) {
	root := opts.Root
	if root == "" {
		root = paths.StateDir()
	}
	dbPath := filepath.Join(root, "fleet.db")
	if _, err := os.Stat(dbPath); err == nil {
		return nil, newFleetErr(fmt.Sprintf("%s already exists; this fleet is initialised", dbPath), 400)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return nil, err
	}
	caKey := filepath.Join(root, "ca.key")
	caCrt := filepath.Join(root, "ca.crt")
	if err := pki.CreateCA(caKey, caCrt, opts.Passphrase, "fleet"); err != nil {
		return nil, err
	}
	if err := os.Chmod(caKey, 0o600); err != nil {
		return nil, err
	}
	f, err := Open(root)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(dbPath, 0o600); err != nil {
		_ = f.Close()
		return nil, err
	}
	cleanup := func(err error) (*Fleet, error) {
		_ = f.Close()
		return nil, err
	}
	if err := f.Registry.SetSetting("lighthouse_host", opts.Host); err != nil {
		return cleanup(err)
	}
	if err := f.Registry.SetSetting("lighthouse_port", strconv.Itoa(opts.Port)); err != nil {
		return cleanup(err)
	}
	if opts.LanIP != "" {
		if err := f.Registry.SetSetting("lighthouse_lan_ip", opts.LanIP); err != nil {
			return cleanup(err)
		}
	}
	cert, err := f.sign(opts.LighthousePubkey, "mini", paths.LighthouseIP, "lighthouse", opts.Passphrase, 0)
	if err != nil {
		return cleanup(err)
	}
	meta, err := pki.Info(cert)
	if err != nil {
		return cleanup(err)
	}
	if err := f.Registry.Add(registry.AddParams{
		Name:        "mini",
		IP:          paths.LighthouseIP,
		Kind:        "lighthouse",
		Grp:         "lighthouse",
		Pubkey:      opts.LighthousePubkey,
		Cert:        cert,
		Fingerprint: meta.Fingerprint,
		NotAfter:    meta.NotAfter,
	}); err != nil {
		return cleanup(err)
	}
	return f, nil
}

// Close stops background workers and closes underlying SQLite databases.
func (f *Fleet) Close() error {
	var errs []error
	if f.Curator != nil {
		f.Curator.Stop()
	}
	if f.Registry != nil {
		if err := f.Registry.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if f.Agents != nil {
		if err := f.Agents.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if f.Memory != nil {
		if err := f.Memory.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if f.Slacker != nil {
		if err := f.Slacker.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Settings returns persisted lighthouse configuration settings.
func (f *Fleet) Settings() (map[string]string, error) {
	out := map[string]string{}
	for _, k := range []string{"lighthouse_host", "lighthouse_port", "lighthouse_lan_ip"} {
		v, ok, err := f.Registry.Setting(k)
		if err != nil {
			return nil, err
		}
		if ok {
			out[k] = v
		}
	}
	return out, nil
}

// DDNSConfig loads the stored Route 53 configuration, or nil if unconfigured.
func (f *Fleet) DDNSConfig() (*ddns.Route53, error) {
	zone, ok, err := f.Registry.Setting("ddns_zone_id")
	if err != nil || !ok || zone == "" {
		return nil, err
	}
	name, _, err := f.Registry.Setting("ddns_name")
	if err != nil {
		return nil, err
	}
	prof, _, err := f.Registry.Setting("ddns_profile")
	if err != nil {
		return nil, err
	}
	nsRaw, _, err := f.Registry.Setting("ddns_nameservers")
	if err != nil {
		return nil, err
	}
	var ns []string
	if nsRaw != "" {
		ns = strings.Split(nsRaw, ",")
	}
	return &ddns.Route53{
		Name:        name,
		ZoneID:      zone,
		Profile:     prof,
		Nameservers: ns,
	}, nil
}

// SetDDNS stores Route 53 DDNS configuration in fleet.db.
func (f *Fleet) SetDDNS(cfg ddns.Route53) error {
	for _, kv := range [][2]string{
		{"ddns_name", cfg.Name},
		{"ddns_zone_id", cfg.ZoneID},
		{"ddns_profile", cfg.Profile},
		{"ddns_nameservers", strings.Join(cfg.Nameservers, ",")},
	} {
		if err := f.Registry.SetSetting(kv[0], kv[1]); err != nil {
			return err
		}
	}
	return nil
}

// CAPem reads the CA certificate PEM from disk.
func (f *Fleet) CAPem() (string, error) {
	b, err := os.ReadFile(f.CACrt)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// CAInfo returns the CA verification ID, fingerprint, and expiration timestamp.
func (f *Fleet) CAInfo() (CAInfo, error) {
	caPEM, err := f.CAPem()
	if err != nil {
		return CAInfo{}, err
	}
	meta, err := pki.Info(caPEM)
	if err != nil {
		return CAInfo{}, err
	}
	return CAInfo{
		CAID:        codes.ShortID(meta.Fingerprint),
		Fingerprint: meta.Fingerprint,
		NotAfter:    meta.NotAfter,
	}, nil
}

// CAID returns the short verification ID derived from the CA certificate fingerprint.
func (f *Fleet) CAID() (string, error) {
	info, err := f.CAInfo()
	if err != nil {
		return "", err
	}
	return info.CAID, nil
}

// ConfigFor renders the Nebula config YAML for dev, or returns nil for mobile devices.
func (f *Fleet) ConfigFor(dev *registry.Device) (*string, error) {
	if dev.Kind == "mobile" {
		return nil, nil
	}
	settings, err := f.Settings()
	if err != nil {
		return nil, err
	}
	blocklist, err := f.Registry.Blocklist()
	if err != nil {
		return nil, err
	}
	cfg, err := render.Render(dev.Kind, settings, blocklist, "")
	if err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (f *Fleet) sign(pubkey, name, ip, group, passphrase string, hours int) (string, error) {
	if passphrase == "" {
		return "", newFleetErr("CA passphrase required", 400)
	}
	cert, err := pki.Sign(pki.SignOpts{
		CAKey:      f.CAKey,
		CACrt:      f.CACrt,
		Pubkey:     pubkey,
		Name:       name,
		IP:         ip,
		Group:      group,
		Passphrase: passphrase,
		Hours:      hours,
	})
	if err != nil {
		if strings.Contains(err.Error(), "invalid passphrase") {
			return "", newFleetErr(WrongPassphraseMsg, 401)
		}
		return "", newFleetErr("signing failed: "+err.Error(), 400)
	}
	return cert, nil
}

func (f *Fleet) mobileHours() (int, error) {
	caPEM, err := f.CAPem()
	if err != nil {
		return 0, err
	}
	meta, err := pki.Info(caPEM)
	if err != nil {
		return 0, err
	}
	rem, err := pki.HoursUntil(meta.NotAfter)
	if err != nil {
		return 0, err
	}
	return max(min(rem-1, paths.MobileCertHours), 1), nil
}

// Identify resolves the calling device from peerIP (loopback -> lighthouse; mesh IP -> active device).
func (f *Fleet) Identify(peerIP string) (*registry.Device, error) {
	if f.IdentifyOverride != nil {
		return f.IdentifyOverride(peerIP), nil
	}
	addr, err := netip.ParseAddr(peerIP)
	if err == nil {
		if addr.IsLoopback() {
			return f.Registry.Lighthouse()
		}
		meshPrefix, pErr := netip.ParsePrefix(paths.MeshCIDR)
		if pErr != nil {
			return nil, pErr
		}
		if meshPrefix.Contains(addr) {
			dev, bErr := f.Registry.ByIP(addr.String())
			if bErr != nil {
				return nil, bErr
			}
			if dev != nil && dev.Active() {
				return dev, nil
			}
		}
	}
	return nil, nil
}

// IsAdmin reports whether dev is authorized to perform admin actions.
func IsAdmin(dev *registry.Device) bool {
	return dev != nil && (dev.Grp == "admin" || dev.Grp == "lighthouse")
}

// BinDir returns <Root>/bin where compiled platform binaries and manifest.json are published.
func (f *Fleet) BinDir() string {
	return filepath.Join(f.Root, "bin")
}

// ReadBinaryManifest loads <Root>/bin/manifest.json if present.
func (f *Fleet) ReadBinaryManifest() (*BinaryManifest, error) {
	data, err := os.ReadFile(filepath.Join(f.BinDir(), "manifest.json"))
	if err != nil {
		return nil, err
	}
	var m BinaryManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// BinaryForPlatform returns the path, SHA-256 hex, and version for osName/archName if published.
func (f *Fleet) BinaryForPlatform(osName, archName string) (string, string, string, error) {
	if osName != "darwin" && osName != "linux" {
		return "", "", "", newFleetErr(fmt.Sprintf("unsupported os %q", osName), 400)
	}
	if archName != "arm64" && archName != "amd64" {
		return "", "", "", newFleetErr(fmt.Sprintf("unsupported arch %q", archName), 400)
	}
	m, err := f.ReadBinaryManifest()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", "", "", newFleetErr("no published binary manifest on lighthouse", 404)
		}
		return "", "", "", err
	}
	key := osName + "/" + archName
	entry, ok := m.Binaries[key]
	if !ok || entry.File == "" || entry.SHA256 == "" {
		return "", "", "", newFleetErr(fmt.Sprintf("no published binary for %s", key), 404)
	}
	if filepath.Base(entry.File) != entry.File || strings.Contains(entry.File, "..") {
		return "", "", "", newFleetErr("invalid manifest binary path", 500)
	}
	binPath := filepath.Join(f.BinDir(), entry.File)
	if info, err := os.Stat(binPath); err != nil || info.IsDir() {
		return "", "", "", newFleetErr(fmt.Sprintf("binary artifact missing for %s", key), 404)
	}
	ver := m.Version
	if m.Commit != "" {
		short := m.Commit
		if len(short) > 7 {
			short = short[:7]
		}
		ver = "go/" + short
	}
	return binPath, entry.SHA256, ver, nil
}

// Sync records device check-in and returns its CA, cert, rendered config, and optional binary metadata.
func (f *Fleet) Sync(dev *registry.Device, clientVersion, osName, archName string) (SyncResult, error) {
	if err := f.Registry.Touch(dev.Name, clientVersion); err != nil {
		return SyncResult{}, err
	}
	caPEM, err := f.CAPem()
	if err != nil {
		return SyncResult{}, err
	}
	cfg, err := f.ConfigFor(dev)
	if err != nil {
		return SyncResult{}, err
	}
	res := SyncResult{
		Name:   dev.Name,
		IP:     dev.IP,
		Kind:   dev.Kind,
		CA:     caPEM,
		Cert:   dev.Cert,
		Config: cfg,
	}
	if osName != "" && archName != "" {
		if _, sha, ver, err := f.BinaryForPlatform(osName, archName); err == nil {
			res.BinarySHA256 = sha
			res.BinaryVersion = ver
		}
	}
	return res, nil
}

func normalizeApproveOpts(opts ApproveOpts) (ApproveOpts, error) {
	opts.Kind = cmp.Or(opts.Kind, "mac")
	opts.Group = cmp.Or(opts.Group, "managed")
	if !paths.NamePattern.MatchString(opts.Name) {
		return opts, newFleetErr("name must be lowercase letters, digits and dashes (max 32)", 400)
	}
	if opts.Kind != "mac" && opts.Kind != "mobile" {
		return opts, newFleetErr(fmt.Sprintf("unknown kind %q", opts.Kind), 400)
	}
	if !paths.ValidGroup(opts.Group) {
		return opts, newFleetErr("group must be one of "+strings.Join(paths.Groups, ", "), 400)
	}
	pubkey, err := pki.NormalizePubkey(opts.Pubkey)
	if err != nil {
		return opts, newFleetErr(err.Error(), 400)
	}
	opts.Pubkey = pubkey
	return opts, nil
}

func (f *Fleet) buildApproveResult(opts ApproveOpts, dev *registry.Device, ip, cert, notAfter string) (ApproveResult, error) {
	caPEM, err := f.CAPem()
	if err != nil {
		return ApproveResult{}, err
	}
	caID, err := f.CAID()
	if err != nil {
		return ApproveResult{}, err
	}
	res := ApproveResult{
		Name: opts.Name, IP: ip, Kind: opts.Kind, Group: opts.Group,
		CA: caPEM, Cert: cert, CAID: caID, NotAfter: notAfter,
	}
	if opts.Kind == "mac" {
		cfg, err := f.ConfigFor(dev)
		if err != nil {
			return ApproveResult{}, err
		}
		res.Config = cfg
		res.API = fmt.Sprintf("http://%s:%d", paths.LighthouseIP, paths.APIPort())
		block, err := codes.Encode(codes.Response, map[string]any{
			"name":   res.Name,
			"ip":     res.IP,
			"kind":   res.Kind,
			"ca":     res.CA,
			"cert":   res.Cert,
			"config": res.Config,
			"api":    res.API,
			"ca_id":  res.CAID,
		})
		if err != nil {
			return ApproveResult{}, err
		}
		res.ResponseBlock = block
		return res, nil
	}
	s, err := f.Settings()
	if err != nil {
		return ApproveResult{}, err
	}
	res.Lighthouse = &LighthouseInfo{
		IP:      paths.LighthouseIP,
		Address: fmt.Sprintf("%s:%s", s["lighthouse_host"], s["lighthouse_port"]),
	}
	return res, nil
}

// Approve validates, signs, and registers (or re-keys) a device.
func (f *Fleet) Approve(opts ApproveOpts) (ApproveResult, error) {
	var err error
	opts, err = normalizeApproveOpts(opts)
	if err != nil {
		return ApproveResult{}, err
	}

	f.approveMu.Lock()
	defer f.approveMu.Unlock()

	existing, err := f.Registry.Get(opts.Name)
	if err != nil {
		return ApproveResult{}, err
	}
	if existing != nil && existing.Kind == "lighthouse" {
		return ApproveResult{}, newFleetErr("the lighthouse can't be re-approved", 409)
	}
	if existing != nil && !opts.Replace {
		return ApproveResult{}, newFleetErr(fmt.Sprintf("%s already exists (%s); pass --replace to re-key it", opts.Name, existing.IP), 409)
	}
	ip := ""
	if existing != nil {
		ip = existing.IP
	} else if ip, err = f.Registry.NextIP(); err != nil {
		return ApproveResult{}, err
	}
	hours := 0
	if opts.Kind == "mobile" {
		if hours, err = f.mobileHours(); err != nil {
			return ApproveResult{}, err
		}
	}
	cert, err := f.sign(opts.Pubkey, opts.Name, ip, opts.Group, opts.Passphrase, hours)
	if err != nil {
		return ApproveResult{}, err
	}
	meta, err := pki.Info(cert)
	if err != nil {
		return ApproveResult{}, err
	}
	if existing != nil {
		err = f.Registry.ReplaceCert(registry.ReplaceCertParams{
			Name: opts.Name, Grp: opts.Group, Pubkey: opts.Pubkey, Cert: cert,
			Fingerprint: meta.Fingerprint, NotAfter: meta.NotAfter, Reason: "replaced",
		})
	} else {
		err = f.Registry.Add(registry.AddParams{
			Name: opts.Name, IP: ip, Kind: opts.Kind, Grp: opts.Group,
			Pubkey: opts.Pubkey, Cert: cert, Fingerprint: meta.Fingerprint, NotAfter: meta.NotAfter,
		})
	}
	if err != nil {
		return ApproveResult{}, err
	}
	dev, err := f.Registry.Get(opts.Name)
	if err != nil {
		return ApproveResult{}, err
	}
	if dev == nil {
		return ApproveResult{}, newFleetErr("device missing after registration", 500)
	}
	return f.buildApproveResult(opts, dev, ip, cert, meta.NotAfter)
}

// SetGroup changes a device's group by re-signing its certificate and blocklisting the old cert.
func (f *Fleet) SetGroup(name, group, passphrase string) (ApproveResult, error) {
	dev, err := f.existing(name)
	if err != nil {
		return ApproveResult{}, err
	}
	return f.Approve(ApproveOpts{
		Name:       name,
		Pubkey:     dev.Pubkey,
		Kind:       dev.Kind,
		Group:      group,
		Passphrase: passphrase,
		Replace:    true,
	})
}

// Revoke cuts off a device and blocklists its current certificate.
func (f *Fleet) Revoke(name string) error {
	f.approveMu.Lock()
	defer f.approveMu.Unlock()
	dev, err := f.existing(name)
	if err != nil {
		return err
	}
	if dev.Kind == "lighthouse" {
		return newFleetErr("the lighthouse can't be revoked", 409)
	}
	if !dev.Active() {
		return newFleetErr(fmt.Sprintf("%s is already revoked", name), 409)
	}
	return f.Registry.Revoke(name)
}

// List returns all devices for GET /v1/devices.
func (f *Fleet) List() ([]DeviceItem, error) {
	devs, err := f.Registry.Devices()
	if err != nil {
		return nil, err
	}
	out := make([]DeviceItem, 0, len(devs))
	for _, d := range devs {
		out = append(out, DeviceItem{
			Name:          d.Name,
			IP:            d.IP,
			Kind:          d.Kind,
			Group:         d.Grp,
			NotAfter:      d.NotAfter,
			LastSeen:      d.LastSeen,
			RevokedAt:     d.RevokedAt,
			ClientVersion: d.ClientVersion,
		})
	}
	return out, nil
}

// ActiveDeviceNames returns a set of all active (non-revoked) device names.
func (f *Fleet) ActiveDeviceNames() (map[string]bool, error) {
	devs, err := f.Registry.Devices()
	if err != nil {
		return nil, err
	}
	names := make(map[string]bool, len(devs))
	for _, d := range devs {
		if d.Active() {
			names[d.Name] = true
		}
	}
	return names, nil
}

func (f *Fleet) existing(name string) (*registry.Device, error) {
	dev, err := f.Registry.Get(name)
	if err != nil {
		return nil, err
	}
	if dev == nil {
		return nil, newFleetErr(fmt.Sprintf("no device named %s", name), 404)
	}
	return dev, nil
}

// RevokedCerts returns all revoked or replaced certificates in the PKI blocklist.
func (f *Fleet) RevokedCerts() ([]registry.RevokedCert, error) {
	return f.Registry.RevokedCerts()
}

// DeviceConfig returns the rendered Nebula config YAML for the named device.
func (f *Fleet) DeviceConfig(name string) (string, error) {
	dev, err := f.existing(name)
	if err != nil {
		return "", err
	}
	if dev.Kind == "mobile" {
		return "", newFleetErr("mobile devices do not use a server-rendered Nebula config", 400)
	}
	cfg, err := f.ConfigFor(dev)
	if err != nil {
		return "", err
	}
	if cfg == nil {
		return "", newFleetErr("config unavailable", 404)
	}
	return *cfg, nil
}

// DecodeJoinBlock parses either a FLEET JOIN REQUEST block or a Mobile Nebula X25519 public key.
func (f *Fleet) DecodeJoinBlock(raw string) (DecodedJoinBlock, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return DecodedJoinBlock{}, newFleetErr("join block or public key is required", 400)
	}
	var out DecodedJoinBlock
	if strings.Contains(trimmed, "BEGIN "+codes.Request) {
		var req struct {
			Name   string `json:"name"`
			Pubkey string `json:"pubkey"`
		}
		if err := codes.Decode(codes.Request, trimmed, &req); err != nil {
			return DecodedJoinBlock{}, newFleetErr(err.Error(), 400)
		}
		pk, err := pki.NormalizePubkey(req.Pubkey)
		if err != nil {
			return DecodedJoinBlock{}, newFleetErr(err.Error(), 400)
		}
		out.Kind = "mac"
		out.Name = strings.TrimSpace(req.Name)
		out.Pubkey = pk
	} else {
		pk, err := pki.NormalizePubkey(trimmed)
		if err != nil {
			return DecodedJoinBlock{}, newFleetErr("expected a FLEET JOIN REQUEST block or NEBULA X25519 PUBLIC KEY", 400)
		}
		out.Kind = "mobile"
		out.Pubkey = pk
	}
	out.RequestID = codes.ShortID(out.Pubkey)
	if out.Name != "" {
		dev, err := f.Registry.Get(out.Name)
		if err != nil {
			return DecodedJoinBlock{}, err
		}
		if dev != nil {
			out.Existing = true
			out.ExistingIP = dev.IP
			out.ExistingGroup = dev.Grp
			out.ExistingRevoked = dev.RevokedAt != nil
		}
	}
	return out, nil
}
