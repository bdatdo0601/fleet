// Package registry manages the lighthouse SQLite device and settings database (fleet.db).
package registry

import (
	"database/sql"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver

	"github.com/bdatdo0601/fleet/internal/paths"
)

const schema = `
CREATE TABLE IF NOT EXISTS settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS devices (
  name           TEXT PRIMARY KEY,
  ip             TEXT NOT NULL UNIQUE,
  kind           TEXT NOT NULL CHECK (kind IN ('lighthouse', 'mac', 'mobile')),
  grp            TEXT NOT NULL,
  pubkey         TEXT NOT NULL,
  cert           TEXT NOT NULL,
  fingerprint    TEXT NOT NULL,
  not_after      TEXT NOT NULL,
  created_at     TEXT NOT NULL,
  revoked_at     TEXT,
  last_seen      TEXT,
  client_version TEXT
);
-- Every certificate ever replaced or revoked. Rendered into pki.blocklist everywhere.
CREATE TABLE IF NOT EXISTS revoked_certs (
  fingerprint TEXT PRIMARY KEY,
  name        TEXT NOT NULL,
  revoked_at  TEXT NOT NULL,
  reason      TEXT NOT NULL
);
`

// NowISO returns the current UTC timestamp formatted identically to Python's datetime.isoformat()
// with +00:00 suffix so lexicographic SQLite comparisons remain consistent across runtimes.
func NowISO() string {
	return time.Now().UTC().Truncate(time.Second).Format("2006-01-02T15:04:05+00:00")
}

// Device represents a registered mesh node in fleet.db.
type Device struct {
	Name          string
	IP            string
	Kind          string
	Grp           string
	Pubkey        string
	Cert          string
	Fingerprint   string
	NotAfter      string
	CreatedAt     string
	RevokedAt     *string
	LastSeen      *string
	ClientVersion *string
}

// Active reports whether the device has not been revoked.
func (d *Device) Active() bool {
	return d != nil && d.RevokedAt == nil
}

// AddParams holds parameters for inserting a new device.
type AddParams struct {
	Name        string
	IP          string
	Kind        string
	Grp         string
	Pubkey      string
	Cert        string
	Fingerprint string
	NotAfter    string
}

// ReplaceCertParams holds parameters for replacing a device's certificate.
type ReplaceCertParams struct {
	Name        string
	Grp         string
	Pubkey      string
	Cert        string
	Fingerprint string
	NotAfter    string
	Reason      string
}

// Registry wraps access to fleet.db.
type Registry struct {
	Path string
	mu   sync.Mutex
	db   *sql.DB
}

// ColumnMigration describes an additive column to ensure on an existing table.
type ColumnMigration struct {
	Col string
	Def string
}

// OpenSQLite opens a single-connection WAL SQLite database, initializes schema, and applies additive migrations.
func OpenSQLite(path, dbSchema, table string, migrations []ColumnMigration) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA foreign_keys = ON",
		"PRAGMA busy_timeout = 5000",
	} {
		if _, err := db.Exec(pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("sqlite %s: %w", pragma, err)
		}
	}
	if _, err := db.Exec(dbSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("init schema %s: %w", path, err)
	}
	if table != "" && len(migrations) > 0 {
		if err := migrateTable(db, table, migrations); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	return db, nil
}

func tableColumns(db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	cols := map[string]bool{}
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt *string
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return nil, err
		}
		cols[name] = true
	}
	return cols, rows.Err()
}

func migrateTable(db *sql.DB, table string, migrations []ColumnMigration) error {
	cols, err := tableColumns(db, table)
	if err != nil {
		return err
	}
	for _, m := range migrations {
		if !cols[m.Col] {
			if _, err := db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, m.Col, m.Def)); err != nil {
				return fmt.Errorf("migrate %s.%s: %w", table, m.Col, err)
			}
		}
	}
	return nil
}

// errRegistryClosed is returned when a Registry method is called after Close.
var errRegistryClosed = errors.New("registry is closed")

// Open opens or initializes fleet.db at path, applying additive schema migrations.
func Open(path string) (*Registry, error) {
	db, err := OpenSQLite(path, schema, "devices", []ColumnMigration{
		{Col: "client_version", Def: "TEXT"},
	})
	if err != nil {
		return nil, err
	}
	return &Registry{Path: path, db: db}, nil
}

// Close closes the underlying SQLite connection.
func (r *Registry) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.db == nil {
		return nil
	}
	err := r.db.Close()
	r.db = nil
	return err
}

// Setting retrieves a setting value by key.
func (r *Registry) Setting(key string) (string, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.db == nil {
		return "", false, errRegistryClosed
	}
	var val string
	err := r.db.QueryRow("SELECT value FROM settings WHERE key = ?", key).Scan(&val)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return val, true, nil
}

// SetSetting upserts a key/value setting.
func (r *Registry) SetSetting(key, value string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.db == nil {
		return errRegistryClosed
	}
	_, err := r.db.Exec(
		"INSERT INTO settings(key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
		key, value,
	)
	return err
}

// SetSettings upserts multiple key/value settings inside a single transaction.
func (r *Registry) SetSettings(kv map[string]string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.db == nil {
		return errRegistryClosed
	}
	if len(kv) == 0 {
		return nil
	}
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.Prepare(
		"INSERT INTO settings(key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
	)
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()
	for k, v := range kv {
		if _, err := stmt.Exec(k, v); err != nil {
			return err
		}
	}
	return tx.Commit()
}

const deviceSelectCols = "name, ip, kind, grp, pubkey, cert, fingerprint, not_after, created_at, revoked_at, last_seen, client_version"

func scanDevice(scanner interface{ Scan(...any) error }) (*Device, error) {
	var d Device
	err := scanner.Scan(
		&d.Name, &d.IP, &d.Kind, &d.Grp, &d.Pubkey, &d.Cert,
		&d.Fingerprint, &d.NotAfter, &d.CreatedAt,
		&d.RevokedAt, &d.LastSeen, &d.ClientVersion,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

func (r *Registry) getLocked(name string) (*Device, error) {
	return scanDevice(r.db.QueryRow("SELECT "+deviceSelectCols+" FROM devices WHERE name = ?", name))
}

// Get returns the device with the given name, or nil if not found.
func (r *Registry) Get(name string) (*Device, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.db == nil {
		return nil, errRegistryClosed
	}
	return r.getLocked(name)
}

// ByIP returns the device assigned the given mesh IP, or nil if not found.
func (r *Registry) ByIP(ip string) (*Device, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.db == nil {
		return nil, errRegistryClosed
	}
	return scanDevice(r.db.QueryRow("SELECT "+deviceSelectCols+" FROM devices WHERE ip = ?", ip))
}

// Lighthouse returns the lighthouse device row, or nil if missing.
func (r *Registry) Lighthouse() (*Device, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.db == nil {
		return nil, errRegistryClosed
	}
	return scanDevice(r.db.QueryRow("SELECT " + deviceSelectCols + " FROM devices WHERE kind = 'lighthouse'"))
}

// Devices returns all devices sorted numerically by mesh IP address.
func (r *Registry) Devices() ([]Device, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.db == nil {
		return nil, errRegistryClosed
	}
	rows, err := r.db.Query("SELECT " + deviceSelectCols + " FROM devices ORDER BY ip")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Device{}
	for rows.Next() {
		d, err := scanDevice(rows)
		if err != nil {
			return nil, err
		}
		if d != nil {
			out = append(out, *d)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		ai, err1 := netip.ParseAddr(out[i].IP)
		aj, err2 := netip.ParseAddr(out[j].IP)
		if err1 == nil && err2 == nil {
			return ai.Compare(aj) < 0
		}
		return out[i].IP < out[j].IP
	})
	return out, nil
}

func (r *Registry) usedIPsLocked() (map[string]bool, error) {
	rows, err := r.db.Query("SELECT ip FROM devices")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	used := map[string]bool{}
	for rows.Next() {
		var ip string
		if err := rows.Scan(&ip); err != nil {
			return nil, err
		}
		used[ip] = true
	}
	return used, rows.Err()
}

// NextIP returns the lowest unassigned host address in the mesh subnet.
// Addresses of revoked devices are never reused.
func (r *Registry) NextIP() (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.db == nil {
		return "", errRegistryClosed
	}
	used, err := r.usedIPsLocked()
	if err != nil {
		return "", err
	}

	prefix, err := netip.ParsePrefix(paths.MeshCIDR)
	if err != nil {
		return "", err
	}
	base := prefix.Masked().Addr().As4()
	baseNum := uint32(base[0])<<24 | uint32(base[1])<<16 | uint32(base[2])<<8 | uint32(base[3])
	hostBits := 32 - prefix.Bits()
	maxHost := (uint32(1) << hostBits) - 2 // exclude network (.0) and broadcast (.255.255)
	for offset := uint32(1); offset <= maxHost; offset++ {
		n := baseNum + offset
		addr := netip.AddrFrom4([4]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)})
		ip := addr.String()
		if ip != paths.LighthouseIP && !used[ip] {
			return ip, nil
		}
	}
	return "", fmt.Errorf("mesh address space exhausted")
}

// Add inserts a new device record.
func (r *Registry) Add(p AddParams) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.db == nil {
		return errRegistryClosed
	}
	_, err := r.db.Exec(
		"INSERT INTO devices(name, ip, kind, grp, pubkey, cert, fingerprint, not_after, created_at) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
		p.Name, p.IP, p.Kind, p.Grp, p.Pubkey, p.Cert, p.Fingerprint, p.NotAfter, NowISO(),
	)
	return err
}

// ReplaceCert assigns a new certificate to an existing device, blocklisting the old one
// and clearing revoked_at inside a single transaction.
func (r *Registry) ReplaceCert(p ReplaceCertParams) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.db == nil {
		return errRegistryClosed
	}
	old, err := r.getLocked(p.Name)
	if err != nil {
		return err
	}
	if old == nil {
		return fmt.Errorf("no device named %s", p.Name)
	}
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := NowISO()
	if _, err := tx.Exec(
		"INSERT OR IGNORE INTO revoked_certs(fingerprint, name, revoked_at, reason) VALUES (?, ?, ?, ?)",
		old.Fingerprint, p.Name, now, p.Reason,
	); err != nil {
		return err
	}
	if _, err := tx.Exec(
		"UPDATE devices SET grp = ?, pubkey = ?, cert = ?, fingerprint = ?, not_after = ?, revoked_at = NULL WHERE name = ?",
		p.Grp, p.Pubkey, p.Cert, p.Fingerprint, p.NotAfter, p.Name,
	); err != nil {
		return err
	}
	return tx.Commit()
}

// Revoke blocklists a device's current certificate and marks the device revoked.
func (r *Registry) Revoke(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.db == nil {
		return errRegistryClosed
	}
	dev, err := r.getLocked(name)
	if err != nil {
		return err
	}
	if dev == nil {
		return fmt.Errorf("no device named %s", name)
	}
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := NowISO()
	if _, err := tx.Exec(
		"INSERT OR IGNORE INTO revoked_certs(fingerprint, name, revoked_at, reason) VALUES (?, ?, ?, ?)",
		dev.Fingerprint, name, now, "revoked",
	); err != nil {
		return err
	}
	if _, err := tx.Exec("UPDATE devices SET revoked_at = ? WHERE name = ?", now, name); err != nil {
		return err
	}
	return tx.Commit()
}

// Blocklist returns all revoked certificate fingerprints sorted ascending.
func (r *Registry) Blocklist() ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.db == nil {
		return nil, errRegistryClosed
	}
	rows, err := r.db.Query("SELECT fingerprint FROM revoked_certs ORDER BY fingerprint")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	fps := []string{}
	for rows.Next() {
		var fp string
		if err := rows.Scan(&fp); err != nil {
			return nil, err
		}
		fps = append(fps, fp)
	}
	return fps, rows.Err()
}

// RevokedCert represents an entry in the PKI blocklist table.
type RevokedCert struct {
	Fingerprint string `json:"fingerprint"`
	Name        string `json:"name"`
	RevokedAt   string `json:"revoked_at"`
	Reason      string `json:"reason"`
}

// RevokedCerts returns all revoked or replaced certificates sorted newest first.
func (r *Registry) RevokedCerts() ([]RevokedCert, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.db == nil {
		return nil, errRegistryClosed
	}
	rows, err := r.db.Query(
		"SELECT fingerprint, name, revoked_at, reason FROM revoked_certs ORDER BY revoked_at DESC, fingerprint ASC",
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []RevokedCert{}
	for rows.Next() {
		var rc RevokedCert
		if err := rows.Scan(&rc.Fingerprint, &rc.Name, &rc.RevokedAt, &rc.Reason); err != nil {
			return nil, err
		}
		out = append(out, rc)
	}
	return out, rows.Err()
}

// Touch updates last_seen (and client_version when non-empty) on a device.
func (r *Registry) Touch(name, clientVersion string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.db == nil {
		return errRegistryClosed
	}
	now := NowISO()
	if clientVersion != "" {
		_, err := r.db.Exec(
			"UPDATE devices SET last_seen = ?, client_version = ? WHERE name = ?",
			now, clientVersion, name,
		)
		return err
	}
	_, err := r.db.Exec("UPDATE devices SET last_seen = ? WHERE name = ?", now, name)
	return err
}
