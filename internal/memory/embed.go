package memory

import (
	"bytes"
	"cmp"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/bdatdo0601/fleet/internal/paths"
	"github.com/bdatdo0601/fleet/internal/registry"
)

const (
	DefaultEmbedProvider  = "ollama"
	DefaultEmbedBaseURL   = "http://127.0.0.1:11434"
	DefaultEmbedModel     = "nomic-embed-text"
	DefaultEmbedKeepAlive = "5m"
)

// EmbedConfig configures the local or remote embedding model used for hybrid semantic search.
type EmbedConfig struct {
	Enabled   bool   `json:"enabled"`
	Provider  string `json:"provider"`
	BaseURL   string `json:"base_url"`
	Model     string `json:"model"`
	APIKey    string `json:"api_key,omitempty"`
	KeepAlive string `json:"keep_alive,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

// EmbedStatus reports non-secret embedding configuration, index progress, and worker health.
type EmbedStatus struct {
	Enabled        bool   `json:"enabled"`
	Configured     bool   `json:"configured"`
	Provider       string `json:"provider"`
	BaseURL        string `json:"base_url"`
	Model          string `json:"model"`
	APIKeyHint     string `json:"api_key_hint"`
	KeepAlive      string `json:"keep_alive,omitempty"`
	EmbeddedCount  int    `json:"embedded_count"`
	PendingCount   int    `json:"pending_count"`
	TotalCount     int    `json:"total_count"`
	LastRunAt      string `json:"last_run_at,omitempty"`
	LastError      string `json:"last_error,omitempty"`
	ProcessedCount int    `json:"processed_count"`
}

func inferEmbedProvider(baseURL, provider string) string {
	p := strings.ToLower(strings.TrimSpace(provider))
	if p != "" {
		return p
	}
	u := strings.ToLower(strings.TrimSpace(baseURL))
	switch {
	case strings.Contains(u, "api.openai.com"):
		return "openai"
	case strings.Contains(u, "openrouter.ai"):
		return "openai"
	case u == "", strings.Contains(u, ":11434"):
		return "ollama"
	default:
		return "openai"
	}
}

// NormalizeEmbedConfig validates and fills defaults for an EmbedConfig.
func NormalizeEmbedConfig(cfg, prev EmbedConfig) (EmbedConfig, error) {
	cfg.Provider = inferEmbedProvider(cfg.BaseURL, cfg.Provider)
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	cfg.Model = strings.TrimSpace(cfg.Model)
	cfg.APIKey = strings.TrimSpace(cfg.APIKey)
	cfg.KeepAlive = strings.TrimSpace(cfg.KeepAlive)

	switch cfg.Provider {
	case "ollama":
		cfg.BaseURL = cmp.Or(cfg.BaseURL, DefaultEmbedBaseURL)
		cfg.Model = cmp.Or(cfg.Model, DefaultEmbedModel)
		cfg.KeepAlive = cmp.Or(cfg.KeepAlive, DefaultEmbedKeepAlive)
	case "openai", "custom":
		cfg.BaseURL = cmp.Or(cfg.BaseURL, DefaultOpenAIURL)
		cfg.Model = cmp.Or(cfg.Model, "text-embedding-3-small")
	default:
		return cfg, newMemoryErr(fmt.Sprintf("unknown embedding provider %q", cfg.Provider), 400)
	}

	if !strings.HasPrefix(cfg.BaseURL, "http://") && !strings.HasPrefix(cfg.BaseURL, "https://") {
		return cfg, newMemoryErr("base_url must start with http:// or https://", 400)
	}
	if cfg.APIKey == "" && prev.APIKey != "" && (prev.Provider == cfg.Provider || prev.BaseURL == cfg.BaseURL) {
		cfg.APIKey = prev.APIKey
	}
	if cfg.Enabled && cfg.Provider == "openai" && strings.Contains(strings.ToLower(cfg.BaseURL), "api.openai.com") && cfg.APIKey == "" {
		return cfg, newMemoryErr("api_key is required for openai embeddings", 400)
	}
	cfg.UpdatedAt = registry.NowISO()
	return cfg, nil
}

// EncodeVector L2-normalizes vec and encodes it as little-endian float32 bytes.
func EncodeVector(vec []float32) []byte {
	if len(vec) == 0 {
		return nil
	}
	var sumSq float64
	for _, v := range vec {
		sumSq += float64(v) * float64(v)
	}
	if sumSq <= 0 || math.IsNaN(sumSq) || math.IsInf(sumSq, 0) {
		return nil
	}
	invNorm := float32(1.0 / math.Sqrt(sumSq))
	buf := make([]byte, len(vec)*4)
	for i, v := range vec {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(v*invNorm))
	}
	return buf
}

// DecodeVector decodes a little-endian float32 byte blob into a slice.
func DecodeVector(raw []byte) []float32 {
	if len(raw) == 0 || len(raw)%4 != 0 {
		return nil
	}
	n := len(raw) / 4
	out := make([]float32, n)
	for i := range n {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(raw[i*4:]))
	}
	return out
}

// DotProduct computes the dot product (cosine similarity for L2-normalized vectors) of a and b.
func DotProduct(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var sum float64
	for i := range a {
		sum += float64(a[i]) * float64(b[i])
	}
	return sum
}

func isOllamaEmbed(cfg EmbedConfig) bool {
	return cfg.Provider == "ollama" || strings.Contains(strings.ToLower(cfg.BaseURL), ":11434")
}

// EmbedTexts calls the configured Ollama or OpenAI-compatible embedding endpoint for texts.
func EmbedTexts(ctx context.Context, httpClient *http.Client, cfg EmbedConfig, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	var endpoint string
	var payload map[string]any
	ollama := isOllamaEmbed(cfg)
	if ollama {
		base := strings.TrimRight(cfg.BaseURL, "/")
		base = strings.TrimSuffix(base, "/v1")
		endpoint = base + "/api/embed"
		payload = map[string]any{
			"model": cfg.Model,
			"input": texts,
		}
		if cfg.KeepAlive != "" {
			payload["keep_alive"] = cfg.KeepAlive
		}
	} else {
		endpoint = strings.TrimRight(cfg.BaseURL, "/") + "/embeddings"
		payload = map[string]any{
			"model": cfg.Model,
			"input": texts,
		}
	}

	rawReq, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(rawReq))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("embed HTTP %d: %s", resp.StatusCode, TruncateRunes(strings.TrimSpace(string(respBytes)), 240))
	}
	return parseEmbedResponse(respBytes, ollama, len(texts))
}

func parseEmbedResponse(respBytes []byte, ollama bool, wantCount int) ([][]float32, error) {
	if ollama {
		var parsed struct {
			Embeddings [][]float32 `json:"embeddings"`
		}
		if err := json.Unmarshal(respBytes, &parsed); err != nil {
			return nil, fmt.Errorf("decode ollama embed response: %w", err)
		}
		if len(parsed.Embeddings) != wantCount {
			return nil, fmt.Errorf("embed returned %d vector(s), expected %d", len(parsed.Embeddings), wantCount)
		}
		return parsed.Embeddings, nil
	}
	var parsed struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBytes, &parsed); err != nil {
		return nil, fmt.Errorf("decode embed response: %w", err)
	}
	if len(parsed.Data) != wantCount {
		return nil, fmt.Errorf("embed returned %d vector(s), expected %d", len(parsed.Data), wantCount)
	}
	out := make([][]float32, wantCount)
	for _, item := range parsed.Data {
		if item.Index < 0 || item.Index >= wantCount {
			return nil, fmt.Errorf("embed response index %d out of range", item.Index)
		}
		out[item.Index] = item.Embedding
	}
	return out, nil
}

// Embedder manages async embedding backfill and query embedding for memory.db.
type Embedder struct {
	store      *Store
	configPath string
	httpClient *http.Client

	mu        sync.Mutex
	drainMu   sync.Mutex
	cfg       EmbedConfig
	lastRunAt string
	lastError string
	processed int

	notifyCh chan struct{}
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	running  bool
}

func loadJSONConfigFile(configPath string, dst any) {
	if configPath == "" {
		return
	}
	if raw, err := os.ReadFile(configPath); err == nil && len(bytes.TrimSpace(raw)) > 0 {
		_ = json.Unmarshal(raw, dst)
	}
}

// NewEmbedder constructs an Embedder and loads configPath if present.
func NewEmbedder(store *Store, configPath string) *Embedder {
	e := &Embedder{
		store:      store,
		configPath: configPath,
		httpClient: &http.Client{Timeout: 60 * time.Second},
		notifyCh:   make(chan struct{}, 1),
	}
	loadJSONConfigFile(configPath, &e.cfg)
	return e
}

func (e *Embedder) saveConfigLocked() error {
	if e.configPath == "" {
		return nil
	}
	data, err := json.MarshalIndent(e.cfg, "", "  ")
	if err != nil {
		return err
	}
	_, err = paths.WriteFileAtomic(e.configPath, append(data, '\n'), 0o600, nil, nil)
	return err
}

// Config returns a copy of the current EmbedConfig.
func (e *Embedder) Config() EmbedConfig {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cfg
}

// Configure validates, optionally tests, and saves embedding configuration.
func (e *Embedder) Configure(ctx context.Context, cfg EmbedConfig, testFirst bool) (EmbedStatus, error) {
	e.mu.Lock()
	prev := e.cfg
	e.mu.Unlock()

	norm, err := NormalizeEmbedConfig(cfg, prev)
	if err != nil {
		return EmbedStatus{}, err
	}
	if norm.Enabled && testFirst {
		if _, tErr := EmbedTexts(ctx, e.httpClient, norm, []string{"fleet memory embedding health check"}); tErr != nil {
			return EmbedStatus{}, newMemoryErr("embedding preflight failed: "+tErr.Error(), 400)
		}
	}

	e.mu.Lock()
	e.cfg = norm
	e.lastError = ""
	saveErr := e.saveConfigLocked()
	e.mu.Unlock()
	if saveErr != nil {
		return EmbedStatus{}, saveErr
	}
	if norm.Enabled {
		e.Notify()
	}
	return e.Status(), nil
}

// Disable turns off background embedding and hybrid vector search while preserving endpoint settings.
func (e *Embedder) Disable() (EmbedStatus, error) {
	e.mu.Lock()
	e.cfg.Enabled = false
	e.cfg.UpdatedAt = registry.NowISO()
	saveErr := e.saveConfigLocked()
	e.mu.Unlock()
	if saveErr != nil {
		return EmbedStatus{}, saveErr
	}
	return e.Status(), nil
}

// Status returns non-secret embedding config and index progress.
func (e *Embedder) Status() EmbedStatus {
	e.mu.Lock()
	cfg := e.cfg
	lastRun := e.lastRunAt
	lastErr := e.lastError
	proc := e.processed
	e.mu.Unlock()

	var embedded, pending, total int
	if e.store != nil {
		embedded, pending, total, _ = e.store.EmbeddingCounts(cfg.Model)
	}
	return EmbedStatus{
		Enabled:        cfg.Enabled,
		Configured:     cfg.BaseURL != "" && cfg.Model != "",
		Provider:       cfg.Provider,
		BaseURL:        cfg.BaseURL,
		Model:          cfg.Model,
		APIKeyHint:     MaskAPIKey(cfg.APIKey),
		KeepAlive:      cfg.KeepAlive,
		EmbeddedCount:  embedded,
		PendingCount:   pending,
		TotalCount:     total,
		LastRunAt:      lastRun,
		LastError:      lastErr,
		ProcessedCount: proc,
	}
}

// QueryEmbedFunc returns a query embedding callback suitable for Store.SetQueryEmbedder.
func (e *Embedder) QueryEmbedFunc() func(string) ([]float32, string, error) {
	return func(query string) ([]float32, string, error) {
		cfg := e.Config()
		if !cfg.Enabled || strings.TrimSpace(cfg.Model) == "" {
			return nil, "", errors.New("embeddings disabled")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
		defer cancel()
		vecs, err := EmbedTexts(ctx, e.httpClient, cfg, []string{query})
		if err != nil || len(vecs) == 0 || len(vecs[0]) == 0 {
			return nil, "", cmp.Or(err, errors.New("empty embedding"))
		}
		norm := DecodeVector(EncodeVector(vecs[0]))
		if len(norm) == 0 {
			return nil, "", errors.New("zero-norm embedding")
		}
		return norm, cfg.Model, nil
	}
}

// Start launches the background embedding backfill worker.
func (e *Embedder) Start(parent context.Context) {
	e.mu.Lock()
	if e.running {
		e.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(parent)
	e.cancel = cancel
	e.running = true
	e.wg.Add(1)
	e.mu.Unlock()

	go e.loop(ctx)
	e.Notify()
}

// Stop stops the background embedding worker and waits for exit.
func (e *Embedder) Stop() {
	e.mu.Lock()
	if !e.running {
		e.mu.Unlock()
		return
	}
	cancel := e.cancel
	e.cancel = nil
	e.running = false
	e.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	e.wg.Wait()
}

// Notify wakes the background embedding worker.
func (e *Embedder) Notify() {
	select {
	case e.notifyCh <- struct{}{}:
	default:
	}
}

func (e *Embedder) loop(ctx context.Context) {
	defer e.wg.Done()
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-e.notifyCh:
			select {
			case <-ctx.Done():
				return
			case <-time.After(1200 * time.Millisecond):
			}
			_, _ = e.BackfillNow(ctx, 32)
		case <-ticker.C:
			_, _ = e.BackfillNow(ctx, 32)
		}
	}
}

// BackfillNow embeds up to limit active memories missing an embedding for the active model.
func (e *Embedder) BackfillNow(ctx context.Context, limit int) (int, error) {
	e.drainMu.Lock()
	defer e.drainMu.Unlock()

	cfg := e.Config()
	if !cfg.Enabled || e.store == nil {
		return 0, nil
	}
	if limit <= 0 || limit > 128 {
		limit = 32
	}
	items, err := e.store.ListNeedingEmbedding(cfg.Model, limit)
	if err != nil {
		e.recordRunError(err.Error())
		return 0, err
	}
	if len(items) == 0 {
		return 0, nil
	}
	texts := make([]string, len(items))
	for i, it := range items {
		texts[i] = TruncateRunes(it.Scope+" "+it.Key+"\n"+it.Summary+"\n"+it.Body, 2000)
	}
	vecs, err := EmbedTexts(ctx, e.httpClient, cfg, texts)
	if err != nil {
		e.recordRunError(err.Error())
		return 0, err
	}
	saved := 0
	for i, it := range items {
		blob := EncodeVector(vecs[i])
		if len(blob) == 0 {
			continue
		}
		if sErr := e.store.SetEmbedding(it.ID, cfg.Model, blob); sErr != nil {
			e.recordRunError(sErr.Error())
			return saved, sErr
		}
		saved++
	}
	e.mu.Lock()
	e.lastRunAt = registry.NowISO()
	e.lastError = ""
	e.processed += saved
	e.mu.Unlock()
	return saved, nil
}

func (e *Embedder) recordRunError(msg string) {
	e.mu.Lock()
	e.lastRunAt = registry.NowISO()
	e.lastError = msg
	e.mu.Unlock()
}
