package memory

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bdatdo0601/fleet/internal/client"
	"github.com/bdatdo0601/fleet/internal/paths"
	"github.com/bdatdo0601/fleet/internal/registry"
)

const (
	maxHarvestBatchItems = 20
	maxHarvestBatchBytes = 160 * 1024
)

// ParsedMemory holds metadata and content parsed from a local Claude/Codex memory file.
type ParsedMemory struct {
	Key         string
	Kind        string
	Summary     string
	Body        string
	SourceMtime string
	OriginPath  string
	SHA256      string
}

// HarvestOpts configures HarvestLocal.
type HarvestOpts struct {
	Home     string
	CacheDir string
	APIURL   string
	Device   string
	UID      *int
	GID      *int
	Force    bool
	DryRun   bool
}

// HarvestItem previews a scanned local memory entry.
type HarvestItem struct {
	Scope      string `json:"scope"`
	Key        string `json:"key"`
	Kind       string `json:"kind"`
	Summary    string `json:"summary"`
	OriginPath string `json:"origin_path"`
}

// HarvestReport summarizes a local-to-lighthouse memory backup pass.
type HarvestReport struct {
	Scanned   int           `json:"scanned"`
	Skipped   int           `json:"skipped"`
	Pending   int           `json:"pending"`
	Created   int           `json:"created"`
	Updated   int           `json:"updated"`
	Unchanged int           `json:"unchanged"`
	Protected int           `json:"protected"`
	Stale     int           `json:"stale"`
	Queued    int           `json:"queued"`
	Items     []HarvestItem `json:"items,omitempty"`
}

type harvestedStateEntry struct {
	SHA256 string `json:"sha256"`
	Scope  string `json:"scope"`
	Key    string `json:"key"`
}

type cachedContextEntry struct {
	Rendered  string `json:"rendered"`
	UpdatedAt string `json:"updated_at"`
}

func runGit(cwd string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	cmdArgs := append([]string{"-C", cwd}, args...)
	out, err := exec.CommandContext(ctx, "git", cmdArgs...).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func repoNameFromRemoteURL(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.TrimRight(s, "/")
	s = strings.TrimSuffix(s, ".git")
	if s == "" {
		return ""
	}
	// Handle SCP-style git@host:org/repo as well as https://host/org/repo.
	if idx := strings.LastIndexAny(s, "/:"); idx >= 0 && idx+1 < len(s) {
		s = s[idx+1:]
	}
	return NormalizeSlug(s, 80)
}

// CanonicalRepo resolves cwd to a canonical git repository slug so worktrees and checkouts share one scope.
func CanonicalRepo(cwd string) string {
	if cwd == "" {
		return ""
	}
	info, err := os.Stat(cwd)
	if err != nil || !info.IsDir() {
		return NormalizeSlug(filepath.Base(cwd), 80)
	}
	if remote, err := runGit(cwd, "remote", "get-url", "origin"); err == nil {
		if slug := repoNameFromRemoteURL(remote); slug != "" {
			return slug
		}
	}
	if commonDir, err := runGit(cwd, "rev-parse", "--git-common-dir"); err == nil && commonDir != "" {
		if !filepath.IsAbs(commonDir) {
			commonDir = filepath.Join(cwd, commonDir)
		}
		commonDir = filepath.Clean(commonDir)
		base := filepath.Base(commonDir)
		if base == ".git" {
			return NormalizeSlug(filepath.Base(filepath.Dir(commonDir)), 80)
		}
		return NormalizeSlug(strings.TrimSuffix(base, ".git"), 80)
	}
	if top, err := runGit(cwd, "rev-parse", "--show-toplevel"); err == nil && top != "" {
		return NormalizeSlug(filepath.Base(top), 80)
	}
	return NormalizeSlug(filepath.Base(cwd), 80)
}

func unquoteYAML(val string) string {
	v := strings.TrimSpace(val)
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			inner := v[1 : len(v)-1]
			if v[0] == '"' {
				inner = strings.ReplaceAll(inner, `\"`, `"`)
				inner = strings.ReplaceAll(inner, `\\`, `\`)
			}
			return strings.TrimSpace(inner)
		}
	}
	return v
}

func parseFrontmatterLines(fmLines []string) (name, desc, kind, modified string) {
	inMeta := false
	var topType, metaType, topMod, metaMod string

	for _, raw := range fmLines {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indented := strings.HasPrefix(raw, " ") || strings.HasPrefix(raw, "\t")
		if !indented {
			inMeta = false
		}
		colon := strings.IndexByte(trimmed, ':')
		if colon <= 0 {
			continue
		}
		k := strings.ToLower(strings.TrimSpace(trimmed[:colon]))
		v := unquoteYAML(trimmed[colon+1:])

		if !indented && k == "metadata" && v == "" {
			inMeta = true
			continue
		}
		if inMeta {
			switch k {
			case "type":
				metaType = v
			case "modified":
				metaMod = v
			}
			continue
		}
		switch k {
		case "name":
			name = v
		case "description":
			desc = v
		case "type":
			topType = v
		case "modified":
			topMod = v
		}
	}
	kind = cmp.Or(metaType, topType)
	modified = cmp.Or(metaMod, topMod)
	return name, desc, kind, modified
}

func splitFrontmatter(raw string) ([]string, string) {
	norm := strings.ReplaceAll(raw, "\r\n", "\n")
	lines := strings.Split(norm, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return nil, strings.TrimSpace(norm)
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			return lines[1:i], strings.TrimSpace(strings.Join(lines[i+1:], "\n"))
		}
	}
	return nil, strings.TrimSpace(norm)
}

func trimBodyToMaxBytes(body string) string {
	if len(body) <= MaxBodyBytes {
		return body
	}
	const suffix = "\n\n[truncated at 32KB]"
	limit := MaxBodyBytes - len(suffix)
	if limit <= 0 {
		return ""
	}
	cut := body[:limit]
	for len(cut) > 0 && (cut[len(cut)-1]&0xc0) == 0x80 {
		cut = cut[:len(cut)-1]
	}
	return strings.TrimSpace(cut) + suffix
}

// ParseClaudeMemoryFile parses a Claude/Codex markdown memory file and its YAML frontmatter.
func ParseClaudeMemoryFile(path string) (*ParsedMemory, error) {
	if strings.EqualFold(filepath.Base(path), "MEMORY.md") {
		return nil, nil
	}
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if fi.IsDir() {
		return nil, nil
	}
	rawBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	fmLines, body := splitFrontmatter(string(rawBytes))
	body = trimBodyToMaxBytes(strings.TrimSpace(body))
	if body == "" {
		return nil, nil
	}

	fmName, fmDesc, fmKind, fmModified := parseFrontmatterLines(fmLines)
	baseName := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	key, err := NormalizeKey(cmp.Or(fmName, baseName))
	if err != nil {
		return nil, err
	}
	kind, err := NormalizeKind(fmKind)
	if err != nil {
		kind = "project"
	}
	summary := DeriveSummary(fmDesc, body)
	mtime := strings.TrimSpace(fmModified)
	if mtime == "" {
		mtime = fi.ModTime().UTC().Truncate(time.Second).Format("2006-01-02T15:04:05+00:00")
	}

	return &ParsedMemory{
		Key:         key,
		Kind:        kind,
		Summary:     summary,
		Body:        body,
		SourceMtime: mtime,
		OriginPath:  path,
		SHA256:      ContentHash(summary, body),
	}, nil
}

func cwdFromJSONL(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()

	r := bufio.NewReader(f)
	for range 40 {
		ln, err := r.ReadBytes('\n')
		if bytes.Contains(ln, []byte(`"cwd"`)) {
			var obj struct {
				CWD     string `json:"cwd"`
				Payload *struct {
					CWD string `json:"cwd"`
				} `json:"payload"`
			}
			if json.Unmarshal(bytes.TrimSpace(ln), &obj) == nil {
				if c := strings.TrimSpace(cmp.Or(obj.CWD, "")); c != "" {
					return c
				}
				if obj.Payload != nil && strings.TrimSpace(obj.Payload.CWD) != "" {
					return strings.TrimSpace(obj.Payload.CWD)
				}
			}
		}
		if err != nil {
			break
		}
	}
	return ""
}

func resolveRepoFromCWDOrParent(cwd string) string {
	for _, marker := range []string{"/.claude/worktrees/", "/.worktrees/"} {
		if idx := strings.Index(cwd, marker); idx > 0 {
			parent := cwd[:idx]
			if slug := CanonicalRepo(parent); slug != "" {
				return slug
			}
		}
	}
	return CanonicalRepo(cwd)
}

func resolveClaudeProjectRepo(projDir string) string {
	entries, err := os.ReadDir(projDir)
	if err == nil {
		type jsonlFile struct {
			path  string
			mtime time.Time
		}
		var files []jsonlFile
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
				continue
			}
			if fi, fiErr := e.Info(); fiErr == nil {
				files = append(files, jsonlFile{path: filepath.Join(projDir, e.Name()), mtime: fi.ModTime()})
			}
		}
		sort.Slice(files, func(i, j int) bool { return files[i].mtime.After(files[j].mtime) })
		if len(files) > 5 {
			files = files[:5]
		}
		for _, jf := range files {
			if cwd := cwdFromJSONL(jf.path); cwd != "" {
				if slug := resolveRepoFromCWDOrParent(cwd); slug != "" {
					return slug
				}
			}
		}
	}

	// Fallback: derive repo from Claude's encoded directory name.
	base := filepath.Base(projDir)
	if idx := strings.Index(base, "--claude-worktrees-"); idx > 0 {
		base = base[:idx]
	}
	if _, after, ok := strings.Cut(base, "-workspace-"); ok && after != "" {
		return NormalizeSlug(after, 80)
	}
	return NormalizeSlug(strings.TrimLeft(base, "-"), 80)
}

func loadHarvestState(path string) map[string]harvestedStateEntry {
	out := map[string]harvestedStateEntry{}
	raw, err := os.ReadFile(path)
	if err != nil || len(bytes.TrimSpace(raw)) == 0 {
		return out
	}
	_ = json.Unmarshal(raw, &out)
	if out == nil {
		return map[string]harvestedStateEntry{}
	}
	return out
}

func saveHarvestState(path string, state map[string]harvestedStateEntry, uid, gid *int) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	_, err = paths.WriteFileAtomic(path, append(data, '\n'), 0o600, uid, gid)
	return err
}

type harvestedUpload struct {
	params UpsertParams
	sha    string
}

func scanClaudeMemories(home string) []harvestedUpload {
	var out []harvestedUpload
	claudeProjectsDir := filepath.Join(home, ".claude", "projects")
	projEntries, err := os.ReadDir(claudeProjectsDir)
	if err != nil {
		return nil
	}
	for _, pe := range projEntries {
		if !pe.IsDir() {
			continue
		}
		projDir := filepath.Join(claudeProjectsDir, pe.Name())
		memDir := filepath.Join(projDir, "memory")
		memEntries, mErr := os.ReadDir(memDir)
		if mErr != nil {
			continue
		}
		repo := resolveClaudeProjectRepo(projDir)
		scope := "global"
		if repo != "" {
			if ns, sErr := NormalizeScope("repo:" + repo); sErr == nil {
				scope = ns
			}
		}
		for _, me := range memEntries {
			if me.IsDir() || !strings.HasSuffix(strings.ToLower(me.Name()), ".md") {
				continue
			}
			fullPath := filepath.Join(memDir, me.Name())
			pm, pErr := ParseClaudeMemoryFile(fullPath)
			if pErr != nil || pm == nil {
				continue
			}
			out = append(out, harvestedUpload{
				params: UpsertParams{
					Scope:       scope,
					Key:         pm.Key,
					Kind:        pm.Kind,
					Summary:     pm.Summary,
					Body:        pm.Body,
					Source:      "claude_backup",
					Harness:     "claude",
					OriginPath:  pm.OriginPath,
					SourceMtime: pm.SourceMtime,
				},
				sha: pm.SHA256,
			})
		}
	}
	return out
}

func scanCodexMemories(home string) []harvestedUpload {
	var out []harvestedUpload
	codexMemDir := filepath.Join(home, ".codex", "memories")
	codexEntries, err := os.ReadDir(codexMemDir)
	if err != nil {
		return nil
	}
	for _, ce := range codexEntries {
		if ce.IsDir() || !strings.HasSuffix(strings.ToLower(ce.Name()), ".md") {
			continue
		}
		fullPath := filepath.Join(codexMemDir, ce.Name())
		pm, pErr := ParseClaudeMemoryFile(fullPath)
		if pErr != nil || pm == nil {
			continue
		}
		out = append(out, harvestedUpload{
			params: UpsertParams{
				Scope:       "global",
				Key:         pm.Key,
				Kind:        pm.Kind,
				Summary:     pm.Summary,
				Body:        pm.Body,
				Source:      "codex_backup",
				Harness:     "codex",
				OriginPath:  pm.OriginPath,
				SourceMtime: pm.SourceMtime,
			},
			sha: pm.SHA256,
		})
	}
	return out
}

func scanHarnessMemories(home string) []harvestedUpload {
	out := scanClaudeMemories(home)
	out = append(out, scanCodexMemories(home)...)
	return out
}

func chunkHarvestUploads(uploads []harvestedUpload) [][]harvestedUpload {
	var batches [][]harvestedUpload
	var cur []harvestedUpload
	curBytes := 32

	flush := func() {
		if len(cur) > 0 {
			batches = append(batches, cur)
			cur = nil
			curBytes = 32
		}
	}

	for _, u := range uploads {
		b, err := json.Marshal(u.params)
		if err != nil {
			continue
		}
		itemBytes := len(b) + 2
		if len(cur) >= maxHarvestBatchItems || (len(cur) > 0 && curBytes+itemBytes > maxHarvestBatchBytes) {
			flush()
		}
		cur = append(cur, u)
		curBytes += itemBytes
	}
	flush()
	return batches
}

// HarvestLocal scans local Claude/Codex memory files and uploads changed entries to the lighthouse.
func HarvestLocal(opts HarvestOpts) (HarvestReport, error) {
	home := opts.Home
	if home == "" {
		var err error
		if home, err = os.UserHomeDir(); err != nil {
			return HarvestReport{}, err
		}
	}
	cacheDir := cmp.Or(opts.CacheDir, paths.CacheDir())
	statePath := filepath.Join(cacheDir, "harvest-state.json")
	state := loadHarvestState(statePath)

	scanned := scanHarnessMemories(home)

	rep := HarvestReport{Scanned: len(scanned)}
	var toUpload []harvestedUpload
	for _, u := range scanned {
		if opts.Device != "" && u.params.Device == "" {
			u.params.Device = opts.Device
		}
		prev, ok := state[u.params.OriginPath]
		if !opts.Force && ok && prev.SHA256 == u.sha && prev.Scope == u.params.Scope && prev.Key == u.params.Key {
			rep.Skipped++
			continue
		}
		toUpload = append(toUpload, u)
		rep.Items = append(rep.Items, HarvestItem{
			Scope:      u.params.Scope,
			Key:        u.params.Key,
			Kind:       u.params.Kind,
			Summary:    u.params.Summary,
			OriginPath: u.params.OriginPath,
		})
	}
	rep.Pending = len(toUpload)
	if opts.DryRun || len(toUpload) == 0 {
		return rep, nil
	}

	api, err := client.ResolveAPI(opts.APIURL)
	if err != nil {
		return rep, err
	}

	for _, batch := range chunkHarvestUploads(toUpload) {
		items := make([]UpsertParams, len(batch))
		for i, u := range batch {
			items[i] = u.params
		}
		var batchRes SyncBatchResult
		if err := client.Call(api, "POST", "/v1/memory/sync", map[string]any{"items": items}, 30*time.Second, nil, &batchRes); err != nil {
			return rep, err
		}
		rep.Created += batchRes.Created
		rep.Updated += batchRes.Updated
		rep.Unchanged += batchRes.Unchanged
		rep.Protected += batchRes.Protected
		rep.Stale += batchRes.Stale
		rep.Queued += batchRes.Queued

		for _, u := range batch {
			state[u.params.OriginPath] = harvestedStateEntry{
				SHA256: u.sha,
				Scope:  u.params.Scope,
				Key:    u.params.Key,
			}
		}
		if err := saveHarvestState(statePath, state, opts.UID, opts.GID); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

// HarvestForOwner runs HarvestLocal and refreshes the global context cache for owner during root `fleet sync`.
func HarvestForOwner(owner, apiURL string) (HarvestReport, error) {
	if owner == "" || owner == "root" {
		return HarvestReport{}, nil
	}
	home := paths.HomeForUser(owner)
	if home == "" {
		return HarvestReport{}, fmt.Errorf("lookup user %q failed", owner)
	}
	uid, gid, err := paths.LookupUIDGID(owner)
	if err != nil {
		return HarvestReport{}, err
	}
	cacheDir := paths.CacheDirForOwner(owner)
	rep, hErr := HarvestLocal(HarvestOpts{
		Home:     home,
		CacheDir: cacheDir,
		APIURL:   apiURL,
		UID:      &uid,
		GID:      &gid,
	})
	if api, aErr := client.ResolveAPI(apiURL); aErr == nil {
		var ctxRes ContextResult
		if cErr := client.Call(api, "GET", "/v1/memory/context", nil, 10*time.Second, nil, &ctxRes); cErr == nil && ctxRes.Rendered != "" {
			_ = SaveContextCache(cacheDir, "", ctxRes.Rendered, &uid, &gid)
		}
	}
	return rep, hErr
}

func contextCacheKey(repo string) string {
	return cmp.Or(NormalizeSlug(repo, 80), "global")
}

// SaveContextCache stores the latest rendered SessionStart memory block in the local offline cache.
func SaveContextCache(cacheDir, repo, rendered string, uid, gid *int) error {
	if rendered == "" {
		return nil
	}
	if cacheDir == "" {
		cacheDir = paths.CacheDir()
	}
	path := filepath.Join(cacheDir, "memory-context.json")
	entries := map[string]cachedContextEntry{}
	if raw, err := os.ReadFile(path); err == nil && len(bytes.TrimSpace(raw)) > 0 {
		_ = json.Unmarshal(raw, &entries)
	}
	if entries == nil {
		entries = map[string]cachedContextEntry{}
	}
	entries[contextCacheKey(repo)] = cachedContextEntry{
		Rendered:  rendered,
		UpdatedAt: registry.NowISO(),
	}
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	_, err = paths.WriteFileAtomic(path, append(data, '\n'), 0o600, uid, gid)
	return err
}

// LoadContextCache reads the cached SessionStart memory block for repo (falling back to global).
func LoadContextCache(cacheDir, repo string) string {
	if cacheDir == "" {
		cacheDir = paths.CacheDir()
	}
	raw, err := os.ReadFile(filepath.Join(cacheDir, "memory-context.json"))
	if err != nil || len(bytes.TrimSpace(raw)) == 0 {
		return ""
	}
	var entries map[string]cachedContextEntry
	if json.Unmarshal(raw, &entries) != nil || entries == nil {
		return ""
	}
	if e, ok := entries[contextCacheKey(repo)]; ok && e.Rendered != "" {
		return e.Rendered
	}
	if e, ok := entries["global"]; ok && e.Rendered != "" {
		return e.Rendered
	}
	return ""
}
