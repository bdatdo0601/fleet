package memory

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestStoreCRUDFTS5RevisionsAndRevert(t *testing.T) {
	st := openTestStore(t)

	// Normalize user -> preference, default tier for global preference is always.
	res1, err := st.Upsert(UpsertParams{
		Scope:  "global",
		Key:    "prefer-few-prs",
		Kind:   "user",
		Body:   "Keep PR count minimal; avoid huge refactors.\n\nSplit only on strict deploy order.",
		Source: "human",
		Device: "laptop",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res1.Action != "created" || res1.Item.Kind != "preference" || res1.Item.Tier != "always" || res1.Queued {
		t.Fatalf("unexpected upsert result: %+v", res1)
	}

	// Repo notes default to searchable and queue curation when source=fleet.
	res2, err := st.Upsert(UpsertParams{
		Scope:   "repo:iron-fillet",
		Key:     "C++_PyMuPDF/gotcha",
		Kind:    "reference",
		Summary: "PyMuPDF C++ insert_pdf partial failure",
		Body:    "Run op://Server/IronFilletDev/OCR_DATABASE_URL checks before retrying insert_pdf.",
		Source:  "fleet",
		Device:  "mini",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res2.Action != "created" || res2.Item.Key != "c_pymupdf-gotcha" || res2.Item.Tier != "searchable" || !res2.Queued {
		t.Fatalf("unexpected repo upsert result: %+v", res2)
	}

	// Hyphenated and punctuated queries work cleanly without FTS syntax errors.
	for _, q := range []string{"prefer-few-prs", "C++", "op://Server", "minimal nonexistentword"} {
		hits, sErr := st.Search(SearchFilter{Query: q})
		if sErr != nil {
			t.Fatalf("search %q failed: %v", q, sErr)
		}
		if len(hits) == 0 {
			t.Fatalf("expected hits for %q (AND->OR fallback), got 0", q)
		}
	}

	// Update + history + revert.
	if _, err := st.Upsert(UpsertParams{
		Scope:   "global",
		Key:     "prefer-few-prs",
		Kind:    "preference",
		Summary: "Always use minimal PRs",
		Body:    "Revised body v2",
		Source:  "human",
		Reason:  "tighten wording",
	}); err != nil {
		t.Fatal(err)
	}
	hist, err := st.History("global", "prefer-few-prs", 10)
	if err != nil || len(hist) != 2 {
		t.Fatalf("expected 2 revisions, got %+v err=%v", hist, err)
	}
	reverted, err := st.Revert("global", "prefer-few-prs", 1, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	if reverted.Revision != 3 || !strings.Contains(reverted.Body, "Keep PR count minimal") {
		t.Fatalf("unexpected reverted item: %+v", reverted)
	}

	// Promote repo item to global/always with lock.
	promoted, err := st.Promote("repo:iron-fillet", "c_pymupdf-gotcha", "global", "pymupdf-rule", "always", "", "laptop", true)
	if err != nil {
		t.Fatal(err)
	}
	if promoted.Scope != "global" || promoted.Tier != "always" || !promoted.Locked {
		t.Fatalf("unexpected promoted item: %+v", promoted)
	}

	// Soft archive hides from default search, hard delete removes row.
	if err := st.Delete("global", "pymupdf-rule", false, "laptop", "cli", "", "test"); err != nil {
		t.Fatal(err)
	}
	active, _ := st.Search(SearchFilter{Query: "PyMuPDF", Scope: "global"})
	if len(active) != 0 {
		t.Fatalf("expected archived item excluded, got %+v", active)
	}
	archived, _ := st.Search(SearchFilter{Query: "PyMuPDF", Scope: "global", IncludeArchived: true})
	if len(archived) != 1 || archived[0].Status != "archived" {
		t.Fatalf("expected archived item when IncludeArchived=true, got %+v", archived)
	}
	if err := st.Delete("global", "pymupdf-rule", true, "laptop", "cli", "", "cleanup"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Get("global", "pymupdf-rule", false); err == nil {
		t.Fatal("expected 404 after hard delete")
	}
}

func TestBackupProtectionAndContextBudget(t *testing.T) {
	st := openTestStore(t)

	// Human/fleet/locked entries cannot be overwritten by claude_backup.
	if _, err := st.Upsert(UpsertParams{
		Scope:  "repo:iron-fillet",
		Key:    "prefer-few-prs",
		Kind:   "feedback",
		Body:   "Human curated rule",
		Source: "human",
		Locked: new(true),
	}); err != nil {
		t.Fatal(err)
	}
	protRes, err := st.Upsert(UpsertParams{
		Scope:  "repo:iron-fillet",
		Key:    "prefer-few-prs",
		Kind:   "feedback",
		Body:   "Claude backup stale overwrite",
		Source: "claude_backup",
	})
	if err != nil {
		t.Fatal(err)
	}
	if protRes.Action != "protected" || protRes.Item.Body != "Human curated rule" {
		t.Fatalf("expected protected skip, got %+v", protRes)
	}

	// Mtime ordering protects newer backup from older timestamp.
	if _, err := st.Upsert(UpsertParams{
		Scope:       "repo:iron-fillet",
		Key:         "newer-note",
		Kind:        "project",
		Body:        "Newer body",
		Source:      "claude_backup",
		SourceMtime: "2026-11-20T12:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	staleRes, err := st.Upsert(UpsertParams{
		Scope:       "repo:iron-fillet",
		Key:         "newer-note",
		Kind:        "project",
		Body:        "Older body",
		Source:      "claude_backup",
		SourceMtime: "2026-10-01T12:00:00Z",
	})
	if err != nil || staleRes.Action != "stale" {
		t.Fatalf("expected stale skip, got %+v err=%v", staleRes, err)
	}

	// RenderContext stays strictly within MaxContextBytes even with many items.
	for i := range 30 {
		_, err := st.Upsert(UpsertParams{
			Scope:   "global",
			Key:     strings.Repeat("k", 20) + string(rune('a'+i%26)) + string(rune('0'+i/10)),
			Kind:    "preference",
			Tier:    "always",
			Summary: strings.Repeat("Rule summary text ", 10),
			Body:    "Body",
			Source:  "human",
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	ctxRes, err := st.RenderContext("iron-fillet", "laptop")
	if err != nil {
		t.Fatal(err)
	}
	if len(ctxRes.Rendered) > MaxContextBytes {
		t.Fatalf("rendered context exceeded MaxContextBytes: %d > %d", len(ctxRes.Rendered), MaxContextBytes)
	}
	if !strings.Contains(ctxRes.Rendered, "[Fleet Memory]") {
		t.Fatalf("expected [Fleet Memory] header in %q", ctxRes.Rendered)
	}
}

func TestParseClaudeMemoryCanonicalRepoHarvestAndCache(t *testing.T) {
	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	cacheDir := filepath.Join(tmp, "cache")

	// Create a local git repo with an SSH origin to verify CanonicalRepo.
	repoDir := filepath.Join(tmp, "alt-checkout")
	_ = os.MkdirAll(repoDir, 0o755)
	if _, err := exec.LookPath("git"); err == nil {
		_ = exec.Command("git", "-C", repoDir, "init").Run()
		_ = exec.Command("git", "-C", repoDir, "remote", "add", "origin", "git@github.com:org/iron-fillet.git").Run()
		if got := CanonicalRepo(repoDir); got != "iron-fillet" {
			t.Fatalf("expected canonical repo iron-fillet, got %q", got)
		}
	}

	projDir := filepath.Join(home, ".claude", "projects", "-tmp-alt-checkout")
	memDir := filepath.Join(projDir, "memory")
	_ = os.MkdirAll(memDir, 0o755)
	_ = os.WriteFile(filepath.Join(projDir, "sess.jsonl"), []byte(`{"type":"user","cwd":`+mustJSONString(repoDir)+`}`+"\n"), 0o644)
	_ = os.WriteFile(filepath.Join(memDir, "MEMORY.md"), []byte("- [skip](prefer-few-prs.md)\n"), 0o644)

	mdContent := `---
name: prefer-few-prs
description: "User prefers minimal PR count"
metadata: 
  node_type: memory
  type: feedback
  originSessionId: abc
  modified: 2026-10-15T15:07:54.718Z
---

Plan the fewest PRs possible and avoid sweeping changes.
`
	mdPath := filepath.Join(memDir, "prefer-few-prs.md")
	_ = os.WriteFile(mdPath, []byte(mdContent), 0o644)

	pm, err := ParseClaudeMemoryFile(mdPath)
	if err != nil || pm == nil {
		t.Fatalf("ParseClaudeMemoryFile failed: %+v err=%v", pm, err)
	}
	if pm.Key != "prefer-few-prs" || pm.Kind != "feedback" || pm.SourceMtime != "2026-10-15T15:07:54.718Z" {
		t.Fatalf("unexpected parsed memory: %+v", pm)
	}

	st := openTestStore(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Items []UpsertParams `json:"items"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		res, sErr := st.SyncBatch("laptop", req.Items)
		if sErr != nil {
			http.Error(w, sErr.Error(), 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(res)
	}))
	defer srv.Close()

	rep, err := HarvestLocal(HarvestOpts{Home: home, CacheDir: cacheDir, APIURL: srv.URL, Device: "laptop"})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Scanned != 1 || rep.Created != 1 || rep.Queued != 1 {
		t.Fatalf("unexpected harvest report: %+v", rep)
	}

	// Second run skips unchanged file from local cache.
	rep2, err := HarvestLocal(HarvestOpts{Home: home, CacheDir: cacheDir, APIURL: srv.URL, Device: "laptop"})
	if err != nil || rep2.Skipped != 1 || rep2.Pending != 0 {
		t.Fatalf("expected cached skip on second run, got %+v err=%v", rep2, err)
	}

	// Context cache roundtrip.
	if err := SaveContextCache(cacheDir, "iron-fillet", "[Fleet Memory]\n- [global/prefer-few-prs] minimal PRs", nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := LoadContextCache(cacheDir, "iron-fillet"); !strings.Contains(got, "minimal PRs") {
		t.Fatalf("unexpected cached context: %q", got)
	}
}

func mustJSONString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestCuratorOpenRouterProposalMergeAndWorker(t *testing.T) {
	st := openTestStore(t)
	cfgPath := filepath.Join(t.TempDir(), "curator.json")
	cur := NewCurator(st, cfgPath)

	var mu sync.Mutex
	var sawOpenRouterPrivacy bool
	var sawSchemaFallback bool
	mode := "promote_global"

	llmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		curMode := mode
		if prov, ok := req["provider"].(map[string]any); ok && prov["data_collection"] == "deny" {
			sawOpenRouterPrivacy = true
		}
		rf, _ := req["response_format"].(map[string]any)
		isSchema := rf != nil && rf["type"] == "json_schema"
		if curMode == "merge" && isSchema {
			sawSchemaFallback = true
			mu.Unlock()
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"json_schema response_format unsupported"}}`))
			return
		}
		mu.Unlock()

		content := `{"ok":true}`
		if strings.Contains(req["messages"].([]any)[1].(map[string]any)["content"].(string), "CANDIDATE:") {
			switch curMode {
			case "promote_global":
				content = `{"action":"promote_global","target_key":"prefer-few-prs","scope":"global","kind":"feedback","tier":"always","summary":"Prefer minimal PRs","body":"Keep PR count minimal across repos.","reason":"Cross-repo workflow rule"}`
			case "merge":
				content = `{"action":"merge","target_key":"existing-rule","scope":"repo:app","kind":"feedback","tier":"searchable","summary":"Merged rule","body":"Combined rule body","reason":"Duplicate feedback"}`
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": content}}},
		})
	}))
	defer llmSrv.Close()

	// Configure with openrouter provider pointed at mock server (force OpenRouter privacy headers via openrouter provider).
	status, err := cur.Configure(context.Background(), CuratorConfig{
		Enabled:  true,
		Provider: "openrouter",
		BaseURL:  llmSrv.URL,
		Model:    DefaultOpenRouterModel,
		APIKey:   "sk-or-v1-secretkey12345678",
		ZDR:      true,
	}, true)
	if err != nil {
		t.Fatalf("Configure failed: %v", err)
	}
	if status.APIKeyHint != "sk-or-v1...5678" || !status.Enabled {
		t.Fatalf("unexpected curator status: %+v", status)
	}

	// Add repo feedback -> curator queues promote_global into memory_proposals when AutoGlobal=false.
	if _, err := st.Upsert(UpsertParams{
		Scope: "repo:iron-fillet", Key: "prefer-few-prs", Kind: "feedback",
		Body: "Keep PR count minimal", Source: "fleet",
	}); err != nil {
		t.Fatal(err)
	}
	reps, err := cur.RunPending(context.Background(), 10, false, false)
	if err != nil || len(reps) != 1 || reps[0].QueuedProposalID == 0 {
		t.Fatalf("expected queued proposal, got %+v err=%v", reps, err)
	}
	if !sawOpenRouterPrivacy {
		t.Fatal("expected provider.data_collection=deny on openrouter request")
	}

	props, err := st.ListProposals("pending", 10)
	if err != nil || len(props) != 1 {
		t.Fatalf("expected 1 pending proposal, got %+v err=%v", props, err)
	}
	resolved, applied, err := st.ResolveProposal(props[0].ID, true, "laptop")
	if err != nil || resolved.Status != "approved" || applied == nil || applied.Scope != "global" || applied.Tier != "always" {
		t.Fatalf("unexpected resolve result: prop=%+v item=%+v err=%v", resolved, applied, err)
	}

	// Test json_schema -> json_object fallback + merge + worker Start/Stop.
	mu.Lock()
	mode = "merge"
	mu.Unlock()
	if _, err := st.Upsert(UpsertParams{
		Scope: "repo:app", Key: "existing-rule", Kind: "feedback",
		Body: "Original rule", Source: "curator", EnqueueCurate: new(false),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Upsert(UpsertParams{
		Scope: "repo:app", Key: "dup-rule", Kind: "feedback",
		Body: "Duplicate rule", Source: "fleet",
	}); err != nil {
		t.Fatal(err)
	}

	cur.Start(context.Background())
	defer cur.Stop()
	cur.Notify()

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		det, gErr := st.Get("repo:app", "dup-rule", false)
		if gErr == nil && det.Item.Status == "superseded" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	det, _ := st.Get("repo:app", "dup-rule", false)
	merged, _ := st.Get("repo:app", "existing-rule", false)
	if det.Item.Status != "superseded" || merged.Item.Body != "Combined rule body" || !sawSchemaFallback {
		t.Fatalf("worker merge failed: dup=%+v merged=%+v fallback=%v", det.Item, merged.Item, sawSchemaFallback)
	}
}
