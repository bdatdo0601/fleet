package assistant

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/bdatdo0601/fleet/internal/events"
)

const (
	routeEventsOnly      = "events_only"
	routeOpsNotify       = "ops_notify"
	routeTriage          = "triage"
	routeWakeOps         = "wake_ops"
	routeEscalateGeneral = "escalate_general"
)

func (e *Engine) runEventRouter(ctx context.Context) {
	defer e.wg.Done()
	if e.deps == nil {
		return
	}
	ch := e.deps.SubscribeEvents()
	if ch == nil {
		return
	}
	defer e.deps.UnsubscribeEvents(ch)

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			e.HandleBusEvent(ctx, ev)
		}
	}
}

// HandleBusEvent logs an authoritative bus event to #events (zero-token) and runs the 3-stage alert router.
func (e *Engine) HandleBusEvent(ctx context.Context, ev events.Event) {
	if e == nil || e.store == nil {
		return
	}
	e.appendSystemChannelNotice("events", "event:"+ev.Type, formatEventLogLine(ev))

	cfg := e.Config()
	if cfg.Events.Enabled != nil && !*cfg.Events.Enabled {
		return
	}

	action, targetChannel := decideStage0Route(cfg.Events, ev)
	if action == routeTriage {
		action, targetChannel = e.triageEventWithSystem1(ctx, ev, targetChannel)
	}
	e.executeRoutedEvent(ctx, cfg, ev, action, targetChannel)
}

func formatEventLogLine(ev events.Event) string {
	sev := strings.ToUpper(cmp.Or(ev.Severity, "info"))
	line := fmt.Sprintf("[%s] `%s` — %s", sev, ev.Type, cmp.Or(ev.Title, ev.Type))
	if sum := strings.TrimSpace(ev.Summary); sum != "" && sum != ev.Title {
		line += "\n" + truncateText(sum, 400)
	}
	return line
}

func (e *Engine) appendSystemChannelNotice(channelID, source, content string) {
	if e == nil || e.store == nil {
		return
	}
	ch, err := e.store.GetChannel(channelID)
	if err != nil || ch == nil {
		return
	}
	msg, err := e.store.AppendMessage(Message{
		ChannelID:    ch.ID,
		ContextEpoch: ch.ContextEpoch,
		Role:         "assistant",
		Content:      content,
		Source:       source,
		Observed:     true,
	})
	if err == nil {
		e.publishEvent(StreamEvent{Type: "message", ChannelID: ch.ID, Data: msg})
	}
}

func defaultEventTargetChannel(ev events.Event) string {
	if ev.Type == "agent.message" {
		return cmp.Or(strings.TrimSpace(ev.ChannelHint), "general")
	}
	if ch := strings.TrimSpace(ev.ChannelHint); ch != "" {
		return ch
	}
	return "fleet-ops"
}

var staticStage0Routes = map[string]string{
	"agent.watch":           routeWakeOps,
	"agent.message":         routeWakeOps,
	"infra.nebula_restart":  routeEscalateGeneral,
	"infra.tls_error":       routeEscalateGeneral,
	"infra.cert_expiring":   routeEscalateGeneral,
	"memory.curator_failed": routeOpsNotify,
	"memory.proposal":       routeEventsOnly,
	"infra.tls_renewed":     routeEventsOnly,
	"mcp.oauth":             routeEventsOnly,
}

func decideStage0Route(cfg EventsConfig, ev events.Event) (action, targetChannel string) {
	targetChannel = defaultEventTargetChannel(ev)
	if override, ok := cfg.Routes[strings.ToLower(strings.TrimSpace(ev.Type))]; ok && override != "" {
		return override, targetChannel
	}
	if staticAction, ok := staticStage0Routes[ev.Type]; ok {
		if staticAction == routeEscalateGeneral {
			return staticAction, "general"
		}
		return staticAction, targetChannel
	}
	switch ev.Type {
	case "usage.threshold":
		if ev.Severity == "critical" {
			return routeEscalateGeneral, "general"
		}
		return routeOpsNotify, "fleet-ops"
	case "slacker.job":
		if status, _ := ev.Payload["status"].(string); ev.Severity == "error" || status == "failed" {
			return routeTriage, "fleet-ops"
		}
		return routeEventsOnly, "events"
	case "agent.transition":
		return routeBySeverity(ev.Severity, false)
	default:
		return routeBySeverity(ev.Severity, true)
	}
}

func routeBySeverity(severity string, triageWarn bool) (string, string) {
	switch severity {
	case "critical":
		return routeEscalateGeneral, "general"
	case "error":
		return routeTriage, "fleet-ops"
	case "warn":
		if triageWarn {
			return routeTriage, "fleet-ops"
		}
	}
	return routeEventsOnly, "events"
}

func (e *Engine) triageEventWithSystem1(ctx context.Context, ev events.Event, targetChannel string) (string, string) {
	rtCfg, err := e.resolveRuntimeConfig(ctx)
	if err != nil || (strings.TrimSpace(rtCfg.APIKey) == "" && !rtCfg.LocalLLM.Enabled) {
		return routeOpsNotify, "fleet-ops"
	}
	state := map[string]any{
		"type":     ev.Type,
		"severity": ev.Severity,
		"source":   ev.Source,
		"actor":    ev.Actor,
		"subject":  ev.Subject,
		"title":    ev.Title,
		"summary":  truncateText(ev.Summary, 400),
		"payload":  ev.Payload,
	}
	questions := []DecisionQuestion{{
		ID:   "action",
		Type: "choice",
		Instructions: "Decide how the always-on Fleet assistant should handle this event: " +
			"archive (keep only in #events), ops_turn (wake #fleet-ops agent to investigate or inspect), " +
			"or escalate_general (alert the user immediately in #general and Discord).",
		Criteria: map[string]string{
			"archive":          "Routine or low-value noise that requires no action beyond the event log",
			"ops_turn":         "Actionable engineering/agent/job failure or warning that #fleet-ops should investigate",
			"escalate_general": "Urgent human attention needed now (blocked critical work, security, infra outage, or repeat failures)",
		},
	}}
	tCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	ans, err := e.llm.DecideState(tCtx, rtCfg, state, questions)
	if err != nil {
		return routeOpsNotify, "fleet-ops"
	}
	switch ans["action"].Value {
	case "archive":
		return routeEventsOnly, "events"
	case "ops_turn":
		return routeWakeOps, cmp.Or(strings.TrimSpace(ev.ChannelHint), targetChannel, "fleet-ops")
	case "escalate_general":
		return routeEscalateGeneral, "general"
	default:
		return routeOpsNotify, "fleet-ops"
	}
}

func (e *Engine) executeRoutedEvent(ctx context.Context, cfg Config, ev events.Event, action, targetChannel string) {
	switch action {
	case routeEventsOnly:
		return
	case routeOpsNotify:
		e.appendSystemChannelNotice("fleet-ops", "event:"+ev.Type, formatEventLogLine(ev))
	case routeWakeOps:
		e.enqueueEventTurn(ev, targetChannel)
	case routeEscalateGeneral:
		if reason := e.escalationSuppressionReason(cfg.Events, ev, time.Now()); reason != "" {
			e.appendSystemChannelNotice("fleet-ops", "event:"+ev.Type, formatEventLogLine(ev)+"\n_(suppressed from #general: "+reason+")_")
			return
		}
		priority := "normal"
		if ev.Severity == "critical" {
			priority = "urgent"
		}
		cooldownKey := ev.Type + ":" + cmp.Or(ev.Subject, ev.Actor, ev.Title)
		_, _ = e.EscalateToGeneral(ctx, cmp.Or(ev.Title, ev.Type), cmp.Or(ev.Summary, ev.Title), priority, cooldownKey)
	}
}

func (e *Engine) enqueueEventTurn(ev events.Event, targetChannel string) {
	chID := cmp.Or(strings.TrimSpace(targetChannel), "fleet-ops")
	if _, err := e.store.GetChannel(chID); err != nil {
		chID = "fleet-ops"
	}
	inboxSource := "event"
	switch ev.Type {
	case "agent.watch":
		inboxSource = "agent_watch"
	case "agent.message":
		inboxSource = "agent_message"
	}
	prompt := buildEventInboxPrompt(ev, chID)
	dedupe := fmt.Sprintf("ev:%s:%s:%d", ev.Type, cmp.Or(ev.DedupeKey, fmt.Sprint(ev.ID)), time.Now().UTC().UnixNano())
	payload := map[string]any{
		"event_id":   ev.ID,
		"event_type": ev.Type,
		"subject":    ev.Subject,
		"prompt":     prompt,
	}
	if _, ins, err := e.store.EnqueueInbox(chID, inboxSource, dedupe, payload, ""); err == nil && ins {
		e.publishEvent(StreamEvent{Type: "event_routed", ChannelID: chID, Data: ev})
		e.notify()
	}
}

func buildEventInboxPrompt(ev events.Event, channelID string) string {
	switch ev.Type {
	case "agent.message":
		fromSess, _ := ev.Payload["from_session"].(string)
		fromDev, _ := ev.Payload["from_device"].(string)
		fromName, _ := ev.Payload["from_name"].(string)
		threadID, _ := ev.Payload["thread_id"].(string)
		body, _ := ev.Payload["body"].(string)
		var msgID float64
		if v, ok := ev.Payload["message_id"].(float64); ok {
			msgID = v
		} else if v, ok := ev.Payload["message_id"].(int64); ok {
			msgID = float64(v)
		}
		return fmt.Sprintf(
			"Incoming Fleet peer message for #%s from %s (%s, session %s, thread %s, message_id %.0f):\n%s\n\nRespond to the user concisely if needed, and if the peer expects a reply, send it with `fleet_send_message(to=%q, message=..., thread_id=%q, reply_to=%.0f)`.",
			channelID, cmp.Or(fromName, fromSess), fromDev, fromSess, threadID, msgID, cmp.Or(body, ev.Summary), fromSess, threadID, msgID,
		)
	case "agent.watch":
		sessID, _ := ev.Payload["session_id"].(string)
		device, _ := ev.Payload["device"].(string)
		harness, _ := ev.Payload["harness"].(string)
		name, _ := ev.Payload["name"].(string)
		newState, _ := ev.Payload["new_state"].(string)
		summary, _ := ev.Payload["summary"].(string)
		gitStatus, _ := ev.Payload["git_status"].(string)
		lastAssistant, _ := ev.Payload["last_assistant"].(string)
		var details []string
		if summary != "" {
			details = append(details, "Summary: "+truncateText(summary, 300))
		}
		if lastAssistant != "" {
			details = append(details, "Last response: "+truncateText(lastAssistant, 600))
		}
		if gitStatus != "" {
			details = append(details, "Git status: "+truncateText(gitStatus, 250))
		}
		if rawPA, ok := ev.Payload["pending_action"]; ok && rawPA != nil {
			if b, err := json.Marshal(rawPA); err == nil && string(b) != "{}" && string(b) != "null" {
				details = append(details, "Pending action: "+string(b))
			}
		}
		out := fmt.Sprintf("Watched %s session %s (%s on %s) transitioned to %s. Call `fleet_inspect_agent` if you need full output/blocker details, then give a concise update in #%s.",
			harness, cmp.Or(name, sessID), sessID, device, newState, channelID)
		if len(details) > 0 {
			out += "\n" + strings.Join(details, "\n")
		}
		return out
	default:
		out := fmt.Sprintf("Fleet event [%s] %s: %s", ev.Type, ev.Title, ev.Summary)
		if len(ev.Payload) > 0 {
			if b, err := json.Marshal(ev.Payload); err == nil {
				out += "\nPayload: " + truncateText(string(b), 600)
			}
		}
		return out + fmt.Sprintf("\nInvestigate briefly if needed and summarize findings concisely in #%s. Only call `escalate_to_general` if urgent human attention is required.", channelID)
	}
}

func inQuietHours(cfg EventsConfig, now time.Time) bool {
	startMin, ok1 := parseHHMM(cfg.QuietHoursStart)
	endMin, ok2 := parseHHMM(cfg.QuietHoursEnd)
	if !ok1 || !ok2 || startMin == endMin {
		return false
	}
	loc := time.Local
	if strings.EqualFold(cfg.Timezone, "UTC") {
		loc = time.UTC
	} else if cfg.Timezone != "" && !strings.EqualFold(cfg.Timezone, "Local") {
		if l, err := time.LoadLocation(cfg.Timezone); err == nil {
			loc = l
		}
	}
	localNow := now.In(loc)
	curMin := localNow.Hour()*60 + localNow.Minute()
	if startMin < endMin {
		return curMin >= startMin && curMin < endMin
	}
	return curMin >= startMin || curMin < endMin
}

func (e *Engine) escalationSuppressionReason(cfg EventsConfig, ev events.Event, now time.Time) string {
	if ev.Severity == "critical" {
		return ""
	}
	if inQuietHours(cfg, now) {
		return "quiet hours"
	}
	cooldown := time.Duration(cfg.EscalationCooldownMinutes) * time.Minute
	if cooldown <= 0 {
		return ""
	}
	key := ev.Type + ":" + cmp.Or(ev.Subject, ev.Actor, ev.Title)
	e.routerMu.Lock()
	last := e.lastEscalate[key]
	e.routerMu.Unlock()
	if !last.IsZero() && now.UTC().Sub(last) < cooldown {
		return fmt.Sprintf("cooldown (%s remaining)", (cooldown - now.UTC().Sub(last)).Round(time.Second))
	}
	return ""
}

// EscalateToGeneral posts a high-signal alert in #general, records outbox delivery, and dispatches external notifications.
func (e *Engine) EscalateToGeneral(ctx context.Context, title, body, priority, cooldownKey string) (map[string]any, error) {
	if e == nil || e.store == nil {
		return nil, newErr("assistant store unavailable", 503)
	}
	cfg := e.Config()
	cleanPriority := strings.ToLower(cmp.Or(strings.TrimSpace(priority), "normal"))
	if cleanPriority != "low" && cleanPriority != "urgent" {
		cleanPriority = "normal"
	}
	cleanKey := strings.TrimSpace(cooldownKey)
	if cleanKey != "" && cleanPriority != "urgent" {
		cooldown := time.Duration(cfg.Events.EscalationCooldownMinutes) * time.Minute
		e.routerMu.Lock()
		last := e.lastEscalate[cleanKey]
		if !last.IsZero() && time.Now().UTC().Sub(last) < cooldown {
			e.routerMu.Unlock()
			return map[string]any{"escalated": false, "suppressed": "cooldown", "cooldown_key": cleanKey}, nil
		}
		e.lastEscalate[cleanKey] = time.Now().UTC()
		e.routerMu.Unlock()
	} else if cleanKey != "" {
		e.routerMu.Lock()
		e.lastEscalate[cleanKey] = time.Now().UTC()
		e.routerMu.Unlock()
	}

	title = cmp.Or(strings.TrimSpace(title), "Fleet Alert")
	body = strings.TrimSpace(body)
	content := fmt.Sprintf("**%s**\n%s", title, body)
	ch, err := e.store.GetChannel("general")
	if err == nil && ch != nil {
		msg, aErr := e.store.AppendMessage(Message{
			ChannelID:    "general",
			ContextEpoch: ch.ContextEpoch,
			Role:         "assistant",
			Content:      content,
			Source:       "escalation",
		})
		if aErr == nil {
			e.publishEvent(StreamEvent{Type: "message", ChannelID: "general", Data: msg})
		}
	}
	item, oErr := e.store.AddOutbox("general", cleanPriority, title, body)
	if oErr != nil {
		return nil, oErr
	}
	e.publishEvent(StreamEvent{Type: "outbox", ChannelID: "general", Data: item})
	if (cfg.Discord.Enabled || cfg.Notifications.Enabled) && priorityMeetsMin(item.Priority, cfg.Notifications.MinPriority) {
		e.spawnBG(ctx, 10*time.Second, func(nCtx context.Context) {
			e.dispatchOutboxNotification(nCtx, item)
		})
	}
	return map[string]any{"escalated": true, "outbox_id": item.ID, "channel_id": "general", "priority": cleanPriority}, nil
}

type sweepAgentSnapshot struct {
	SessionID      string `json:"session_id"`
	Device         string `json:"device"`
	Harness        string `json:"harness"`
	Name           string `json:"name"`
	State          string `json:"state"`
	StateChangedAt string `json:"state_changed_at"`
	UpdatedAt      string `json:"updated_at"`
	Summary        string `json:"summary"`
}

func (e *Engine) runEventSweeps(ctx context.Context, now time.Time, tickCount int) {
	if e == nil || e.deps == nil {
		return
	}
	e.deps.SweepStaleAgents(now)

	cfg := e.Config()
	if cfg.Events.Enabled != nil && !*cfg.Events.Enabled {
		return
	}
	e.sweepWaitingAgents(cfg.Events, now)
	if tickCount%120 == 1 {
		e.sweepExpiringCerts(now)
	}
	_ = ctx
}

func (e *Engine) sweepWaitingAgents(cfg EventsConfig, now time.Time) {
	dwell := time.Duration(max(cfg.WaitingDwellMinutes, 1)) * time.Minute
	raw, err := e.deps.ListAgents("", "", "", "", false)
	if err != nil || raw == nil {
		return
	}
	b, mErr := json.Marshal(raw)
	if mErr != nil {
		return
	}
	var snaps []sweepAgentSnapshot
	if json.Unmarshal(b, &snaps) != nil {
		return
	}
	for _, s := range snaps {
		if s.State != "waiting_permission" && s.State != "waiting_question" {
			continue
		}
		refTimeStr := cmp.Or(s.StateChangedAt, s.UpdatedAt)
		refTime, pErr := time.Parse(time.RFC3339, refTimeStr)
		if pErr != nil || now.Sub(refTime) < dwell {
			continue
		}
		stamp := refTimeStr
		e.routerMu.Lock()
		prevStamp := e.waitingAlerted[s.SessionID]
		if prevStamp == stamp {
			e.routerMu.Unlock()
			continue
		}
		e.waitingAlerted[s.SessionID] = stamp
		e.routerMu.Unlock()

		label := cmp.Or(s.Name, s.SessionID)
		_, _, _ = e.deps.PublishEvent(events.Event{
			Type:      "agent.transition",
			Severity:  "error",
			Source:    "assistant_sweep",
			Actor:     s.SessionID,
			Subject:   s.SessionID,
			DedupeKey: fmt.Sprintf("dwell:%s:%s", s.SessionID, stamp),
			Title:     fmt.Sprintf("Agent waiting %dm+: %s", int(now.Sub(refTime).Minutes()), label),
			Summary:   fmt.Sprintf("%s (%s on %s) has been stuck in %s since %s. %s", label, s.Harness, s.Device, s.State, refTimeStr, s.Summary),
			Payload: map[string]any{
				"session_id": s.SessionID,
				"device":     s.Device,
				"harness":    s.Harness,
				"name":       s.Name,
				"state":      s.State,
				"since":      refTimeStr,
			},
		})
	}
}

func (e *Engine) sweepExpiringCerts(now time.Time) {
	health, err := e.deps.SystemHealth()
	if err != nil || health == nil {
		return
	}
	today := now.UTC().Format("2006-01-02")
	if rawList, ok := health["expiring_device_certs"].([]map[string]any); ok {
		for _, item := range rawList {
			name, _ := item["name"].(string)
			notAfter, _ := item["not_after"].(string)
			e.publishCertExpiryEvent("device:"+name, name, notAfter, today, now)
		}
	}
	if tlsMap, ok := health["tls"].(map[string]any); ok {
		domain, _ := tlsMap["domain"].(string)
		if certMap, ok := tlsMap["cert"].(map[string]any); ok {
			notAfter, _ := certMap["not_after"].(string)
			if expAt, pErr := time.Parse(time.RFC3339, notAfter); pErr == nil && expAt.Sub(now) <= 14*24*time.Hour {
				e.publishCertExpiryEvent("tls:"+domain, domain, notAfter, today, now)
			}
		}
	}
}

func (e *Engine) publishCertExpiryEvent(subject, label, notAfter, today string, now time.Time) {
	key := subject + ":" + today
	e.routerMu.Lock()
	if e.certAlerted[subject] == key {
		e.routerMu.Unlock()
		return
	}
	e.certAlerted[subject] = key
	e.routerMu.Unlock()

	sev := "warn"
	if exp, err := time.Parse(time.RFC3339, notAfter); err == nil && exp.Sub(now) <= 3*24*time.Hour {
		sev = "critical"
	}
	_, _, _ = e.deps.PublishEvent(events.Event{
		Type:      "infra.cert_expiring",
		Severity:  sev,
		Source:    "assistant_sweep",
		Subject:   subject,
		DedupeKey: "certexp:" + key,
		Title:     fmt.Sprintf("Certificate expiring soon: %s", label),
		Summary:   fmt.Sprintf("%s expires at %s.", label, notAfter),
		Payload:   map[string]any{"subject": subject, "not_after": notAfter},
	})
}
