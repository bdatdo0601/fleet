package agents

import (
	"cmp"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/bdatdo0601/fleet/internal/paths"
	"github.com/bdatdo0601/fleet/internal/registry"
)

// UsageWindow describes one rate-limit or quota window for a harness.
type UsageWindow struct {
	Label       string  `json:"label"`
	UsedPercent float64 `json:"used_percent"`
	WindowMins  int     `json:"window_mins,omitempty"`
	ResetsAt    string  `json:"resets_at,omitempty"`
}

// HarnessUsage stores the latest quota/usage snapshot for a harness.
type HarnessUsage struct {
	Harness   string         `json:"harness"`
	Device    string         `json:"device"`
	Source    string         `json:"source"`
	Plan      string         `json:"plan,omitempty"`
	Status    string         `json:"status"`
	Detail    string         `json:"detail,omitempty"`
	Windows   []UsageWindow  `json:"windows"`
	Metrics   map[string]any `json:"metrics,omitempty"`
	UpdatedAt string         `json:"updated_at"`
}

func canonicalHarnessOrder(h string) int {
	switch h {
	case "claude":
		return 0
	case "codex":
		return 1
	case "antigravity":
		return 2
	default:
		return 3
	}
}

func normalizeUsage(u HarnessUsage) (HarnessUsage, error) {
	u.Harness = strings.ToLower(strings.TrimSpace(u.Harness))
	if !paths.ValidHarness(u.Harness) {
		return HarnessUsage{}, newAgentErr(fmt.Sprintf("unknown harness %q", u.Harness), 400)
	}
	u.Device = strings.TrimSpace(u.Device)
	u.Source = strings.TrimSpace(u.Source)
	u.Plan = strings.TrimSpace(u.Plan)
	u.Status = cmp.Or(strings.ToLower(strings.TrimSpace(u.Status)), "ok")
	u.Detail = strings.TrimSpace(u.Detail)
	if u.Windows == nil {
		u.Windows = []UsageWindow{}
	}
	if u.UpdatedAt == "" {
		u.UpdatedAt = registry.NowISO()
	}
	return u, nil
}

func scanUsageRow(scanner interface{ Scan(...any) error }) (HarnessUsage, error) {
	var u HarnessUsage
	var winRaw, metRaw string
	if err := scanner.Scan(&u.Harness, &u.Device, &u.Source, &u.Plan, &u.Status, &u.Detail, &winRaw, &metRaw, &u.UpdatedAt); err != nil {
		return HarnessUsage{}, err
	}
	u.Windows = []UsageWindow{}
	if strings.TrimSpace(winRaw) != "" {
		_ = json.Unmarshal([]byte(winRaw), &u.Windows)
		if u.Windows == nil {
			u.Windows = []UsageWindow{}
		}
	}
	if strings.TrimSpace(metRaw) != "" && strings.TrimSpace(metRaw) != "{}" {
		var m map[string]any
		if json.Unmarshal([]byte(metRaw), &m) == nil && len(m) > 0 {
			u.Metrics = m
		}
	}
	return u, nil
}

func parseUsageTime(ts string) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, ts); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// UpsertUsage saves a harness usage snapshot while guarding against degraded/empty reports overwriting fresh quota windows.
func (h *Hub) UpsertUsage(u HarnessUsage) (HarnessUsage, error) {
	norm, err := normalizeUsage(u)
	if err != nil {
		return HarnessUsage{}, err
	}

	h.mu.Lock()
	if err := h.checkOpenLocked(); err != nil {
		h.mu.Unlock()
		return HarnessUsage{}, err
	}

	row := h.db.QueryRow(`
		SELECT harness, device, source, plan, status, detail, windows_json, metrics_json, updated_at
		FROM harness_usage WHERE harness = ?
	`, norm.Harness)
	var prevPtr *HarnessUsage
	existing, err := scanUsageRow(row)
	if err == nil {
		prevCopy := existing
		prevPtr = &prevCopy
		if existing.Status == "ok" && norm.Status != "ok" {
			h.mu.Unlock()
			return existing, nil
		}
		if existing.Status == "ok" && norm.Status == "ok" && len(existing.Windows) > 0 && len(norm.Windows) == 0 {
			if ts, ok := parseUsageTime(existing.UpdatedAt); ok && time.Since(ts) < 24*time.Hour {
				h.mu.Unlock()
				return existing, nil
			}
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		h.mu.Unlock()
		return HarnessUsage{}, err
	}

	winBytes, err := json.Marshal(norm.Windows)
	if err != nil {
		h.mu.Unlock()
		return HarnessUsage{}, err
	}
	metBytes := []byte("{}")
	if len(norm.Metrics) > 0 {
		if b, mErr := json.Marshal(norm.Metrics); mErr == nil {
			metBytes = b
		}
	}

	_, err = h.db.Exec(`
		INSERT INTO harness_usage (harness, device, source, plan, status, detail, windows_json, metrics_json, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(harness) DO UPDATE SET
			device = excluded.device,
			source = excluded.source,
			plan = excluded.plan,
			status = excluded.status,
			detail = excluded.detail,
			windows_json = excluded.windows_json,
			metrics_json = excluded.metrics_json,
			updated_at = excluded.updated_at
	`, norm.Harness, norm.Device, norm.Source, norm.Plan, norm.Status, norm.Detail, string(winBytes), string(metBytes), norm.UpdatedAt)
	cb := h.onUsage
	h.mu.Unlock()
	if err != nil {
		return HarnessUsage{}, err
	}
	if cb != nil {
		cb(prevPtr, norm)
	}
	return norm, nil
}

// ListUsage returns all persisted harness usage snapshots in canonical harness order.
func (h *Hub) ListUsage() ([]HarnessUsage, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.checkOpenLocked(); err != nil {
		return nil, err
	}

	rows, err := h.db.Query(`
		SELECT harness, device, source, plan, status, detail, windows_json, metrics_json, updated_at
		FROM harness_usage
	`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := []HarnessUsage{}
	for rows.Next() {
		item, sErr := scanUsageRow(rows)
		if sErr != nil {
			return nil, sErr
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	slices.SortFunc(out, func(a, b HarnessUsage) int {
		oa, ob := canonicalHarnessOrder(a.Harness), canonicalHarnessOrder(b.Harness)
		if oa != ob {
			return cmp.Compare(oa, ob)
		}
		return strings.Compare(a.Harness, b.Harness)
	})
	return out, nil
}
