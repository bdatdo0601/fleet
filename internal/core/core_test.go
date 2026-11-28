package core

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/bdatdo0601/fleet/internal/pki"
	"github.com/bdatdo0601/fleet/internal/registry"
)

const testPassphrase = "correct horse battery staple"

func haveNebula(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("nebula"); err != nil {
		t.Skip("nebula not installed")
	}
	if _, err := exec.LookPath("nebula-cert"); err != nil {
		t.Skip("nebula-cert not installed")
	}
}

func makeKeypair(t *testing.T, dir, name string) (string, string) {
	t.Helper()
	key := filepath.Join(dir, name+".key")
	pub := filepath.Join(dir, name+".pub")
	if err := pki.Keygen(key, pub); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(pub)
	if err != nil {
		t.Fatal(err)
	}
	return key, string(b)
}

func runNebulaTest(t *testing.T, cfg, pkiDir, ca, cert, keyPath string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(pkiDir, "ca.crt"), []byte(ca), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkiDir, "host.crt"), []byte(cert), 0o644); err != nil {
		t.Fatal(err)
	}
	keyBytes, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkiDir, "host.key"), keyBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(pkiDir, "config.yml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("nebula", "-test", "-config", cfgPath).CombinedOutput()
	if err != nil {
		t.Fatalf("nebula -test failed: %v\n%s", err, string(out))
	}
}

func newTestFleet(t *testing.T) (*Fleet, string, string) {
	t.Helper()
	tmp := t.TempDir()
	pkiDir := filepath.Join(tmp, "etc")
	if err := os.MkdirAll(pkiDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FLEET_NEBULA_ETC", pkiDir)
	t.Setenv("FLEET_API_PORT", "7443")

	miniKey, miniPub := makeKeypair(t, tmp, "mini")
	f, err := Initialize(InitOpts{
		Root:             filepath.Join(tmp, "state"),
		Host:             "home.example.com",
		Port:             443,
		LanIP:            "192.168.1.50",
		Passphrase:       testPassphrase,
		LighthousePubkey: miniPub,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f, tmp, miniKey
}

func TestInitializeAndRefuseTwice(t *testing.T) {
	haveNebula(t)
	f, tmp, miniKey := newTestFleet(t)

	mini, err := f.Registry.Lighthouse()
	if err != nil || mini == nil {
		t.Fatalf("missing lighthouse: %v", err)
	}
	if mini.Name != "mini" || mini.IP != "10.213.0.1" || mini.Grp != "lighthouse" {
		t.Fatalf("unexpected lighthouse: %+v", mini)
	}
	st, err := os.Stat(filepath.Join(tmp, "state"))
	if err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("expected 0700 state dir, got %v %v", st.Mode().Perm(), err)
	}
	kst, err := os.Stat(filepath.Join(tmp, "state", "ca.key"))
	if err != nil || kst.Mode().Perm() != 0o600 {
		t.Fatalf("expected 0600 ca.key, got %v %v", kst.Mode().Perm(), err)
	}
	keyText, _ := os.ReadFile(filepath.Join(tmp, "state", "ca.key"))
	if !strings.Contains(string(keyText), "ENCRYPTED") {
		t.Fatalf("expected encrypted ca.key")
	}

	if _, err := Initialize(InitOpts{
		Root:             filepath.Join(tmp, "state"),
		Host:             "x",
		Port:             1,
		Passphrase:       testPassphrase,
		LighthousePubkey: mini.Pubkey,
	}); err == nil {
		t.Fatal("expected Initialize to refuse twice")
	}

	cfg, err := f.ConfigFor(mini)
	if err != nil || cfg == nil {
		t.Fatal(err)
	}
	caPEM, _ := f.CAPem()
	runNebulaTest(t, *cfg, filepath.Join(tmp, "etc"), caPEM, mini.Cert, miniKey)
	if !strings.Contains(*cfg, "port: 7443") {
		t.Fatalf("expected port: 7443 in lighthouse config")
	}
}

func TestApproveRevokeSetGroupIdentifyAndNumericIPSort(t *testing.T) {
	haveNebula(t)
	f, tmp, _ := newTestFleet(t)
	pkiDir := filepath.Join(tmp, "etc")

	lapKey, lapPub := makeKeypair(t, tmp, "laptop")
	resp, err := f.Approve(ApproveOpts{
		Name: "laptop", Pubkey: lapPub, Kind: "mac", Group: "admin", Passphrase: testPassphrase,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.IP != "10.213.0.2" {
		t.Fatalf("expected 10.213.0.2, got %s", resp.IP)
	}
	meta, err := pki.Info(resp.Cert)
	if err != nil {
		t.Fatal(err)
	}
	if len(meta.Groups) != 1 || meta.Groups[0] != "admin" || len(meta.Networks) != 1 || meta.Networks[0] != "10.213.0.2/16" {
		t.Fatalf("unexpected cert meta: %+v", meta)
	}
	if err := pki.Verify(resp.CA, resp.Cert); err != nil {
		t.Fatal(err)
	}
	if resp.API != "http://10.213.0.1:7443" {
		t.Fatalf("unexpected api: %s", resp.API)
	}
	runNebulaTest(t, *resp.Config, pkiDir, resp.CA, resp.Cert, lapKey)

	_, offPub := makeKeypair(t, tmp, "office")
	offResp, err := f.Approve(ApproveOpts{
		Name: "office", Pubkey: offPub, Kind: "mac", Group: "managed", Passphrase: testPassphrase,
	})
	if err != nil || offResp.IP != "10.213.0.3" {
		t.Fatalf("unexpected office resp: %+v, err=%v", offResp, err)
	}

	// Duplicate without --replace returns 409
	_, err = f.Approve(ApproveOpts{Name: "laptop", Pubkey: lapPub, Kind: "mac", Group: "admin", Passphrase: testPassphrase})
	if fe, ok := errors.AsType[*FleetError](err); !ok || fe.Status != 409 {
		t.Fatalf("expected 409 FleetError, got %v", err)
	}
	second, err := f.Approve(ApproveOpts{
		Name: "laptop", Pubkey: lapPub, Kind: "mac", Group: "admin", Passphrase: testPassphrase, Replace: true,
	})
	if err != nil || second.IP != resp.IP {
		t.Fatalf("replace failed: %+v, %v", second, err)
	}
	bl, _ := f.Registry.Blocklist()
	if len(bl) == 0 || bl[0] != meta.Fingerprint {
		t.Fatalf("expected old fingerprint %s in blocklist %v", meta.Fingerprint, bl)
	}

	// Wrong passphrase returns 401 and does not register
	_, badPub := makeKeypair(t, tmp, "x")
	_, err = f.Approve(ApproveOpts{Name: "x", Pubkey: badPub, Kind: "mac", Group: "admin", Passphrase: "nope"})
	if fe, ok := errors.AsType[*FleetError](err); !ok || fe.Status != 401 || !strings.Contains(fe.Msg, "wrong CA passphrase") {
		t.Fatalf("expected 401 wrong passphrase, got %v", err)
	}
	if dev, _ := f.Registry.Get("x"); dev != nil {
		t.Fatalf("failed signing should not register device")
	}

	// Mobile cert
	_, phPub := makeKeypair(t, tmp, "phone")
	phResp, err := f.Approve(ApproveOpts{
		Name: "phone", Pubkey: phPub, Kind: "mobile", Group: "admin", Passphrase: testPassphrase,
	})
	if err != nil || phResp.Config != nil || phResp.Lighthouse == nil || phResp.Lighthouse.Address != "home.example.com:443" {
		t.Fatalf("unexpected mobile resp: %+v, %v", phResp, err)
	}
	hrs, err := pki.HoursUntil(phResp.NotAfter)
	if err != nil || hrs < 8750 || hrs > 8761 {
		t.Fatalf("unexpected mobile cert hours: %d, %v", hrs, err)
	}

	// Identify
	if d, _ := f.Identify("127.0.0.1"); d == nil || d.Name != "mini" {
		t.Fatalf("expected mini on loopback")
	}
	if d, _ := f.Identify("10.213.0.2"); d == nil || d.Name != "laptop" || !IsAdmin(d) {
		t.Fatalf("expected admin laptop")
	}
	if d, _ := f.Identify("10.213.0.3"); d == nil || d.Name != "office" || IsAdmin(d) {
		t.Fatalf("expected managed office")
	}
	if d, _ := f.Identify("10.213.9.9"); d != nil {
		t.Fatalf("unexpected unregistered device: %+v", d)
	}
	if d, _ := f.Identify("192.168.1.20"); d != nil {
		t.Fatalf("unexpected LAN IP match: %+v", d)
	}
	if err := f.Revoke("office"); err != nil {
		t.Fatal(err)
	}
	if d, _ := f.Identify("10.213.0.3"); d != nil {
		t.Fatalf("revoked device should not identify")
	}

	// SetGroup
	sgResp, err := f.SetGroup("laptop", "managed", testPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	sgMeta, _ := pki.Info(sgResp.Cert)
	if len(sgMeta.Groups) != 1 || sgMeta.Groups[0] != "managed" {
		t.Fatalf("expected managed group, got %v", sgMeta.Groups)
	}
}

func TestLegacyFleetDBMigrationAndNumericSort(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "fleet.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	// Create legacy schema without client_version
	_, err = db.Exec(`
		CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
		CREATE TABLE devices (
			name TEXT PRIMARY KEY, ip TEXT NOT NULL UNIQUE,
			kind TEXT NOT NULL CHECK (kind IN ('lighthouse', 'mac', 'mobile')),
			grp TEXT NOT NULL, pubkey TEXT NOT NULL, cert TEXT NOT NULL,
			fingerprint TEXT NOT NULL, not_after TEXT NOT NULL,
			created_at TEXT NOT NULL, revoked_at TEXT, last_seen TEXT
		);
		CREATE TABLE revoked_certs (
			fingerprint TEXT PRIMARY KEY, name TEXT NOT NULL, revoked_at TEXT NOT NULL, reason TEXT NOT NULL
		);
		INSERT INTO devices(name, ip, kind, grp, pubkey, cert, fingerprint, not_after, created_at)
		VALUES ('mini', '10.213.0.1', 'lighthouse', 'lighthouse', 'p', 'c', 'f', '2031-01-01T00:00:00+00:00', '2026-01-01T00:00:00+00:00'),
		       ('ten', '10.213.0.10', 'mac', 'admin', 'p', 'c', 'f2', '2031-01-01T00:00:00+00:00', '2026-01-01T00:00:00+00:00'),
		       ('two', '10.213.0.2', 'mac', 'admin', 'p', 'c', 'f3', '2031-01-01T00:00:00+00:00', '2026-01-01T00:00:00+00:00');
	`)
	_ = db.Close()
	if err != nil {
		t.Fatal(err)
	}

	reg, err := registry.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reg.Close() }()

	if err := reg.Touch("two", "go/abc1234"); err != nil {
		t.Fatal(err)
	}
	devs, err := reg.Devices()
	if err != nil {
		t.Fatal(err)
	}
	if len(devs) != 3 || devs[0].Name != "mini" || devs[1].Name != "two" || devs[2].Name != "ten" {
		t.Fatalf("expected numeric IP sort [mini, two, ten], got %+v", devs)
	}
	if devs[1].ClientVersion == nil || *devs[1].ClientVersion != "go/abc1234" {
		t.Fatalf("expected client_version go/abc1234, got %+v", devs[1].ClientVersion)
	}
}

func TestSpawnAgentLighthouseDefaultsRepoToHome(t *testing.T) {
	f, _, _ := newTestFleet(t)
	res, err := f.SpawnAgent(context.Background(), nil, SpawnAgentRequest{
		Harness: "claude",
		Device:  "mini",
		Prompt:  "install uv",
	})
	if err != nil {
		t.Fatalf("SpawnAgent on lighthouse failed: %v", err)
	}
	if res.Job.TargetDevice != "mini" || res.Job.RepoPath != "~" {
		t.Fatalf("expected TargetDevice=mini RepoPath=~, got %+v", res.Job)
	}
	if list, err := f.Agents.ListPaths("mini", ""); err != nil || len(list) != 0 {
		t.Fatalf("expected ~ not recorded as project repo path, got %+v (err=%v)", list, err)
	}
}
