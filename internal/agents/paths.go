package agents

import (
	"cmp"
	"database/sql"
	"errors"
	"path"
	"sort"
	"strings"

	"github.com/bdatdo0601/fleet/internal/registry"
)

// TrackedPath represents a known working directory and optional git repository observed on a fleet device.
type TrackedPath struct {
	Device    string `json:"device"`
	Path      string `json:"path"`
	TildePath string `json:"tilde_path,omitempty"`
	Subpath   string `json:"subpath,omitempty"`
	Repo      string `json:"repo"`
	Source    string `json:"source"`
	UseCount  int    `json:"use_count"`
	FirstSeen string `json:"first_seen"`
	LastSeen  string `json:"last_seen"`
}

type sqlExecQuerier interface {
	Exec(string, ...any) (sql.Result, error)
	Query(string, ...any) (*sql.Rows, error)
	QueryRow(string, ...any) *sql.Row
}

// NormalizeTrackedPath cleans an absolute or ~/ directory path, returning "" for empty, root, or relative paths.
func NormalizeTrackedPath(raw string) string {
	p := strings.TrimSpace(raw)
	if p == "" || p == "/" {
		return ""
	}
	if p == "~" {
		return "~"
	}
	if strings.HasPrefix(p, "~/") {
		rest := path.Clean(p[2:])
		if rest == "." {
			return "~"
		}
		if rest == ".." || strings.HasPrefix(rest, "../") {
			return ""
		}
		return "~/" + rest
	}
	if strings.HasPrefix(p, "/") {
		cleaned := path.Clean(p)
		if cleaned == "/" {
			return ""
		}
		return cleaned
	}
	return ""
}

// ToTildePath converts /Users/<user>/... or /home/<user>/... (or ~/...) to ~/..., returning "" otherwise.
func ToTildePath(raw string) string {
	p := NormalizeTrackedPath(raw)
	if p == "" {
		return ""
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		return p
	}
	for _, prefix := range []string{"/Users/", "/home/"} {
		if rem, ok := strings.CutPrefix(p, prefix); ok {
			if _, after, ok := strings.Cut(rem, "/"); ok && after != "" {
				return "~/" + after
			}
			if rem != "" && !strings.Contains(rem, "/") {
				return "~"
			}
		}
	}
	return ""
}

// WorkspaceSubpath returns rawPath relative to workspaceDir if rawPath is strictly inside workspaceDir.
func WorkspaceSubpath(workspaceDir, rawPath string) string {
	ws := NormalizeTrackedPath(workspaceDir)
	p := NormalizeTrackedPath(rawPath)
	if ws == "" || p == "" {
		return ""
	}
	if ws == "~" {
		t := ToTildePath(p)
		if after, ok := strings.CutPrefix(t, "~/"); ok {
			return after
		}
		return ""
	}
	if strings.HasPrefix(ws, "~/") {
		t := ToTildePath(p)
		if after, ok := strings.CutPrefix(t, ws+"/"); t != "" && ok {
			return after
		}
		return ""
	}
	if strings.HasPrefix(ws, "/") {
		if after, ok := strings.CutPrefix(p, ws+"/"); ok {
			return after
		}
		if wsTilde := ToTildePath(ws); wsTilde != "" {
			if after, ok := strings.CutPrefix(p, wsTilde+"/"); ok {
				return after
			}
		}
	}
	return ""
}

// RepoNameFromPath derives a repository/directory basename from an absolute or ~/ path.
func RepoNameFromPath(raw string) string {
	norm := NormalizeTrackedPath(raw)
	if norm == "" || norm == "~" {
		return ""
	}
	b := path.Base(norm)
	if b == "." || b == "/" || b == "~" {
		return ""
	}
	return b
}

func findAbsPathForTildeLocked(eq sqlExecQuerier, device, tildePath string) (string, error) {
	rows, err := eq.Query(`SELECT path FROM agent_paths WHERE device = ? AND path LIKE '/%'`, device)
	if err != nil {
		return "", err
	}
	defer func() { _ = rows.Close() }()
	var match string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return "", err
		}
		if ToTildePath(p) == tildePath {
			match = p
			break
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return match, nil
}

func upgradeTildeRowLocked(eq sqlExecQuerier, device, absPath, tildePath, repo, source, nowISO string, bump int) (bool, error) {
	var tildeFirst string
	var tildeRepo, tildeSource string
	var tildeCount int
	err := eq.QueryRow(
		`SELECT repo, source, use_count, first_seen FROM agent_paths WHERE device = ? AND path = ?`,
		device, tildePath,
	).Scan(&tildeRepo, &tildeSource, &tildeCount, &tildeFirst)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	effRepo := cmp.Or(repo, tildeRepo)
	effSource := cmp.Or(source, tildeSource)
	var exists int
	if err := eq.QueryRow(`SELECT COUNT(*) FROM agent_paths WHERE device = ? AND path = ?`, device, absPath).Scan(&exists); err != nil {
		return false, err
	}
	if exists == 0 {
		if _, err := eq.Exec(
			`UPDATE agent_paths
			 SET path = ?, repo = ?, source = ?, use_count = use_count + ?, last_seen = ?
			 WHERE device = ? AND path = ?`,
			absPath, effRepo, effSource, bump, nowISO, device, tildePath,
		); err != nil {
			return false, err
		}
		return true, nil
	}
	if _, err := eq.Exec(`DELETE FROM agent_paths WHERE device = ? AND path = ?`, device, tildePath); err != nil {
		return false, err
	}
	if _, err := eq.Exec(
		`UPDATE agent_paths
		 SET repo = CASE WHEN ? != '' THEN ? ELSE repo END,
		     source = CASE WHEN ? != '' THEN ? ELSE source END,
		     use_count = use_count + ? + ?,
		     first_seen = CASE WHEN ? != '' AND ? < first_seen THEN ? ELSE first_seen END,
		     last_seen = ?
		 WHERE device = ? AND path = ?`,
		effRepo, effRepo, effSource, effSource, tildeCount, bump, tildeFirst, tildeFirst, tildeFirst, nowISO, device, absPath,
	); err != nil {
		return false, err
	}
	return true, nil
}

func recordPathExecLocked(eq sqlExecQuerier, device, rawPath, repo, source, nowISO string, bumpCount bool) error {
	dev := strings.TrimSpace(device)
	norm := NormalizeTrackedPath(rawPath)
	if dev == "" || norm == "" {
		return nil
	}
	repo = strings.TrimSpace(repo)
	source = cmp.Or(strings.TrimSpace(source), "manual")
	bump := 0
	if bumpCount {
		bump = 1
	}

	if norm == "~" || strings.HasPrefix(norm, "~/") {
		if absMatch, err := findAbsPathForTildeLocked(eq, dev, norm); err != nil {
			return err
		} else if absMatch != "" {
			norm = absMatch
		}
	} else if tilde := ToTildePath(norm); tilde != "" {
		if upgraded, err := upgradeTildeRowLocked(eq, dev, norm, tilde, repo, source, nowISO, bump); err != nil {
			return err
		} else if upgraded {
			return nil
		}
	}

	_, err := eq.Exec(
		`INSERT INTO agent_paths(device, path, repo, source, use_count, first_seen, last_seen)
		 VALUES (?, ?, ?, ?, 1, ?, ?)
		 ON CONFLICT(device, path) DO UPDATE SET
		   repo = CASE WHEN excluded.repo != '' THEN excluded.repo ELSE agent_paths.repo END,
		   source = CASE WHEN excluded.source != '' THEN excluded.source ELSE agent_paths.source END,
		   use_count = agent_paths.use_count + ?,
		   last_seen = CASE WHEN excluded.last_seen > agent_paths.last_seen THEN excluded.last_seen ELSE agent_paths.last_seen END`,
		dev, norm, repo, source, nowISO, nowISO, bump,
	)
	return err
}

func loadSessionPathSeeds(db *sql.DB) ([]struct{ device, cwd, repo, startedAt, updatedAt string }, error) {
	rows, err := db.Query(
		`SELECT device, cwd, repo, started_at, updated_at
		 FROM agent_sessions
		 WHERE cwd != '' AND cwd != '/'
		 ORDER BY updated_at ASC`,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var seeds []struct{ device, cwd, repo, startedAt, updatedAt string }
	for rows.Next() {
		var r struct{ device, cwd, repo, startedAt, updatedAt string }
		if err := rows.Scan(&r.device, &r.cwd, &r.repo, &r.startedAt, &r.updatedAt); err != nil {
			return nil, err
		}
		seeds = append(seeds, r)
	}
	return seeds, rows.Err()
}

func backfillPathsFromSessions(db *sql.DB) error {
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM agent_paths`).Scan(&count); err != nil || count > 0 {
		return err
	}
	seeds, err := loadSessionPathSeeds(db)
	if err != nil {
		return err
	}
	for _, s := range seeds {
		ts := cmp.Or(s.updatedAt, s.startedAt, registry.NowISO())
		if err := recordPathExecLocked(db, s.device, s.cwd, s.repo, "heartbeat", ts, true); err != nil {
			return err
		}
	}
	return nil
}

// RecordPath records or updates a tracked directory/repository on device.
func (h *Hub) RecordPath(device, rawPath, repo, source string, bumpCount bool) error {
	norm := NormalizeTrackedPath(rawPath)
	if strings.TrimSpace(device) == "" || norm == "" {
		return nil
	}
	nowISO := registry.NowISO()
	h.mu.Lock()
	defer h.mu.Unlock()
	return recordPathExecLocked(h.db, device, norm, repo, source, nowISO, bumpCount)
}

// ListPaths returns tracked directories and repositories ordered by most recently seen.
func (h *Hub) ListPaths(device, query string) ([]TrackedPath, error) {
	dev := strings.TrimSpace(device)
	q := strings.ToLower(strings.TrimSpace(query))

	var where []string
	var args []any
	if dev != "" {
		where = append(where, "device = ?")
		args = append(args, dev)
	}
	if q != "" {
		like := "%" + q + "%"
		where = append(where, "(LOWER(path) LIKE ? OR LOWER(repo) LIKE ? OR LOWER(device) LIKE ?)")
		args = append(args, like, like, like)
	}
	sqlStr := `SELECT device, path, repo, source, use_count, first_seen, last_seen FROM agent_paths`
	if len(where) > 0 {
		sqlStr += ` WHERE ` + strings.Join(where, " AND ")
	}
	sqlStr += ` ORDER BY last_seen DESC, use_count DESC, path ASC`

	h.mu.Lock()
	defer h.mu.Unlock()
	rows, err := h.db.Query(sqlStr, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := []TrackedPath{}
	for rows.Next() {
		var p TrackedPath
		if err := rows.Scan(&p.Device, &p.Path, &p.Repo, &p.Source, &p.UseCount, &p.FirstSeen, &p.LastSeen); err != nil {
			return nil, err
		}
		p.TildePath = ToTildePath(p.Path)
		out = append(out, p)
	}
	return out, rows.Err()
}

// DeletePath removes a tracked path on device (matching exact path or its ~/ equivalent).
func (h *Hub) DeletePath(device, rawPath string) (bool, error) {
	dev := strings.TrimSpace(device)
	norm := NormalizeTrackedPath(rawPath)
	if dev == "" || norm == "" {
		return false, newAgentErr("device and valid path are required", 400)
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	res, err := h.db.Exec(`DELETE FROM agent_paths WHERE device = ? AND path = ?`, dev, norm)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		return true, nil
	}
	if norm == "~" || strings.HasPrefix(norm, "~/") {
		if absMatch, mErr := findAbsPathForTildeLocked(h.db, dev, norm); mErr != nil {
			return false, mErr
		} else if absMatch != "" {
			res2, err := h.db.Exec(`DELETE FROM agent_paths WHERE device = ? AND path = ?`, dev, absMatch)
			if err != nil {
				return false, err
			}
			n2, _ := res2.RowsAffected()
			return n2 > 0, nil
		}
	}
	return false, nil
}

// KnownRepos returns sorted distinct non-empty repository names across agent_paths and agent_sessions.
func (h *Hub) KnownRepos(device string) ([]string, error) {
	dev := strings.TrimSpace(device)
	h.mu.Lock()
	defer h.mu.Unlock()

	seen := map[string]bool{}
	collect := func(q string, args ...any) error {
		rows, err := h.db.Query(q, args...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var r string
			if err := rows.Scan(&r); err != nil {
				return err
			}
			if r = strings.TrimSpace(r); r != "" {
				seen[r] = true
			}
		}
		return rows.Err()
	}

	if dev != "" {
		if err := collect(`SELECT DISTINCT repo FROM agent_paths WHERE device = ? AND repo != ''`, dev); err != nil {
			return nil, err
		}
		if err := collect(`SELECT DISTINCT repo FROM agent_sessions WHERE device = ? AND repo != ''`, dev); err != nil {
			return nil, err
		}
	} else {
		if err := collect(`SELECT DISTINCT repo FROM agent_paths WHERE repo != ''`); err != nil {
			return nil, err
		}
		if err := collect(`SELECT DISTINCT repo FROM agent_sessions WHERE repo != ''`); err != nil {
			return nil, err
		}
	}

	repos := make([]string, 0, len(seen))
	for r := range seen {
		repos = append(repos, r)
	}
	sort.Strings(repos)
	return repos, nil
}
