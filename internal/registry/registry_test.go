package registry

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func TestOpenSchemaMigration(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "fleet.db")
	// Simulate legacy schema lacking client_version column.
	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = rawDB.Exec(`
		CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
		CREATE TABLE devices (
			name TEXT PRIMARY KEY,
			ip TEXT NOT NULL UNIQUE,
			kind TEXT NOT NULL CHECK (kind IN ('lighthouse', 'mac', 'mobile')),
			grp TEXT NOT NULL,
			pubkey TEXT NOT NULL,
			cert TEXT NOT NULL,
			fingerprint TEXT NOT NULL,
			not_after TEXT NOT NULL,
			created_at TEXT NOT NULL,
			revoked_at TEXT,
			last_seen TEXT
		);
		CREATE TABLE revoked_certs (
			fingerprint TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			revoked_at TEXT NOT NULL,
			reason TEXT NOT NULL
		);
	`)
	_ = rawDB.Close()
	if err != nil {
		t.Fatal(err)
	}

	reg, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open migration failed: %v", err)
	}
	defer func() { _ = reg.Close() }()

	if err := reg.Add(AddParams{
		Name: "laptop", IP: "10.213.0.2", Kind: "mac", Grp: "admin",
		Pubkey: "pk", Cert: "crt", Fingerprint: "fp1", NotAfter: "2030-01-01T00:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Touch("laptop", "go/abc1234"); err != nil {
		t.Fatal(err)
	}
	dev, err := reg.Get("laptop")
	if err != nil || dev == nil || dev.ClientVersion == nil || *dev.ClientVersion != "go/abc1234" {
		t.Fatalf("expected migrated client_version, got %+v err=%v", dev, err)
	}
}

func TestNextIPReplaceCertRevokeAndSettings(t *testing.T) {
	reg, err := Open(filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reg.Close() }()

	// NextIP skips 10.213.0.1 (lighthouse) and returns 10.213.0.2.
	ip1, err := reg.NextIP()
	if err != nil || ip1 != "10.213.0.2" {
		t.Fatalf("expected 10.213.0.2, got (%q, %v)", ip1, err)
	}
	if err := reg.Add(AddParams{
		Name: "mac1", IP: ip1, Kind: "mac", Grp: "managed",
		Pubkey: "pk1", Cert: "crt1", Fingerprint: "fp-old", NotAfter: "2030-01-01T00:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}

	// Revoke blocklists fingerprint and never reuses revoked IP.
	if err := reg.Revoke("mac1"); err != nil {
		t.Fatal(err)
	}
	dev, err := reg.Get("mac1")
	if err != nil || dev == nil || dev.Active() {
		t.Fatalf("expected revoked device, got %+v err=%v", dev, err)
	}
	ip2, err := reg.NextIP()
	if err != nil || ip2 != "10.213.0.3" {
		t.Fatalf("expected 10.213.0.3 after revoke, got (%q, %v)", ip2, err)
	}
	bl, err := reg.Blocklist()
	if err != nil || len(bl) != 1 || bl[0] != "fp-old" {
		t.Fatalf("unexpected blocklist: %v err=%v", bl, err)
	}

	// ReplaceCert clears revoked_at and updates cert fields atomically.
	if err := reg.ReplaceCert(ReplaceCertParams{
		Name: "mac1", Grp: "admin", Pubkey: "pk2", Cert: "crt2",
		Fingerprint: "fp-new", NotAfter: "2031-01-01T00:00:00Z", Reason: "re-enrolled",
	}); err != nil {
		t.Fatal(err)
	}
	dev, err = reg.Get("mac1")
	if err != nil || dev == nil || !dev.Active() || dev.Grp != "admin" || dev.Fingerprint != "fp-new" {
		t.Fatalf("expected active updated device after ReplaceCert, got %+v err=%v", dev, err)
	}

	// Missing setting returns ("", false, nil); SetSettings writes multiple keys transactionally.
	if _, ok, err := reg.Setting("missing"); err != nil || ok {
		t.Fatalf("expected missing setting false/nil, got ok=%v err=%v", ok, err)
	}
	if err := reg.SetSettings(map[string]string{"k1": "v1", "k2": "v2"}); err != nil {
		t.Fatal(err)
	}
	if err := reg.SetSettings(map[string]string{"k1": "v1b", "k3": "v3"}); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{"k1": "v1b", "k2": "v2", "k3": "v3"} {
		got, ok, err := reg.Setting(k)
		if err != nil || !ok || got != want {
			t.Fatalf("Setting(%q) = (%q, %v, %v), want %q", k, got, ok, err, want)
		}
	}
}

func TestPostCloseSafety(t *testing.T) {
	reg, err := Open(filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Close(); err != nil {
		t.Fatal(err)
	}
	if err := reg.Close(); err != nil {
		t.Fatalf("second Close should be no-op, got %v", err)
	}

	checkClosed := func(name string, err error) {
		t.Helper()
		if !errors.Is(err, errRegistryClosed) {
			t.Fatalf("%s after Close expected errRegistryClosed, got %v", name, err)
		}
	}
	_, _, err = reg.Setting("k")
	checkClosed("Setting", err)
	checkClosed("SetSetting", reg.SetSetting("k", "v"))
	checkClosed("SetSettings", reg.SetSettings(map[string]string{"k": "v"}))
	_, err = reg.Get("x")
	checkClosed("Get", err)
	_, err = reg.ByIP("10.213.0.1")
	checkClosed("ByIP", err)
	_, err = reg.Lighthouse()
	checkClosed("Lighthouse", err)
	_, err = reg.Devices()
	checkClosed("Devices", err)
	_, err = reg.NextIP()
	checkClosed("NextIP", err)
	checkClosed("Add", reg.Add(AddParams{Name: "x"}))
	checkClosed("ReplaceCert", reg.ReplaceCert(ReplaceCertParams{Name: "x"}))
	checkClosed("Revoke", reg.Revoke("x"))
	_, err = reg.Blocklist()
	checkClosed("Blocklist", err)
	_, err = reg.RevokedCerts()
	checkClosed("RevokedCerts", err)
	checkClosed("Touch", reg.Touch("x", "v"))
}
