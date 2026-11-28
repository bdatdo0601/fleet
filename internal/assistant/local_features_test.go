package assistant

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bdatdo0601/fleet/internal/memory"
)

func TestLocalLLMRoutingAndDoctor(t *testing.T) {
	var seenBodies []map[string]any
	var seenAuth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{{"id": "qwen3:8b"}},
			})
		case "/v1/chat/completions":
			seenAuth = append(seenAuth, r.Header.Get("Authorization"))
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			seenBodies = append(seenBodies, body)
			if stream, _ := body["stream"].(bool); stream {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"local ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{{"message": map[string]any{"content": "completed local"}}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cfg := DefaultConfig()
	cfg.APIKey = ""
	cfg.APIKeyOPRef = ""
	cfg.LocalLLM = LocalLLMConfig{
		Enabled:   true,
		BaseURL:   srv.URL + "/v1",
		Model:     "qwen3:8b",
		KeepAlive: "3m",
		NumCtx:    16384,
	}

	cli := newOpenRouterClient()
	msgs := []ChatMessage{
		{Role: "system", Content: []PromptPart{{Type: "text", Text: "Part A", CacheControl: map[string]any{"type": "ephemeral"}}, {Type: "text", Text: "Part B"}}},
		{Role: "user", Content: "hello"},
	}
	res, err := cli.StreamChat(t.Context(), cfg, "local/default", msgs, nil, nil)
	if err != nil {
		t.Fatalf("StreamChat local/default failed: %v", err)
	}
	if res.Content != "local ok" {
		t.Fatalf("expected 'local ok', got %q", res.Content)
	}

	txt, err := cli.CompleteText(t.Context(), cfg, "local/qwen3:8b", "sys", "user", 128)
	if err != nil || txt != "completed local" {
		t.Fatalf("CompleteText local failed: txt=%q err=%v", txt, err)
	}
	if len(seenBodies) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(seenBodies))
	}
	if seenAuth[0] != "" {
		t.Fatalf("expected no Authorization header for local endpoint, got %q", seenAuth[0])
	}
	if _, hasProv := seenBodies[0]["provider"]; hasProv {
		t.Fatalf("expected no OpenRouter provider field on local request")
	}
	if seenBodies[0]["model"] != "qwen3:8b" {
		t.Fatalf("unexpected local model payload: %+v", seenBodies[0])
	}
	ollamaBody := buildChatRequestBody(cfg, "http://127.0.0.1:11434/v1", "qwen3:8b", msgs, nil, true, 8192, true)
	if ollamaBody["keep_alive"] != "3m" {
		t.Fatalf("expected Ollama keep_alive=3m, got %+v", ollamaBody)
	}
	if opts, _ := ollamaBody["options"].(map[string]any); opts["num_ctx"] != 16384 {
		t.Fatalf("expected Ollama options.num_ctx=16384, got %+v", ollamaBody["options"])
	}
	sentMsgs, _ := seenBodies[0]["messages"].([]any)
	sysMsg, _ := sentMsgs[0].(map[string]any)
	if sysStr, ok := sysMsg["content"].(string); !ok || !strings.Contains(sysStr, "Part A") || !strings.Contains(sysStr, "Part B") {
		t.Fatalf("expected flattened string system prompt for local model, got %#v", sysMsg["content"])
	}

	// ConfigView.Configured must be true when LocalLLM is enabled even without OpenRouter API key.
	if !cfg.View(t.TempDir()).Configured {
		t.Fatalf("expected ConfigView.Configured=true when LocalLLM.Enabled=true")
	}

	// Engine doctor reports ok for reachable server and warn for unreachable server.
	tmp := t.TempDir()
	eng, err := Open(filepath.Join(tmp, "assistant.db"), filepath.Join(tmp, "assistant.json"), filepath.Join(tmp, "vault"), &fakeDeps{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = eng.Close() }()

	if c := eng.checkLocalLLMDoctor(t.Context(), cfg); c.Status != "ok" {
		t.Fatalf("expected local_llm doctor ok, got %+v", c)
	}
	badCfg := cfg
	badCfg.LocalLLM.BaseURL = "http://127.0.0.1:1/v1"
	if c := eng.checkLocalLLMDoctor(t.Context(), badCfg); c.Status != "warn" {
		t.Fatalf("expected unreachable local_llm doctor warn, got %+v", c)
	}
}

func TestVaultHybridSearchAndChunking(t *testing.T) {
	chunks := chunkMarkdown("# Project Alpha\nIntro\n\n## Budget\n"+strings.Repeat("word ", 500), 1200)
	if len(chunks) < 3 {
		t.Fatalf("expected multi-chunk heading split, got %d", len(chunks))
	}
	if chunks[0].heading != "Project Alpha" || !strings.Contains(chunks[1].heading, "Project Alpha > Budget") {
		t.Fatalf("unexpected heading hierarchy: %+v", chunks[:2])
	}

	// Mock Ollama embedder that maps automotive semantics ("automobile/vehicle/garage")
	// to e0 and cooking ("espresso") to e1 cleanly without lexical overlap.
	var failEmbed bool
	embedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failEmbed || r.URL.Path != "/api/embed" {
			http.Error(w, "embed down", http.StatusServiceUnavailable)
			return
		}
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		vecs := make([][]float64, len(req.Input))
		for i, text := range req.Input {
			low := strings.ToLower(text)
			switch {
			case strings.Contains(low, "vehicle") || strings.Contains(low, "garage") || strings.Contains(low, "automobile") || strings.Contains(low, "car repair"):
				vecs[i] = []float64{1, 0, 0}
			case strings.Contains(low, "espresso") || strings.Contains(low, "beans"):
				vecs[i] = []float64{0, 1, 0}
			default:
				vecs[i] = []float64{0, 0, 1}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": vecs})
	}))
	defer embedSrv.Close()

	tmp := t.TempDir()
	eng, err := Open(filepath.Join(tmp, "assistant.db"), filepath.Join(tmp, "assistant.json"), filepath.Join(tmp, "vault"), &fakeDeps{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = eng.Close() }()

	extraRoot := filepath.Join(tmp, "personal")
	if err := os.MkdirAll(filepath.Join(extraRoot, "Notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	autoNote := filepath.Join(extraRoot, "Notes", "Honda.md")
	if err := os.WriteFile(autoNote, []byte("# Maintenance\nRotate tires in the garage and inspect vehicle brakes.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	coffeeNote := filepath.Join(extraRoot, "Notes", "Coffee.md")
	if err := os.WriteFile(coffeeNote, []byte("# Espresso\nDial in dark roast beans at 9 bars.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultConfig()
	cfg.ExtraVaults = []VaultMount{{Name: "personal", Path: extraRoot, ReadOnly: true}}
	cfg.Embeddings = memory.EmbedConfig{
		Enabled:  true,
		Provider: "ollama",
		BaseURL:  embedSrv.URL,
		Model:    "nomic-embed-text",
	}
	if _, err := eng.UpdateConfig(cfg, false); err != nil {
		t.Fatal(err)
	}
	eng.SyncAllVaultIndexes(t.Context())

	// Query "car repair" has zero lexical overlap with Honda.md ("vehicle"/"garage") -> semantic search finds it!
	hits, err := eng.SearchVaults("all", "car repair", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 || hits[0].Path != "Notes/Honda.md" || hits[0].Vault != "personal" {
		t.Fatalf("expected Honda.md top semantic hit, got %+v", hits)
	}
	if hits[0].Heading != "Maintenance" {
		t.Fatalf("expected heading preserved on hit, got %+v", hits[0])
	}

	// Incremental delete removes deleted note from index.
	_ = os.Remove(coffeeNote)
	hitsCoffee, err := eng.SearchVaults("personal", "Espresso", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hitsCoffee) != 0 {
		t.Fatalf("expected deleted Coffee.md removed after incremental sync, got %+v", hitsCoffee)
	}

	// Doctor check reports ready with embedded chunk counts.
	if doc := eng.checkEmbeddingsDoctor(t.Context(), eng.Config()); doc.Status != "ok" {
		t.Fatalf("expected embeddings doctor ok, got %+v", doc)
	}

	// Graceful FTS5 fallback when embedder goes down.
	failEmbed = true
	ftsHits, err := eng.SearchVaults("personal", "brakes", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ftsHits) == 0 || ftsHits[0].Path != "Notes/Honda.md" {
		t.Fatalf("expected graceful FTS5 fallback hit when embedder down, got %+v", ftsHits)
	}
}
