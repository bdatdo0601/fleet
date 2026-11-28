package cli

import (
	"context"
	"flag"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"github.com/bdatdo0601/fleet/internal/agents"
	"github.com/bdatdo0601/fleet/internal/client"
	"github.com/bdatdo0601/fleet/internal/paths"
	"github.com/bdatdo0601/fleet/internal/system"
	"github.com/bdatdo0601/fleet/internal/usage"
)

func formatUsageBar(pct float64) string {
	clamped := max(0.0, min(100.0, pct))
	filled := int(math.Round(clamped / 10.0))
	return "[" + strings.Repeat("█", filled) + strings.Repeat("░", 10-filled) + "]"
}

func formatResetCountdown(ts string) string {
	if ts == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ""
	}
	secs := int(time.Until(t).Seconds())
	if secs <= 0 {
		return "resetting soon"
	}
	days := secs / 86400
	rem := secs % 86400
	hours := rem / 3600
	mins := (rem % 3600) / 60
	switch {
	case days > 0:
		return fmt.Sprintf("resets in %dd %dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("resets in %dh %dm", hours, mins)
	default:
		return fmt.Sprintf("resets in %dm", max(1, mins))
	}
}

func formatCompactUsageFooter(items []agents.HarnessUsage) string {
	if len(items) == 0 {
		return ""
	}
	var parts []string
	for _, u := range items {
		if len(u.Windows) > 0 {
			var wins []string
			for _, w := range u.Windows {
				wins = append(wins, fmt.Sprintf("%s %.0f%%", w.Label, w.UsedPercent))
			}
			label := u.Harness + " " + strings.Join(wins, " · ")
			if u.Plan != "" {
				label += " (" + u.Plan + ")"
			}
			parts = append(parts, label)
			continue
		}
		if convs, ok := u.Metrics["conversations_24h"]; ok {
			parts = append(parts, fmt.Sprintf("%s %v convs/24h", u.Harness, convs))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "Usage: " + strings.Join(parts, " | ")
}

func formatUsageMetrics(m map[string]any) string {
	if len(m) == 0 {
		return ""
	}
	var parts []string
	for _, k := range []string{"model", "session_cost_usd", "context_used_pct", "lifetime_tokens", "peak_daily_tokens", "current_streak_days", "credits_balance", "conversations_24h", "steps_24h", "conversations_total", "active_conversations"} {
		v, ok := m[k]
		if !ok || v == nil {
			continue
		}
		switch k {
		case "session_cost_usd":
			parts = append(parts, fmt.Sprintf("cost=$%v", v))
		case "context_used_pct":
			parts = append(parts, fmt.Sprintf("ctx=%v%%", v))
		default:
			parts = append(parts, fmt.Sprintf("%s=%v", k, v))
		}
	}
	return strings.Join(parts, " · ")
}

func printUsageTable(cio *IO, items []agents.HarnessUsage) {
	if len(items) == 0 {
		cio.printf("no harness usage snapshots yet\n")
		return
	}
	for i, u := range items {
		if i > 0 {
			cio.printf("\n")
		}
		header := strings.ToUpper(u.Harness)
		if u.Plan != "" {
			header += " (" + u.Plan + ")"
		}
		meta := []string{u.Status}
		if u.Device != "" {
			meta = append(meta, u.Device)
		}
		if u.Source != "" {
			meta = append(meta, u.Source)
		}
		if u.UpdatedAt != "" {
			ts := u.UpdatedAt
			meta = append(meta, ago(&ts))
		}
		cio.printf("%s  [%s]\n", header, strings.Join(meta, " · "))
		for _, w := range u.Windows {
			resetStr := formatResetCountdown(w.ResetsAt)
			if resetStr != "" {
				cio.printf("  %-10s %s %5.1f%%  (%s)\n", w.Label, formatUsageBar(w.UsedPercent), w.UsedPercent, resetStr)
			} else {
				cio.printf("  %-10s %s %5.1f%%\n", w.Label, formatUsageBar(w.UsedPercent), w.UsedPercent)
			}
		}
		if met := formatUsageMetrics(u.Metrics); met != "" {
			cio.printf("  metrics: %s\n", met)
		}
		if u.Detail != "" {
			cio.printf("  note:    %s\n", u.Detail)
		}
	}
}

func cmdUsage(args []string, cio *IO) error {
	fs := flag.NewFlagSet("usage", flag.ContinueOnError)
	cached := fs.Bool("cached", false, "skip local collection and read cached lighthouse snapshots")
	localOnly := fs.Bool("local", false, "collect locally without contacting the lighthouse")
	jsonOut := fs.Bool("json", false, "print JSON output")
	harnessFilter := fs.String("harness", "", "filter by harness (claude, codex, antigravity)")
	pos, err := parseFlagsInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(pos, " "))
	}
	if *cached && *localOnly {
		return fmt.Errorf("--cached and --local cannot be combined")
	}
	if *harnessFilter != "" && !paths.ValidHarness(*harnessFilter) {
		return fmt.Errorf("unknown harness %q (expected claude, codex, or antigravity)", *harnessFilter)
	}

	var items []agents.HarnessUsage
	if !*cached {
		items = usage.CollectAll(context.Background(), usage.CollectOpts{
			SkipCodex: system.IsRoot(),
		})
	}

	if !*localOnly {
		api, apiErr := client.ResolveAPI("")
		if apiErr == nil && api != "" {
			if !*cached && len(items) > 0 {
				_ = usage.ReportToLighthouse(api, items)
			}
			var resp struct {
				Usage []agents.HarnessUsage `json:"usage"`
			}
			if err := client.Call(api, "GET", "/v1/usage", nil, 10*time.Second, nil, &resp); err == nil {
				items = resp.Usage
			} else if *cached {
				return err
			}
		} else if *cached {
			return apiErr
		}
	}

	if *harnessFilter != "" {
		var filtered []agents.HarnessUsage
		for _, u := range items {
			if u.Harness == *harnessFilter {
				filtered = append(filtered, u)
			}
		}
		items = filtered
	}

	if *jsonOut {
		return writeJSONOut(cio.Stdout, map[string]any{"usage": items})
	}
	printUsageTable(cio, items)
	return nil
}

func cmdStatusLine(args []string, cio *IO) error {
	if len(args) == 0 || args[0] != "claude" {
		return fmt.Errorf("usage: fleet statusline claude [--wrap CMD]")
	}
	fs := flag.NewFlagSet("statusline claude", flag.ContinueOnError)
	wrap := fs.String("wrap", "", "existing statusLine command to tee stdin to")
	pos, err := parseFlagsInterspersed(fs, args[1:])
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(pos, " "))
	}
	if os.Getenv("FLEET_STATUSLINE_DISABLE") == "1" {
		return nil
	}
	return usage.HandleClaudeStatusLine(cio.Stdin, cio.Stdout, cio.Stderr, *wrap, "", "")
}
