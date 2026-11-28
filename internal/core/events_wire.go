package core

import (
	"cmp"
	"fmt"
	"strings"
	"time"

	"github.com/bdatdo0601/fleet/internal/agents"
	"github.com/bdatdo0601/fleet/internal/events"
	"github.com/bdatdo0601/fleet/internal/slacker"
)

func (f *Fleet) wireEvents() {
	if f.Events == nil {
		return
	}
	if f.Agents != nil {
		f.Agents.SetOnTransition(f.publishAgentTransition)
		f.Agents.SetOnWatchTriggered(f.publishAssistantWatch)
		f.Agents.SetOnAssistantMessage(f.publishAssistantMessage)
		f.Agents.SetOnUsage(f.publishUsageThreshold)
	}
	if f.Curator != nil {
		f.Curator.SetOnEvent(f.publishCuratorEvent)
	}
}

func (f *Fleet) publishAgentTransition(ev agents.StateTransition) {
	if f.Events == nil || !ev.StateChanged || ev.NewState == "working" {
		return
	}
	sev := "info"
	if agents.IsWaitingState(ev.NewState) {
		sev = "warn"
	}
	label := cmp.Or(ev.Name, ev.SessionID)
	summary := fmt.Sprintf("%s (%s on %s) -> %s", label, ev.Harness, ev.Device, ev.NewState)
	if ev.Summary != "" {
		summary += " — " + ev.Summary
	}
	_, _, _ = f.Events.Publish(events.Event{
		Type:      "agent.transition",
		Severity:  sev,
		Source:    "agents",
		Actor:     ev.SessionID,
		Subject:   ev.SessionID,
		DedupeKey: fmt.Sprintf("tr:%s:%s:%s:%d", ev.SessionID, ev.NewState, ev.ChangedAt, time.Now().UTC().UnixNano()),
		Title:     fmt.Sprintf("Agent %s: %s", label, ev.NewState),
		Summary:   summary,
		Payload: map[string]any{
			"session_id":     ev.SessionID,
			"device":         ev.Device,
			"harness":        ev.Harness,
			"name":           ev.Name,
			"title":          ev.Title,
			"old_state":      ev.OldState,
			"new_state":      ev.NewState,
			"pending_action": ev.PendingAction,
			"summary":        ev.Summary,
			"git_status":     ev.GitStatus,
			"last_assistant": ev.LastAssistant,
			"changed_at":     ev.ChangedAt,
		},
	})
}

func (f *Fleet) publishAssistantWatch(w agents.Watch, ev agents.StateTransition) {
	if f.Events == nil {
		return
	}
	channelID, ok := agents.ParseAssistantTarget(w.WatcherSession)
	if !ok {
		channelID = "fleet-ops"
	}
	sev := "info"
	if agents.IsWaitingState(ev.NewState) {
		sev = "warn"
	}
	label := cmp.Or(ev.Name, ev.SessionID)
	_, _, _ = f.Events.Publish(events.Event{
		Type:        "agent.watch",
		Severity:    sev,
		Source:      "agents",
		Actor:       ev.SessionID,
		Subject:     ev.SessionID,
		ChannelHint: channelID,
		DedupeKey:   fmt.Sprintf("watch:%s:%s:%d", w.ID, ev.NewState, time.Now().UTC().UnixNano()),
		Title:       fmt.Sprintf("Watched agent %s: %s", label, ev.NewState),
		Summary:     fmt.Sprintf("Watched %s session %s (%s on %s) transitioned to %s.", ev.Harness, label, ev.SessionID, ev.Device, ev.NewState),
		Payload: map[string]any{
			"watch_id":       w.ID,
			"session_id":     ev.SessionID,
			"device":         ev.Device,
			"harness":        ev.Harness,
			"name":           ev.Name,
			"old_state":      ev.OldState,
			"new_state":      ev.NewState,
			"pending_action": ev.PendingAction,
			"summary":        ev.Summary,
			"git_status":     ev.GitStatus,
			"last_assistant": ev.LastAssistant,
		},
	})
}

func (f *Fleet) publishAssistantMessage(msg agents.Message) {
	if f.Events == nil || msg.ToSession == nil {
		return
	}
	channelID, ok := agents.ParseAssistantTarget(*msg.ToSession)
	if !ok {
		channelID = "general"
	}
	sender := cmp.Or(msg.FromName, msg.FromSession)
	_, _, _ = f.Events.Publish(events.Event{
		Type:        "agent.message",
		Severity:    "info",
		Source:      "agents",
		Actor:       msg.FromSession,
		Subject:     msg.ThreadID,
		ChannelHint: channelID,
		DedupeKey:   fmt.Sprintf("amsg:%d", msg.ID),
		Title:       fmt.Sprintf("Message from %s (%s)", sender, msg.FromDevice),
		Summary:     msg.Body,
		Payload: map[string]any{
			"message_id":   msg.ID,
			"thread_id":    msg.ThreadID,
			"reply_to":     msg.ReplyTo,
			"from_session": msg.FromSession,
			"from_device":  msg.FromDevice,
			"from_name":    msg.FromName,
			"from_title":   msg.FromTitle,
			"to_session":   *msg.ToSession,
			"body":         msg.Body,
		},
	})
}

func maxWindowBucket(windows []agents.UsageWindow, label string) int {
	for _, w := range windows {
		if w.Label != label {
			continue
		}
		if w.UsedPercent >= 95 {
			return 95
		}
		if w.UsedPercent >= 85 {
			return 85
		}
	}
	return 0
}

func (f *Fleet) publishUsageThreshold(prev *agents.HarnessUsage, cur agents.HarnessUsage) {
	if f.Events == nil || cur.Status != "ok" {
		return
	}
	for _, w := range cur.Windows {
		bucket := 0
		sev := "warn"
		if w.UsedPercent >= 95 {
			bucket = 95
			sev = "critical"
		} else if w.UsedPercent >= 85 {
			bucket = 85
		}
		if bucket == 0 {
			continue
		}
		if prev != nil && maxWindowBucket(prev.Windows, w.Label) >= bucket {
			continue
		}
		_, _, _ = f.Events.Publish(events.Event{
			Type:      "usage.threshold",
			Severity:  sev,
			Source:    "usage",
			Actor:     cur.Device,
			Subject:   cur.Harness + ":" + w.Label,
			DedupeKey: fmt.Sprintf("usage:%s:%s:%d:%s", cur.Harness, w.Label, bucket, w.ResetsAt),
			Title:     fmt.Sprintf("%s %s quota at %.0f%%", cur.Harness, w.Label, w.UsedPercent),
			Summary:   fmt.Sprintf("%s %s usage reached %.0f%% (resets %s)", cur.Harness, w.Label, w.UsedPercent, cmp.Or(w.ResetsAt, "soon")),
			Payload: map[string]any{
				"harness":      cur.Harness,
				"device":       cur.Device,
				"label":        w.Label,
				"used_percent": w.UsedPercent,
				"resets_at":    w.ResetsAt,
				"threshold":    bucket,
			},
		})
	}
}

func (f *Fleet) publishCuratorEvent(kind, scope, key, detail string) {
	if f.Events == nil {
		return
	}
	evType := "memory." + kind
	sev := "info"
	if kind == "curator_failed" {
		sev = "error"
	}
	_, _, _ = f.Events.Publish(events.Event{
		Type:      evType,
		Severity:  sev,
		Source:    "memory",
		Subject:   scope + "/" + key,
		DedupeKey: fmt.Sprintf("mem:%s:%s:%s:%d", kind, scope, key, time.Now().UTC().Unix()/60),
		Title:     fmt.Sprintf("Memory %s: %s/%s", strings.ReplaceAll(kind, "_", " "), scope, key),
		Summary:   detail,
		Payload: map[string]any{
			"kind":   kind,
			"scope":  scope,
			"key":    key,
			"detail": detail,
		},
	})
}

// PublishSlackerReceiptEvent records a Slacker job receipt onto the authoritative event bus.
func (f *Fleet) PublishSlackerReceiptEvent(job slacker.Job, rec slacker.Receipt) {
	if f == nil || f.Events == nil {
		return
	}
	sev := "info"
	if rec.Status == "failed" {
		sev = "error"
	}
	summary := fmt.Sprintf("Slacker job %s (%s on %s) %s", job.ID, job.SessionName, job.TargetDevice, rec.Status)
	if rec.Error != "" {
		summary += ": " + rec.Error
	}
	_, _, _ = f.Events.Publish(events.Event{
		Type:      "slacker.job",
		Severity:  sev,
		Source:    "slacker",
		Actor:     job.TargetDevice,
		Subject:   job.ID,
		DedupeKey: fmt.Sprintf("slacker:%s:%s", job.ID, rec.Status),
		Title:     fmt.Sprintf("Slacker %s: %s (%s)", rec.Status, job.SessionName, job.TargetDevice),
		Summary:   summary,
		Payload: map[string]any{
			"job_id":       job.ID,
			"device":       job.TargetDevice,
			"harness":      job.Harness,
			"session_name": job.SessionName,
			"status":       rec.Status,
			"error":        rec.Error,
			"channel_name": job.ChannelName,
			"permalink":    job.Permalink,
		},
	})
}
