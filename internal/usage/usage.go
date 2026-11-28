// Package usage collects quota/rate-limit and activity metrics for claude, codex, and antigravity.
package usage

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bdatdo0601/fleet/internal/agents"
	"github.com/bdatdo0601/fleet/internal/client"
	"github.com/bdatdo0601/fleet/internal/paths"
	"github.com/bdatdo0601/fleet/internal/registry"
	_ "modernc.org/sqlite" // register pure-Go SQLite driver for database/sql
)

// CollectOpts configures CollectAll.
type CollectOpts struct {
	Home      string
	Device    string
	CodexBin  string
	SkipCodex bool
}

func resolveHome(home string) string {
	if strings.TrimSpace(home) != "" {
		return home
	}
	h, _ := os.UserHomeDir()
	return h
}

func resolveDeviceName(override string) string {
	if strings.TrimSpace(override) != "" {
		return override
	}
	if n, err := client.LoadNode(); err == nil && n.Name != "" {
		return n.Name
	}
	host, _ := os.Hostname()
	return cmp.Or(host, "unknown")
}

func findExec(override, name string, cands []string) string {
	if override != "" {
		if fi, err := os.Stat(override); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return override
		}
		return ""
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	for _, p := range cands {
		if p == "" {
			continue
		}
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return p
		}
	}
	return ""
}

func windowLabel(mins int) string {
	switch {
	case mins <= 0:
		return "window"
	case mins%1440 == 0:
		return fmt.Sprintf("%dd", mins/1440)
	case mins%60 == 0:
		return fmt.Sprintf("%dh", mins/60)
	default:
		return fmt.Sprintf("%dm", mins)
	}
}

func parseResetTimestamp(raw any) string {
	switch v := raw.(type) {
	case float64:
		if v > 0 {
			if v > 1e12 {
				return time.UnixMilli(int64(v)).UTC().Format(time.RFC3339)
			}
			return time.Unix(int64(v), 0).UTC().Format(time.RFC3339)
		}
	case int64:
		if v > 0 {
			if v > 1e12 {
				return time.UnixMilli(v).UTC().Format(time.RFC3339)
			}
			return time.Unix(v, 0).UTC().Format(time.RFC3339)
		}
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return ""
		}
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
			if t, err := time.Parse(layout, s); err == nil {
				return t.UTC().Format(time.RFC3339)
			}
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil && f > 0 {
			return parseResetTimestamp(f)
		}
	}
	return ""
}

type codexRPCEnvelope struct {
	ID     *int            `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type codexRPCClient struct {
	cancel    context.CancelFunc
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	stderrBuf bytes.Buffer
	msgCh     chan codexRPCEnvelope
	pending   map[int]codexRPCEnvelope
	stdoutWG  sync.WaitGroup
	waitOnce  sync.Once
}

func startCodexAppServer(ctx context.Context, bin string) (*codexRPCClient, error) {
	runCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	cmd := exec.CommandContext(runCtx, bin, "app-server", "--listen", "stdio://")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	c := &codexRPCClient{
		cancel:  cancel,
		cmd:     cmd,
		stdin:   stdin,
		msgCh:   make(chan codexRPCEnvelope, 16),
		pending: make(map[int]codexRPCEnvelope),
	}
	cmd.Stderr = &c.stderrBuf
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, err
	}
	c.stdoutWG.Go(func() {
		defer close(c.msgCh)
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			var env codexRPCEnvelope
			if json.Unmarshal(sc.Bytes(), &env) == nil && env.ID != nil {
				select {
				case c.msgCh <- env:
				case <-runCtx.Done():
					return
				}
			}
		}
	})
	return c, nil
}

func (c *codexRPCClient) wait() {
	c.waitOnce.Do(func() {
		_ = c.stdin.Close()
		c.cancel()
		c.stdoutWG.Wait()
		_ = c.cmd.Wait()
	})
}

func (c *codexRPCClient) writeRPC(obj any) error {
	b, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	_, err = c.stdin.Write(append(b, '\n'))
	return err
}

func (c *codexRPCClient) awaitID(ctx context.Context, want int) (codexRPCEnvelope, error) {
	if env, ok := c.pending[want]; ok {
		delete(c.pending, want)
		return env, nil
	}
	for {
		select {
		case <-ctx.Done():
			return codexRPCEnvelope{}, ctx.Err()
		case env, ok := <-c.msgCh:
			if !ok {
				c.wait()
				detail := strings.TrimSpace(c.stderrBuf.String())
				if detail != "" {
					return codexRPCEnvelope{}, fmt.Errorf("app-server closed pipe: %s", detail)
				}
				return codexRPCEnvelope{}, fmt.Errorf("app-server closed pipe")
			}
			if env.ID == nil {
				continue
			}
			if *env.ID == want {
				return env, nil
			}
			c.pending[*env.ID] = env
		}
	}
}

func applyCodexRateLimits(base *agents.HarnessUsage, metrics map[string]any, raw json.RawMessage) error {
	type codexWindow struct {
		UsedPercent        float64 `json:"usedPercent"`
		WindowDurationMins int     `json:"windowDurationMins"`
		ResetsAt           any     `json:"resetsAt"`
	}
	var rl struct {
		OrdinaryUsageAllowed *bool `json:"ordinaryUsageAllowed"`
		RateLimits           *struct {
			LimitID              string       `json:"limitId"`
			PlanType             string       `json:"planType"`
			Primary              *codexWindow `json:"primary"`
			Secondary            *codexWindow `json:"secondary"`
			RateLimitReachedType string       `json:"rateLimitReachedType"`
			SpendControlReached  bool         `json:"spendControlReached"`
			Credits              *struct {
				HasCredits bool   `json:"hasCredits"`
				Unlimited  bool   `json:"unlimited"`
				Balance    string `json:"balance"`
			} `json:"credits"`
		} `json:"rateLimits"`
	}
	if err := json.Unmarshal(raw, &rl); err != nil {
		return err
	}
	if rl.RateLimits != nil {
		base.Plan = rl.RateLimits.PlanType
		for _, cw := range []*codexWindow{rl.RateLimits.Secondary, rl.RateLimits.Primary} {
			if cw == nil {
				continue
			}
			base.Windows = append(base.Windows, agents.UsageWindow{
				Label:       windowLabel(cw.WindowDurationMins),
				UsedPercent: cw.UsedPercent,
				WindowMins:  cw.WindowDurationMins,
				ResetsAt:    parseResetTimestamp(cw.ResetsAt),
			})
		}
		if rl.RateLimits.Credits != nil && rl.RateLimits.Credits.HasCredits {
			metrics["credits_balance"] = rl.RateLimits.Credits.Balance
			if rl.RateLimits.Credits.Unlimited {
				metrics["credits_unlimited"] = true
			}
		}
		if rl.RateLimits.RateLimitReachedType != "" {
			base.Detail = rl.RateLimits.RateLimitReachedType
		}
	}
	if rl.OrdinaryUsageAllowed != nil && !*rl.OrdinaryUsageAllowed && base.Detail == "" {
		base.Detail = "rate limit reached"
	}
	return nil
}

func applyCodexTokenUsage(metrics map[string]any, tokResp codexRPCEnvelope) {
	if tokResp.Error != nil || len(tokResp.Result) == 0 {
		return
	}
	var tu struct {
		Summary *struct {
			LifetimeTokens    *int64 `json:"lifetimeTokens"`
			PeakDailyTokens   *int64 `json:"peakDailyTokens"`
			CurrentStreakDays *int64 `json:"currentStreakDays"`
		} `json:"summary"`
	}
	if json.Unmarshal(tokResp.Result, &tu) != nil || tu.Summary == nil {
		return
	}
	if tu.Summary.LifetimeTokens != nil {
		metrics["lifetime_tokens"] = *tu.Summary.LifetimeTokens
	}
	if tu.Summary.PeakDailyTokens != nil {
		metrics["peak_daily_tokens"] = *tu.Summary.PeakDailyTokens
	}
	if tu.Summary.CurrentStreakDays != nil {
		metrics["current_streak_days"] = *tu.Summary.CurrentStreakDays
	}
}

func setCodexError(base agents.HarnessUsage, err error) agents.HarnessUsage {
	base.Status = "error"
	base.Detail = err.Error()
	return base
}

// CollectCodex queries Codex rate limits and token usage via `codex app-server --listen stdio://`.
func CollectCodex(ctx context.Context, codexBin, home string) agents.HarnessUsage {
	base := agents.HarnessUsage{
		Harness:   "codex",
		Source:    "codex-app-server",
		Status:    "unavailable",
		Windows:   []agents.UsageWindow{},
		UpdatedAt: registry.NowISO(),
	}
	if os.Geteuid() == 0 {
		base.Detail = "skipped as root"
		return base
	}
	h := resolveHome(home)
	bin := findExec(codexBin, "codex", []string{
		filepath.Join(h, ".local", "bin", "codex"),
		"/opt/homebrew/bin/codex",
		"/usr/local/bin/codex",
	})
	if bin == "" {
		base.Detail = "codex CLI not installed"
		return base
	}

	rpc, err := startCodexAppServer(ctx, bin)
	if err != nil {
		return setCodexError(base, err)
	}
	defer rpc.wait()

	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	if err := rpc.writeRPC(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"clientInfo": map[string]string{"name": "fleet-usage", "version": "0.1"},
		},
	}); err != nil {
		return setCodexError(base, err)
	}
	if _, err := rpc.awaitID(reqCtx, 1); err != nil {
		return setCodexError(base, err)
	}
	_ = rpc.writeRPC(map[string]any{"jsonrpc": "2.0", "method": "initialized", "params": map[string]any{}})

	if err := rpc.writeRPC(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "account/rateLimits/read", "params": nil}); err != nil {
		return setCodexError(base, err)
	}
	_ = rpc.writeRPC(map[string]any{"jsonrpc": "2.0", "id": 3, "method": "account/usage/read", "params": nil})

	rlResp, err := rpc.awaitID(reqCtx, 2)
	if err != nil {
		return setCodexError(base, err)
	}
	if rlResp.Error != nil {
		base.Status = "error"
		base.Detail = rlResp.Error.Message
		return base
	}

	metrics := map[string]any{}
	if err := applyCodexRateLimits(&base, metrics, rlResp.Result); err != nil {
		return setCodexError(base, err)
	}
	if tokResp, tErr := rpc.awaitID(reqCtx, 3); tErr == nil {
		applyCodexTokenUsage(metrics, tokResp)
	}
	if len(metrics) > 0 {
		base.Metrics = metrics
	}
	base.Status = "ok"
	return base
}

// ClaudeCachePath returns the local file storing the last captured Claude Code rate_limits snapshot.
func ClaudeCachePath(home string) string {
	return filepath.Join(resolveHome(home), ".fleet-cache", "claude-usage.json")
}

// ParseClaudeStatusLineJSON extracts rate_limits and session metrics from Claude Code's statusLine stdin JSON.
func ParseClaudeStatusLineJSON(raw []byte) (agents.HarnessUsage, bool) {
	var payload struct {
		Model struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
		} `json:"model"`
		Cost struct {
			TotalCostUSD *float64 `json:"total_cost_usd"`
		} `json:"cost"`
		ContextWindow struct {
			UsedPercentage      *float64 `json:"used_percentage"`
			RemainingPercentage *float64 `json:"remaining_percentage"`
		} `json:"context_window"`
		RateLimits map[string]*struct {
			UsedPercentage *float64 `json:"used_percentage"`
			ResetsAt       any      `json:"resets_at"`
		} `json:"rate_limits"`
	}
	if json.Unmarshal(raw, &payload) != nil || len(payload.RateLimits) == 0 {
		return agents.HarnessUsage{}, false
	}

	windowSpecs := []struct {
		key   string
		label string
		mins  int
	}{
		{"five_hour", "5h", 300},
		{"seven_day", "7d", 10080},
		{"seven_day_sonnet", "7d sonnet", 10080},
		{"seven_day_opus", "7d opus", 10080},
	}

	var windows []agents.UsageWindow
	for _, spec := range windowSpecs {
		rl := payload.RateLimits[spec.key]
		if rl == nil || rl.UsedPercentage == nil {
			continue
		}
		windows = append(windows, agents.UsageWindow{
			Label:       spec.label,
			UsedPercent: *rl.UsedPercentage,
			WindowMins:  spec.mins,
			ResetsAt:    parseResetTimestamp(rl.ResetsAt),
		})
	}
	if len(windows) == 0 {
		return agents.HarnessUsage{}, false
	}

	metrics := map[string]any{}
	if model := cmp.Or(payload.Model.DisplayName, payload.Model.ID); model != "" {
		metrics["model"] = model
	}
	if payload.Cost.TotalCostUSD != nil {
		metrics["session_cost_usd"] = math.Round(*payload.Cost.TotalCostUSD*10000) / 10000
	}
	if payload.ContextWindow.UsedPercentage != nil {
		metrics["context_used_pct"] = math.Round(*payload.ContextWindow.UsedPercentage*10) / 10
	} else if payload.ContextWindow.RemainingPercentage != nil {
		metrics["context_used_pct"] = math.Round((100-*payload.ContextWindow.RemainingPercentage)*10) / 10
	}

	out := agents.HarnessUsage{
		Harness:   "claude",
		Source:    "claude-statusline",
		Status:    "ok",
		Windows:   windows,
		UpdatedAt: registry.NowISO(),
	}
	if len(metrics) > 0 {
		out.Metrics = metrics
	}
	return out, true
}

func readClaudeCache(cachePath string) (agents.HarnessUsage, bool) {
	raw, err := os.ReadFile(cachePath)
	if err != nil {
		return agents.HarnessUsage{}, false
	}
	var cached agents.HarnessUsage
	if json.Unmarshal(raw, &cached) != nil || cached.Harness == "" {
		return agents.HarnessUsage{}, false
	}
	if cached.Windows == nil {
		cached.Windows = []agents.UsageWindow{}
	}
	return cached, true
}

func claudeUsageWindowsEqual(a, b []agents.UsageWindow) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Label != b[i].Label || math.Round(a[i].UsedPercent*10) != math.Round(b[i].UsedPercent*10) || a[i].ResetsAt != b[i].ResetsAt {
			return false
		}
	}
	return true
}

func readStdinWithTimeout(r io.Reader, timeout time.Duration) []byte {
	if r == nil {
		return nil
	}
	ch := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(io.LimitReader(r, 1024*1024))
		ch <- b
	}()
	select {
	case b := <-ch:
		return b
	case <-time.After(timeout):
		return nil
	}
}

// HandleClaudeStatusLine tees Claude Code's statusLine stdin to an optional wrapped command, caches rate_limits, and pushes updates to the lighthouse.
func HandleClaudeStatusLine(stdin io.Reader, stdout, stderr io.Writer, wrapCmd, home, apiURL string) error {
	raw := readStdinWithTimeout(stdin, 2500*time.Millisecond)

	if strings.TrimSpace(wrapCmd) != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "sh", "-c", wrapCmd)
		cmd.Stdin = bytes.NewReader(raw)
		cmd.Stdout = stdout
		cmd.Stderr = stderr
		_ = cmd.Run()
	}

	snap, ok := ParseClaudeStatusLineJSON(raw)
	if !ok {
		return nil
	}
	snap.Device = resolveDeviceName("")

	cachePath := ClaudeCachePath(home)
	prev, hasPrev := readClaudeCache(cachePath)
	_, _ = paths.WriteFileAtomic(cachePath, func() []byte {
		b, _ := json.MarshalIndent(snap, "", "  ")
		return append(b, '\n')
	}(), 0o600, nil, nil)

	shouldPush := !hasPrev || !claudeUsageWindowsEqual(prev.Windows, snap.Windows)
	if !shouldPush {
		if prevTime, err := time.Parse(time.RFC3339, prev.UpdatedAt); err != nil || time.Since(prevTime) >= 30*time.Second {
			shouldPush = true
		}
	}
	if shouldPush {
		api, err := client.ResolveAPI(apiURL)
		if err == nil && api != "" {
			_ = client.Call(api, "POST", "/v1/usage", snap, 1500*time.Millisecond, nil, nil)
		}
	}
	return nil
}

// CollectClaude reads the locally cached Claude Code rate_limits snapshot captured by statusLine.
func CollectClaude(home string) agents.HarnessUsage {
	if cached, ok := readClaudeCache(ClaudeCachePath(home)); ok && cached.Status == "ok" {
		return cached
	}
	return agents.HarnessUsage{
		Harness:   "claude",
		Source:    "claude-statusline",
		Status:    "unavailable",
		Detail:    "waiting for next Claude turn (captured automatically via statusLine)",
		Windows:   []agents.UsageWindow{},
		UpdatedAt: registry.NowISO(),
	}
}

func parseAgyUsageCLIOutput(raw []byte) (agents.HarnessUsage, bool) {
	// Flexible parser for optional `antigravity-usage --json` output.
	var obj map[string]any
	if json.Unmarshal(raw, &obj) != nil {
		return agents.HarnessUsage{}, false
	}
	var windows []agents.UsageWindow
	addFromSlice := func(items []any) {
		for _, it := range items {
			m, ok := it.(map[string]any)
			if !ok {
				continue
			}
			label, _ := m["label"].(string)
			if label == "" {
				label, _ = m["name"].(string)
			}
			if label == "" {
				label, _ = m["model"].(string)
			}
			pct := -1.0
			if v, ok := m["used_percent"].(float64); ok {
				pct = v
			} else if v, ok := m["usedPercentage"].(float64); ok {
				pct = v
			} else if rem, ok := m["remainingPercent"].(float64); ok {
				pct = 100 - rem
			} else if rem, ok := m["remainingFraction"].(float64); ok {
				pct = (1 - rem) * 100
			}
			if label != "" && pct >= 0 {
				resets := parseResetTimestamp(cmp.Or(m["resets_at"], m["resetTime"]))
				windows = append(windows, agents.UsageWindow{
					Label:       label,
					UsedPercent: math.Round(pct*10) / 10,
					ResetsAt:    resets,
				})
			}
		}
	}
	for _, key := range []string{"windows", "quotas", "models"} {
		if arr, ok := obj[key].([]any); ok {
			addFromSlice(arr)
		}
	}
	if len(windows) == 0 {
		return agents.HarnessUsage{}, false
	}
	plan, _ := obj["plan"].(string)
	return agents.HarnessUsage{
		Harness:   "antigravity",
		Source:    "antigravity-usage-cli",
		Plan:      plan,
		Status:    "ok",
		Windows:   windows,
		UpdatedAt: registry.NowISO(),
	}, true
}

// CollectAntigravity collects local Antigravity activity stats from conversation_summaries.db and optional antigravity-usage CLI.
func CollectAntigravity(ctx context.Context, home string) agents.HarnessUsage {
	h := resolveHome(home)
	now := registry.NowISO()
	out := agents.HarnessUsage{
		Harness:   "antigravity",
		Source:    "antigravity-local",
		Status:    "unavailable",
		Windows:   []agents.UsageWindow{},
		UpdatedAt: now,
	}

	metrics := map[string]any{}
	dbPath := filepath.Join(h, ".gemini", "antigravity", "conversation_summaries.db")
	if _, err := os.Stat(dbPath); err == nil {
		if db, oErr := sql.Open("sqlite", "file:"+dbPath+"?mode=ro&_pragma=busy_timeout(2000)"); oErr == nil {
			var total, conv24h, steps24h, active int
			err := db.QueryRowContext(ctx, `
				SELECT
					COUNT(*),
					COALESCE(SUM(CASE WHEN datetime(last_modified_time) >= datetime('now', '-1 day') THEN 1 ELSE 0 END), 0),
					COALESCE(SUM(CASE WHEN datetime(last_modified_time) >= datetime('now', '-1 day') THEN step_count ELSE 0 END), 0),
					COALESCE(SUM(CASE WHEN not_fully_idle = 1 THEN 1 ELSE 0 END), 0)
				FROM conversation_summaries
			`).Scan(&total, &conv24h, &steps24h, &active)
			_ = db.Close()
			if err == nil {
				metrics["conversations_total"] = total
				metrics["conversations_24h"] = conv24h
				metrics["steps_24h"] = steps24h
				metrics["active_conversations"] = active
				out.Status = "ok"
				out.Detail = "quota N/A headless (check /usage in agy)"
			}
		}
	}

	cliBin := findExec(os.Getenv("FLEET_AGY_USAGE_BIN"), "antigravity-usage", []string{
		filepath.Join(h, ".local", "bin", "antigravity-usage"),
		"/opt/homebrew/bin/antigravity-usage",
		"/usr/local/bin/antigravity-usage",
	})
	if cliBin != "" {
		runCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		raw, rErr := exec.CommandContext(runCtx, cliBin, "--json").Output()
		cancel()
		if rErr == nil {
			if parsed, ok := parseAgyUsageCLIOutput(raw); ok {
				if len(metrics) > 0 {
					parsed.Metrics = metrics
				}
				return parsed
			}
		}
	}

	if len(metrics) > 0 {
		out.Metrics = metrics
	} else {
		out.Detail = "Antigravity data not found"
	}
	return out
}

// CollectAll collects usage snapshots for claude, codex, and antigravity concurrently.
func CollectAll(ctx context.Context, opts CollectOpts) []agents.HarnessUsage {
	home := resolveHome(opts.Home)
	dev := resolveDeviceName(opts.Device)
	res := make([]agents.HarnessUsage, 3)

	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		u := CollectClaude(home)
		if u.Device == "" {
			u.Device = dev
		}
		res[0] = u
	}()
	go func() {
		defer wg.Done()
		if opts.SkipCodex {
			res[1] = agents.HarnessUsage{
				Harness:   "codex",
				Device:    dev,
				Source:    "codex-app-server",
				Status:    "unavailable",
				Detail:    "skipped as root",
				Windows:   []agents.UsageWindow{},
				UpdatedAt: registry.NowISO(),
			}
			return
		}
		u := CollectCodex(ctx, opts.CodexBin, home)
		u.Device = dev
		res[1] = u
	}()
	go func() {
		defer wg.Done()
		u := CollectAntigravity(ctx, home)
		u.Device = dev
		res[2] = u
	}()
	wg.Wait()
	return res
}

// ReportToLighthouse sends collected usage snapshots to POST /v1/usage.
func ReportToLighthouse(apiURL string, items []agents.HarnessUsage) error {
	return ReportToLighthouseContext(context.Background(), apiURL, items)
}

// ReportToLighthouseContext sends collected usage snapshots to POST /v1/usage using ctx.
func ReportToLighthouseContext(ctx context.Context, apiURL string, items []agents.HarnessUsage) error {
	if len(items) == 0 {
		return nil
	}
	api, err := client.ResolveAPI(apiURL)
	if err != nil {
		return err
	}
	return client.CallContext(ctx, api, "POST", "/v1/usage", map[string]any{"usage": items}, 10*time.Second, nil, nil)
}

// StartPeriodicReporter runs a best-effort background loop reporting harness usage to the lighthouse.
func StartPeriodicReporter(ctx context.Context, interval time.Duration, apiURL string) {
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	go func() {
		timer := time.NewTimer(2 * time.Second)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				items := CollectAll(ctx, CollectOpts{})
				_ = ReportToLighthouseContext(ctx, apiURL, items)
				timer.Reset(interval)
			}
		}
	}()
}
