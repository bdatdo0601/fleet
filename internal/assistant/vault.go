package assistant

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"
)

const skillProvenanceFile = ".fleet-skill.json"

var skillSlugRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// Skill describes one SKILL.md bundle inside <vault>/_skills/<name>/SKILL.md.
type Skill struct {
	Name        string           `json:"name"`
	Description string           `json:"description"`
	Model       string           `json:"model,omitempty"`
	Approval    string           `json:"approval,omitempty"`
	SyncToFleet bool             `json:"sync_to_fleet,omitempty"`
	Tags        []string         `json:"tags,omitempty"`
	Path        string           `json:"path"`
	Content     string           `json:"content,omitempty"`
	Files       []string         `json:"files,omitempty"`
	Source      *SkillProvenance `json:"source,omitempty"`
}

// VaultSearchHit represents one search match in the Obsidian vault.
type VaultSearchHit struct {
	Vault   string `json:"vault,omitempty"`
	Path    string `json:"path"`
	Title   string `json:"title"`
	Snippet string `json:"snippet"`
}

// Vault manages path-jailed Markdown operations on an Obsidian vault or local assistant workspace.
type Vault struct {
	Root string
}

// ResolveVaultRoot resolves cfg.VaultPath (expanding ~) or defaults to fallbackRoot.
func ResolveVaultRoot(configured, fallbackRoot string) string {
	p := strings.TrimSpace(configured)
	if p == "" {
		return fallbackRoot
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			if p == "~" {
				p = home
			} else {
				p = filepath.Join(home, p[2:])
			}
		}
	}
	abs, err := filepath.Abs(p)
	if err == nil {
		return abs
	}
	return filepath.Clean(p)
}

// OpenVault returns a Vault rooted at dir (creating default agent files if missing).
func OpenVault(dir string) (*Vault, error) {
	if dir == "" {
		return nil, newErr("vault path is required", http.StatusBadRequest)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, err
	}
	v := &Vault{Root: abs}
	if err := v.EnsureScaffold(); err != nil {
		return nil, err
	}
	return v, nil
}

// OpenVaultNoScaffold opens an existing Obsidian vault directory without creating any agent files or folders.
func OpenVaultNoScaffold(dir string) (*Vault, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, newErr("vault path is required", http.StatusBadRequest)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, newErr(fmt.Sprintf("vault directory %q not found", abs), http.StatusNotFound)
		}
		return nil, err
	}
	if !fi.IsDir() {
		return nil, newErr(fmt.Sprintf("vault path %q is not a directory", abs), http.StatusBadRequest)
	}
	return &Vault{Root: abs}, nil
}

// ResolveRel validates and resolves a relative vault path without escaping Root.
func (v *Vault) ResolveRel(rel string) (string, string, error) {
	if strings.ContainsRune(rel, 0) {
		return "", "", newErr("invalid path", http.StatusBadRequest)
	}
	trimmed := strings.TrimSpace(rel)
	if trimmed == "" || filepath.IsAbs(trimmed) || strings.HasPrefix(trimmed, "/") || strings.HasPrefix(trimmed, `\`) {
		return "", "", newErr("path must be relative and stay inside vault", http.StatusBadRequest)
	}
	clean := filepath.Clean(trimmed)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", "", newErr("path must stay inside vault", http.StatusBadRequest)
	}
	full := filepath.Join(v.Root, clean)
	rootReal, err := filepath.EvalSymlinks(v.Root)
	if err != nil {
		rootReal = v.Root
	}
	for _, candidate := range []string{full, filepath.Dir(full)} {
		if resolved, rErr := filepath.EvalSymlinks(candidate); rErr == nil {
			relCheck, relErr := filepath.Rel(rootReal, resolved)
			if relErr != nil || relCheck == ".." || strings.HasPrefix(relCheck, ".."+string(filepath.Separator)) {
				return "", "", newErr("symlink escapes vault", http.StatusForbidden)
			}
		}
	}
	return filepath.ToSlash(clean), full, nil
}

// IsAutoWritable reports whether rel sits inside an auto-writable vault folder.
func IsAutoWritable(rel string, allowedDirs []string) bool {
	slash := filepath.ToSlash(strings.TrimPrefix(filepath.Clean(rel), "/"))
	top, _, _ := strings.Cut(slash, "/")
	for _, d := range allowedDirs {
		if strings.EqualFold(strings.TrimSpace(d), top) {
			return true
		}
	}
	return false
}

// EnsureScaffold creates baseline _agent and _skills files only if missing.
func (v *Vault) EnsureScaffold() error {
	for _, dir := range []string{"_agent/memory/archive", "_skills", "Daily", "Inbox"} {
		if err := os.MkdirAll(filepath.Join(v.Root, dir), 0o755); err != nil {
			return err
		}
	}
	seeds := map[string]string{
		"_agent/SOUL.md": `# Fleet Always-On Assistant

You are the user's calm, concise always-on chief of staff running on the Fleet lighthouse.
- Prefer direct answers and short bullet points for mobile reading.
- Use Fleet MCP tools to inspect agents, shared memory, and Slack before guessing.
- Never claim an action succeeded unless the tool output confirmed it.
- Keep _agent/memory/REFLECTIONS.md and _skills concise; put engineering/repo conventions in fleet memory.
`,
		"_agent/HEARTBEAT.md": `# Heartbeat Checklist
# Lines starting with # are comments. Add active checks below when ready.
# Example:
# - Check fleet_list_agents(state="waiting"); if any session has been waiting >10 minutes, send one concise notification.
`,
		"_agent/memory/REFLECTIONS.md": `# Reflections
_Condensed durable personal context and active projects._
`,
		"_agent/memory/OBSERVATIONS.md": `# Observations
_Append-only dated observation log maintained by the background Observer._
`,
		"_skills/lighthouse-self-config/SKILL.md": `---
name: lighthouse-self-config
description: Self-configure the Fleet lighthouse host (install CLIs/runtimes like uv or brew packages, troubleshoot stdio MCP servers, inspect local services/logs) by delegating to the lighthouse Fleet worker.
approval: required
tags: [lighthouse, ops, self-config]
---
# Lighthouse Host Self-Configuration

Use the lighthouse's local Fleet worker (` + "`slacker-worker`" + ` running in ` + "`gui/<uid>`" + `) to configure or troubleshoot the machine ` + "`fleet serve`" + ` runs on.

## When to Use (and When Not To)
- **Use ` + "`fleet_spawn_agent`" + ` targeting the lighthouse device** (shown in the ` + "`Lighthouse Host Worker`" + ` system section, e.g. ` + "`mini`" + `) with ` + "`repo_path: \"~\"`" + ` and ` + "`policy: \"auto\"`" + ` (unless the user requests another policy) when:
  - Installing missing host CLI dependencies or MCP runtimes (` + "`uv`" + `/` + "`uvx`" + `, ` + "`node`" + `/` + "`npx`" + `, ` + "`brew`" + ` formulae, etc.).
  - Diagnosing failing stdio MCP server commands or verifying local runtime versions.
  - Inspecting lighthouse launchd services or logs (` + "`~/.fleet/*.log`" + `).
- **Do NOT use this for** document/data/file tasks (use ` + "`sandbox_*`" + ` micro-VM tools) or normal software repo coding (target awake Mac workstations such as ` + "`laptop`" + ` or ` + "`office`" + `).

## Pre-Flight & Offline Handling
- Check the ` + "`Lighthouse Host Worker`" + ` section in the system prompt.
- If the lighthouse worker is **offline**, tell the user to run ` + "`fleet assistant worker-install`" + ` on the lighthouse (supports Rex automatically or ` + "`--use-tmux`" + `).

## Prompt Guidelines for the Spawned Worker
- Prefer idempotent, non-interactive commands (` + "`brew install <pkg>`" + ` or official non-interactive installers) and ask the worker to verify with ` + "`which <bin> && <bin> --version`" + `.
- Explicitly instruct the worker **never** to read, print, or modify secret files (` + "`~/.fleet/op-service-account.token`" + `, ` + "`~/.fleet/assistant.db`" + `, OAuth tokens) and **not** to restart ` + "`fleet serve`" + ` unless explicitly requested.
- Keep scope minimal and ask for a concise summary of what changed.

## After Completion
- When the delegation wakes you up on completion (` + "`idle`" + `/` + "`exited`" + `), ` + "`MCPManager`" + ` automatically searches ` + "`/opt/homebrew/bin`" + `, ` + "`~/.local/bin`" + `, and common Node/Python shims — call ` + "`mcp_test`" + ` or ` + "`mcp_configure`" + ` directly to verify without restarting ` + "`fleet serve`" + `.

## Approval Policy
- Spawning a worker on the lighthouse is only allowed during direct user chat and **always** pauses for explicit user approval (never bypassed by ` + "`auto_approve`" + `).
`,
	}
	for rel, content := range seeds {
		_, full, err := v.ResolveRel(rel)
		if err != nil {
			return err
		}
		if _, statErr := os.Stat(full); errors.Is(statErr, os.ErrNotExist) {
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
				return err
			}
		}
	}
	return nil
}

// ReadFile reads a markdown or text file inside the vault.
func (v *Vault) ReadFile(rel string) (string, error) {
	b, err := v.ReadFileBytes(rel, 0)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ReadFileBytes reads raw bytes from a file inside the vault, enforcing an optional maxBytes cap.
func (v *Vault) ReadFileBytes(rel string, maxBytes int64) ([]byte, error) {
	_, full, err := v.ResolveRel(rel)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(full)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, newErr(fmt.Sprintf("file %q not found", rel), http.StatusNotFound)
		}
		return nil, err
	}
	if info.IsDir() {
		return nil, newErr(fmt.Sprintf("%q is a directory", rel), http.StatusBadRequest)
	}
	if maxBytes > 0 && info.Size() > maxBytes {
		return nil, newErr(fmt.Sprintf("file %q (%d bytes) exceeds %d byte limit", rel, info.Size(), maxBytes), http.StatusRequestEntityTooLarge)
	}
	return os.ReadFile(full)
}

// WriteFile writes content to rel inside the vault, creating parent directories.
func (v *Vault) WriteFile(rel, content string) (string, error) {
	norm, full, err := v.ResolveRel(rel)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		return "", err
	}
	return norm, nil
}

// AppendFile appends a line/block to rel inside the vault.
func (v *Vault) AppendFile(rel, addition string) (string, error) {
	norm, full, err := v.ResolveRel(rel)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return "", err
	}
	existing, err := os.ReadFile(full)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	var b strings.Builder
	b.Write(existing)
	if len(existing) > 0 && !strings.HasSuffix(string(existing), "\n") {
		b.WriteByte('\n')
	}
	b.WriteString(strings.TrimRight(addition, "\n"))
	b.WriteByte('\n')
	if err := os.WriteFile(full, []byte(b.String()), 0o644); err != nil {
		return "", err
	}
	return norm, nil
}

// ParseSkillFrontmatter extracts metadata from a SKILL.md file's YAML frontmatter.
func ParseSkillFrontmatter(slug, content string) Skill {
	sk := Skill{Name: slug}
	trimmed := strings.TrimSpace(content)
	if !strings.HasPrefix(trimmed, "---") {
		return sk
	}
	rest := strings.TrimPrefix(trimmed, "---")
	rest = strings.TrimPrefix(rest, "\r\n")
	rest = strings.TrimPrefix(rest, "\n")
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return sk
	}
	fm := rest[:end]
	inTags := false
	for line := range strings.SplitSeq(fm, "\n") {
		raw := strings.TrimSpace(line)
		if raw == "" || strings.HasPrefix(raw, "#") {
			continue
		}
		if inTags && strings.HasPrefix(raw, "- ") {
			sk.Tags = append(sk.Tags, strings.Trim(strings.TrimSpace(raw[2:]), `"'`))
			continue
		}
		inTags = false
		k, val, ok := strings.Cut(raw, ":")
		if !ok {
			continue
		}
		k = strings.ToLower(strings.TrimSpace(k))
		val = strings.Trim(strings.TrimSpace(val), `"'`)
		switch k {
		case "name":
			if val != "" {
				sk.Name = val
			}
		case "description":
			sk.Description = val
		case "model":
			sk.Model = val
		case "approval":
			sk.Approval = val
		case "sync_to_fleet":
			sk.SyncToFleet = strings.EqualFold(val, "true")
		case "tags":
			if val == "" {
				inTags = true
			} else {
				val = strings.Trim(val, "[]")
				for t := range strings.SplitSeq(val, ",") {
					if clean := strings.Trim(strings.TrimSpace(t), `"'`); clean != "" {
						sk.Tags = append(sk.Tags, clean)
					}
				}
			}
		}
	}
	return sk
}

// ListSkills returns metadata for all _skills/*/SKILL.md files.
func (v *Vault) ListSkills() ([]Skill, error) {
	skillsDir := filepath.Join(v.Root, "_skills")
	entries, err := os.ReadDir(skillsDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []Skill
	for _, ent := range entries {
		if !ent.IsDir() || strings.HasPrefix(ent.Name(), ".") {
			continue
		}
		slug := ent.Name()
		rel := "_skills/" + slug + "/SKILL.md"
		content, rErr := v.ReadFile(rel)
		if rErr != nil {
			continue
		}
		sk := ParseSkillFrontmatter(slug, content)
		sk.Name = slug
		sk.Path = rel
		sk.Files = v.listSkillFiles(slug)
		sk.Source = v.readSkillProvenance(slug)
		out = append(out, sk)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// GetSkill reads one skill bundle including full Markdown content.
func (v *Vault) GetSkill(name string) (Skill, error) {
	slug := strings.TrimSpace(name)
	if !skillSlugRE.MatchString(slug) {
		return Skill{}, newErr("invalid skill name", http.StatusBadRequest)
	}
	rel := "_skills/" + slug + "/SKILL.md"
	content, err := v.ReadFile(rel)
	if err != nil {
		return Skill{}, err
	}
	sk := ParseSkillFrontmatter(slug, content)
	sk.Name = slug
	sk.Path = rel
	sk.Content = content
	sk.Files = v.listSkillFiles(slug)
	sk.Source = v.readSkillProvenance(slug)
	return sk, nil
}

// ReadSkillFile reads SKILL.md or a validated companion file inside _skills/<name>/.
func (v *Vault) ReadSkillFile(name, relFile string) (string, error) {
	slug := strings.TrimSpace(name)
	if !skillSlugRE.MatchString(slug) {
		return "", newErr("invalid skill name", http.StatusBadRequest)
	}
	trimmed := strings.TrimSpace(relFile)
	if trimmed == "" || trimmed == "SKILL.md" {
		return v.ReadFile("_skills/" + slug + "/SKILL.md")
	}
	cleanSub, err := validateSkillSubpath(trimmed)
	if err != nil {
		return "", err
	}
	b, err := v.ReadFileBytes("_skills/"+slug+"/"+cleanSub, maxSkillFileBytes)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// SaveSkill writes _skills/<name>/SKILL.md.
func (v *Vault) SaveSkill(name, content string) (Skill, error) {
	slug := strings.TrimSpace(name)
	if !skillSlugRE.MatchString(slug) {
		return Skill{}, newErr("skill name must match [a-z0-9][a-z0-9_-]*", http.StatusBadRequest)
	}
	rel := "_skills/" + slug + "/SKILL.md"
	if _, err := v.WriteFile(rel, content); err != nil {
		return Skill{}, err
	}
	return v.GetSkill(slug)
}

// SaveSkillBundle atomically replaces _skills/<name>/ with SKILL.md, companion files, and optional provenance.
func (v *Vault) SaveSkillBundle(name, skillMD string, extraFiles map[string][]byte, prov *SkillProvenance) (Skill, error) {
	slug := strings.TrimSpace(name)
	if !skillSlugRE.MatchString(slug) {
		return Skill{}, newErr("skill name must match [a-z0-9][a-z0-9_-]*", http.StatusBadRequest)
	}
	if len(extraFiles)+1 > maxSkillBundleFiles {
		return Skill{}, newErr(fmt.Sprintf("skill bundle exceeds %d file limit", maxSkillBundleFiles), http.StatusBadRequest)
	}
	totalBytes := len(skillMD)
	cleanExtra := make(map[string][]byte, len(extraFiles))
	for rawSub, data := range extraFiles {
		cleanSub, err := validateSkillSubpath(rawSub)
		if err != nil {
			return Skill{}, err
		}
		if len(data) > maxSkillFileBytes {
			return Skill{}, newErr(fmt.Sprintf("skill file %q exceeds %d byte limit", cleanSub, maxSkillFileBytes), http.StatusBadRequest)
		}
		totalBytes += len(data)
		if totalBytes > maxSkillBundleBytes {
			return Skill{}, newErr(fmt.Sprintf("skill bundle exceeds %d byte total limit", maxSkillBundleBytes), http.StatusBadRequest)
		}
		cleanExtra[cleanSub] = data
	}

	_, dirFull, err := v.ResolveRel("_skills/" + slug)
	if err != nil {
		return Skill{}, err
	}
	if err := os.RemoveAll(dirFull); err != nil {
		return Skill{}, err
	}
	if _, err := v.WriteFile("_skills/"+slug+"/SKILL.md", skillMD); err != nil {
		return Skill{}, err
	}
	for cleanSub, data := range cleanExtra {
		_, fileFull, rErr := v.ResolveRel("_skills/" + slug + "/" + cleanSub)
		if rErr != nil {
			return Skill{}, rErr
		}
		if mkErr := os.MkdirAll(filepath.Dir(fileFull), 0o755); mkErr != nil {
			return Skill{}, mkErr
		}
		if wErr := os.WriteFile(fileFull, data, 0o644); wErr != nil {
			return Skill{}, wErr
		}
	}
	if prov != nil {
		rawProv, mErr := json.MarshalIndent(prov, "", "  ")
		if mErr != nil {
			return Skill{}, mErr
		}
		_, provFull, rErr := v.ResolveRel("_skills/" + slug + "/" + skillProvenanceFile)
		if rErr != nil {
			return Skill{}, rErr
		}
		if wErr := os.WriteFile(provFull, append(rawProv, '\n'), 0o644); wErr != nil {
			return Skill{}, wErr
		}
	}
	return v.GetSkill(slug)
}

// DeleteSkill removes the entire _skills/<name>/ bundle directory.
func (v *Vault) DeleteSkill(name string) error {
	slug := strings.TrimSpace(name)
	if !skillSlugRE.MatchString(slug) {
		return newErr("invalid skill name", http.StatusBadRequest)
	}
	rel := "_skills/" + slug + "/SKILL.md"
	_, full, err := v.ResolveRel(rel)
	if err != nil {
		return err
	}
	if _, statErr := os.Stat(full); statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			return newErr(fmt.Sprintf("skill %q not found", slug), http.StatusNotFound)
		}
		return statErr
	}
	_, dirFull, err := v.ResolveRel("_skills/" + slug)
	if err != nil {
		return err
	}
	return os.RemoveAll(dirFull)
}

func validateSkillSubpath(rel string) (string, error) {
	if strings.ContainsRune(rel, 0) {
		return "", newErr("invalid skill file path", http.StatusBadRequest)
	}
	trimmed := strings.TrimSpace(rel)
	if trimmed == "" || filepath.IsAbs(trimmed) || strings.HasPrefix(trimmed, "/") || strings.HasPrefix(trimmed, `\`) {
		return "", newErr("skill file path must be relative", http.StatusBadRequest)
	}
	slash := filepath.ToSlash(filepath.Clean(trimmed))
	if slash == "." || slash == ".." || strings.HasPrefix(slash, "../") || strings.Contains(slash, "/../") {
		return "", newErr("skill file path escapes bundle directory", http.StatusBadRequest)
	}
	if slash == "SKILL.md" || slash == skillProvenanceFile {
		return "", newErr(fmt.Sprintf("%s cannot be addressed as a companion file", slash), http.StatusBadRequest)
	}
	for seg := range strings.SplitSeq(slash, "/") {
		if seg == "" || strings.HasPrefix(seg, ".") {
			return "", newErr("hidden files are not allowed in skill bundles", http.StatusBadRequest)
		}
	}
	return slash, nil
}

func (v *Vault) listSkillFiles(slug string) []string {
	_, dirFull, err := v.ResolveRel("_skills/" + slug)
	if err != nil {
		return nil
	}
	var files []string
	_ = filepath.WalkDir(dirFull, func(p string, d fs.DirEntry, wErr error) error {
		if wErr != nil {
			return wErr
		}
		if d == nil || p == dirFull {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if rel, rErr := filepath.Rel(dirFull, p); rErr == nil {
			if slash := filepath.ToSlash(rel); slash != "SKILL.md" {
				files = append(files, slash)
			}
		}
		return nil
	})
	slices.Sort(files)
	return files
}

func (v *Vault) readSkillProvenance(slug string) *SkillProvenance {
	b, err := v.ReadFileBytes("_skills/"+slug+"/"+skillProvenanceFile, 64*1024)
	if err != nil {
		return nil
	}
	var prov SkillProvenance
	if err := json.Unmarshal(b, &prov); err != nil || strings.TrimSpace(prov.Source) == "" {
		return nil
	}
	return &prov
}

// Search scans Markdown files in the vault for query terms.
func (v *Vault) Search(query string, limit int) ([]VaultSearchHit, error) {
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	terms := strings.Fields(strings.ToLower(strings.TrimSpace(query)))
	var hits []VaultSearchHit
	err := filepath.WalkDir(v.Root, func(path string, d fs.DirEntry, wErr error) error {
		if wErr != nil || d == nil {
			return filepath.SkipDir
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".git" || name == ".obsidian" || name == ".trash" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(d.Name()), ".md") {
			return nil
		}
		hit, ok := matchVaultFile(v.Root, path, terms)
		if !ok {
			return nil
		}
		hits = append(hits, hit)
		if len(hits) >= limit {
			return fs.SkipAll
		}
		return nil
	})
	return hits, err
}

func matchVaultFile(root, path string, terms []string) (VaultSearchHit, bool) {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return VaultSearchHit{}, false
	}
	rel = filepath.ToSlash(rel)
	data, err := os.ReadFile(path)
	if err != nil {
		return VaultSearchHit{}, false
	}
	text := string(data)
	low := strings.ToLower(rel + "\n" + text)
	for _, term := range terms {
		if !strings.Contains(low, term) {
			return VaultSearchHit{}, false
		}
	}
	return VaultSearchHit{
		Path:    rel,
		Title:   strings.TrimSuffix(filepath.Base(rel), ".md"),
		Snippet: firstMatchingSnippet(text, terms),
	}, true
}

func firstMatchingSnippet(text string, terms []string) string {
	for ln := range strings.SplitSeq(text, "\n") {
		t := strings.TrimSpace(ln)
		if t == "" || t == "---" {
			continue
		}
		if len(terms) == 0 {
			return truncateText(t, 180)
		}
		low := strings.ToLower(t)
		for _, term := range terms {
			if strings.Contains(low, term) {
				return truncateText(t, 180)
			}
		}
	}
	return truncateText(strings.TrimSpace(text), 180)
}

func truncateUTF8Bytes(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

func truncateText(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return strings.TrimSpace(truncateUTF8Bytes(s, maxLen)) + "..."
}

// VaultCommit describes one git commit inside a git-backed Obsidian vault.
type VaultCommit struct {
	SHA      string `json:"sha"`
	ShortSHA string `json:"short_sha"`
	Date     string `json:"date"`
	Message  string `json:"message"`
}

var gitSHARE = regexp.MustCompile(`^[0-9a-fA-F]{4,40}$`)

func validateGitSHA(sha string) (string, error) {
	clean := strings.TrimSpace(sha)
	if !gitSHARE.MatchString(clean) {
		return "", newErr("invalid git commit SHA", http.StatusBadRequest)
	}
	return clean, nil
}

// GitHistory returns recent commits in the vault git repo (or empty slice if not git-backed).
func (v *Vault) GitHistory(limit int) ([]VaultCommit, error) {
	if _, err := os.Stat(filepath.Join(v.Root, ".git")); errors.Is(err, os.ErrNotExist) {
		return []VaultCommit{}, nil
	} else if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	out, err := exec.Command("git", "-C", v.Root, "log", fmt.Sprintf("-n%d", limit), "--format=%H\t%h\t%cI\t%s").Output()
	if err != nil {
		return []VaultCommit{}, err
	}
	var commits []VaultCommit
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 4)
		if len(parts) < 4 {
			continue
		}
		commits = append(commits, VaultCommit{SHA: parts[0], ShortSHA: parts[1], Date: parts[2], Message: parts[3]})
	}
	return commits, nil
}

// GitDiff returns the stat + patch diff for a specific vault commit SHA.
func (v *Vault) GitDiff(sha string) (string, error) {
	clean, err := validateGitSHA(sha)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(v.Root, ".git")); err != nil {
		return "", newErr("vault is not a git repository", http.StatusBadRequest)
	}
	out, err := exec.Command("git", "-C", v.Root, "show", "--stat", "--patch", clean).CombinedOutput()
	if err != nil {
		return "", newErr(fmt.Sprintf("git show %s failed: %s", clean, strings.TrimSpace(string(out))), http.StatusNotFound)
	}
	const maxDiff = 64 * 1024
	if len(out) > maxDiff {
		return string(out[:maxDiff]) + "\n... (diff truncated)\n", nil
	}
	return string(out), nil
}

// GitRollback restores the vault working tree to commit sha and records a forward rollback commit.
func (v *Vault) GitRollback(sha string) (VaultCommit, error) {
	clean, err := validateGitSHA(sha)
	if err != nil {
		return VaultCommit{}, err
	}
	if _, err := os.Stat(filepath.Join(v.Root, ".git")); err != nil {
		return VaultCommit{}, newErr("vault is not a git repository", http.StatusBadRequest)
	}
	if out, err := exec.Command("git", "-C", v.Root, "checkout", clean, "--", ".").CombinedOutput(); err != nil {
		return VaultCommit{}, newErr(fmt.Sprintf("git checkout %s failed: %s", clean, strings.TrimSpace(string(out))), http.StatusBadRequest)
	}
	short := clean
	if len(short) > 7 {
		short = short[:7]
	}
	msg := "ui: rollback vault to " + short
	_ = exec.Command("git", "-C", v.Root, "add", "-A").Run()
	_ = exec.Command("git", "-C", v.Root, "commit", "--allow-empty", "-m", msg, "--quiet").Run()
	hist, _ := v.GitHistory(1)
	if len(hist) > 0 {
		return hist[0], nil
	}
	return VaultCommit{SHA: clean, ShortSHA: short, Message: msg}, nil
}

// MaybeGitCommit stages and commits changes if the vault is already a git repository.
func (v *Vault) MaybeGitCommit(enabled bool, msg string) {
	if !enabled {
		return
	}
	if _, err := os.Stat(filepath.Join(v.Root, ".git")); err != nil {
		return
	}
	addCmd := exec.Command("git", "-C", v.Root, "add", "-A")
	if err := addCmd.Run(); err != nil {
		return
	}
	commitMsg := cmp.Or(strings.TrimSpace(msg), "assistant: update vault")
	_ = exec.Command("git", "-C", v.Root, "commit", "-m", commitMsg, "--quiet").Run()
}

// NonEmptyBody reports whether a markdown file has non-comment, non-heading content.
func NonEmptyBody(content string) bool {
	for line := range strings.SplitSeq(content, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, "_") {
			continue
		}
		return true
	}
	return false
}
