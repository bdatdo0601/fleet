package assistant

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// NotificationField represents one compact metadata field on an outbound alert.
type NotificationField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline,omitempty"`
}

// Notification describes an outbound read-only alert emitted by the assistant.
type Notification struct {
	Category  string              `json:"category,omitempty"` // approval, delegation, briefing, alert
	ChannelID string              `json:"channel_id,omitempty"`
	Priority  string              `json:"priority,omitempty"` // low, normal, urgent
	Title     string              `json:"title"`
	Summary   string              `json:"summary"`
	DeepLink  string              `json:"deep_link,omitempty"`
	Fields    []NotificationField `json:"fields,omitempty"`
}

// Notifier dispatches outbound read-only alerts to an external messaging sink.
type Notifier interface {
	Send(ctx context.Context, n Notification) error
}

// DiscordWebhookNotifier posts rich embeds to a Discord Webhook resolved from 1Password.
type DiscordWebhookNotifier struct {
	cfg                  NotificationsConfig
	reader               OPReaderFunc
	httpClient           *http.Client
	allowInsecureTestURL bool
}

// NewDiscordWebhookNotifier creates a DiscordWebhookNotifier from NotificationsConfig.
func NewDiscordWebhookNotifier(cfg NotificationsConfig, reader OPReaderFunc, httpClient *http.Client) *DiscordWebhookNotifier {
	if reader == nil {
		reader = defaultOPReader
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	return &DiscordWebhookNotifier{
		cfg:        normalizeNotificationsConfig(cfg),
		reader:     reader,
		httpClient: httpClient,
	}
}

func priorityRank(p string) int {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case "low":
		return 0
	case "urgent":
		return 2
	default:
		return 1
	}
}

func priorityMeetsMin(priority, minPriority string) bool {
	return priorityRank(priority) >= priorityRank(minPriority)
}

func (d *DiscordWebhookNotifier) resolveOPRef(n Notification) string {
	if cat := strings.ToLower(strings.TrimSpace(n.Category)); cat != "" {
		if ref, ok := d.cfg.DiscordChannelOPRefs[cat]; ok && strings.TrimSpace(ref) != "" {
			return strings.TrimSpace(ref)
		}
		switch cat {
		case "approval":
			if ref, ok := d.cfg.DiscordChannelOPRefs["approvals"]; ok && strings.TrimSpace(ref) != "" {
				return strings.TrimSpace(ref)
			}
		case "approvals":
			if ref, ok := d.cfg.DiscordChannelOPRefs["approval"]; ok && strings.TrimSpace(ref) != "" {
				return strings.TrimSpace(ref)
			}
		}
	}
	if ch := strings.ToLower(strings.TrimSpace(n.ChannelID)); ch != "" {
		if ref, ok := d.cfg.DiscordChannelOPRefs[ch]; ok && strings.TrimSpace(ref) != "" {
			return strings.TrimSpace(ref)
		}
	}
	return strings.TrimSpace(d.cfg.DiscordWebhookOPRef)
}

func validateDiscordWebhookURL(raw string, allowInsecureTest bool) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Hostname() == "" {
		return errors.New("invalid Discord webhook URL")
	}
	if allowInsecureTest && (u.Scheme == "http" || u.Scheme == "https") {
		return nil
	}
	host := strings.ToLower(u.Hostname())
	if u.Scheme != "https" || (host != "discord.com" && host != "discordapp.com") || !strings.HasPrefix(u.Path, "/api/webhooks/") {
		return errors.New("discord webhook URL must start with https://discord.com/api/webhooks/")
	}
	return nil
}

func discordEmbedColor(n Notification) int {
	if strings.EqualFold(n.Priority, "urgent") || strings.EqualFold(n.Category, "approval") || strings.EqualFold(n.Category, "approvals") {
		return 0xef4444
	}
	if strings.EqualFold(n.Category, "delegation") {
		return 0xf59e0b
	}
	if strings.EqualFold(n.Priority, "low") {
		return 0x64748b
	}
	return 0x0284c7
}

func buildFleetDeepLink(baseURL, channelID string) string {
	base := strings.TrimRight(cmp.Or(strings.TrimSpace(baseURL), "http://10.213.0.1:7443"), "/")
	ch := cmp.Or(strings.TrimSpace(channelID), "general")
	return base + "/#chat/" + url.PathEscape(ch)
}

// Send resolves the Discord webhook from 1Password and posts a concise privacy-bounded embed.
func (d *DiscordWebhookNotifier) Send(ctx context.Context, n Notification) error {
	ref := d.resolveOPRef(n)
	if ref == "" {
		return newErr("discord_webhook_op_ref is not configured", http.StatusBadRequest)
	}
	webhookURL, err := d.reader(ctx, ref)
	if err != nil {
		return err
	}
	webhookURL = strings.TrimSpace(webhookURL)
	if err := validateDiscordWebhookURL(webhookURL, d.allowInsecureTestURL); err != nil {
		return errors.New(scrubSecret(err.Error(), webhookURL))
	}
	deepLink := cmp.Or(strings.TrimSpace(n.DeepLink), buildFleetDeepLink(d.cfg.FleetBaseURL, n.ChannelID))
	desc := truncateText(strings.TrimSpace(n.Summary), 350)
	if deepLink != "" {
		if desc != "" {
			desc += "\n\n"
		}
		desc += fmt.Sprintf("[Open in Fleet Console](%s)", deepLink)
	}
	var fields []map[string]any
	for _, f := range n.Fields {
		if strings.TrimSpace(f.Name) == "" || strings.TrimSpace(f.Value) == "" {
			continue
		}
		fields = append(fields, map[string]any{
			"name":   truncateText(strings.TrimSpace(f.Name), 64),
			"value":  truncateText(strings.TrimSpace(f.Value), 160),
			"inline": f.Inline,
		})
	}
	payload := map[string]any{
		"embeds": []map[string]any{{
			"title":       truncateText(cmp.Or(strings.TrimSpace(n.Title), "Fleet Assistant Alert"), 120),
			"description": desc,
			"url":         deepLink,
			"color":       discordEmbedColor(n),
			"fields":      fields,
			"footer":      map[string]string{"text": "Fleet Always-On Assistant · Read-only alerts"},
			"timestamp":   time.Now().UTC().Format(time.RFC3339),
		}},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, bytes.NewReader(raw))
	if err != nil {
		return errors.New(scrubSecret(err.Error(), webhookURL))
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.httpClient.Do(req)
	if err != nil {
		return errors.New(scrubSecret(err.Error(), webhookURL))
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return errors.New(scrubSecret(fmt.Sprintf("Discord webhook HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body))), webhookURL))
	}
	return nil
}
