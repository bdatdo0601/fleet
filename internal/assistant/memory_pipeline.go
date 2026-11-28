package assistant

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	observerCheckInterval = 5 * time.Minute
	observerErrorBackoff  = 15 * time.Minute
	observerBGTimeout     = 90 * time.Second
)

func (e *Engine) logObserver(format string, args ...any) {
	e.observerMu.Lock()
	w := e.observerLogW
	e.observerMu.Unlock()
	if w == nil {
		w = os.Stderr
	}
	_, _ = fmt.Fprintf(w, "%s assistant observer: "+format+"\n", append([]any{time.Now().UTC().Format(time.RFC3339)}, args...)...)
}

func canRunObserver(cfg Config) bool {
	return strings.TrimSpace(cfg.APIKey) != "" || strings.TrimSpace(cfg.APIKeyOPRef) != "" || cfg.LocalLLM.Enabled
}

func hasSubstantiveMessages(msgs []Message) bool {
	if len(msgs) < 2 {
		return false
	}
	var hasUser, hasAssistant bool
	var chars int
	for _, m := range msgs {
		trimmed := strings.TrimSpace(m.Content)
		if trimmed == "" {
			continue
		}
		chars += len(trimmed)
		switch m.Role {
		case "user":
			hasUser = true
		case "assistant":
			hasAssistant = true
		}
	}
	if hasUser && hasAssistant {
		return chars >= 20
	}
	return chars >= 80
}

func parseMessageTime(raw string) (time.Time, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if ts, err := time.Parse(layout, trimmed); err == nil {
			return ts, true
		}
	}
	return time.Time{}, false
}

func findIdleUnobservedChannels(msgs []Message, now time.Time, idleAfter time.Duration) []string {
	if idleAfter <= 0 || len(msgs) == 0 {
		return nil
	}
	byChannel := make(map[string][]Message)
	for _, m := range msgs {
		if m.ChannelID == "" {
			continue
		}
		byChannel[m.ChannelID] = append(byChannel[m.ChannelID], m)
	}
	chIDs := make([]string, 0, len(byChannel))
	for chID := range byChannel {
		chIDs = append(chIDs, chID)
	}
	slices.Sort(chIDs)

	var ready []string
	for _, chID := range chIDs {
		chMsgs := byChannel[chID]
		if !hasSubstantiveMessages(chMsgs) {
			continue
		}
		var newest time.Time
		for _, m := range chMsgs {
			if ts, ok := parseMessageTime(m.CreatedAt); ok && ts.After(newest) {
				newest = ts
			}
		}
		if !newest.IsZero() && now.Sub(newest) >= idleAfter {
			ready = append(ready, chID)
		}
	}
	return ready
}

func (e *Engine) maybeAutoObserve(ctx context.Context, cfg Config, channelID string) {
	msgs, err := e.store.ListUnobservedMessages(channelID, 200)
	if err != nil {
		e.logObserver("list unobserved #%s failed: %v", channelID, err)
		return
	}
	if len(msgs) < 6 {
		return
	}
	var chars int
	for _, m := range msgs {
		chars += len(m.Content)
	}
	if chars/4 < cfg.ObserverThresholdTokens {
		return
	}
	if _, err := e.RunObserver(ctx, channelID); err != nil {
		e.logObserver("auto-observe #%s failed: %v", channelID, err)
	}
}

func (e *Engine) flushObserverOnReset(ctx context.Context, channelID string, snapshot []Message, cleared bool) {
	if !canRunObserver(e.Config()) || !hasSubstantiveMessages(snapshot) {
		return
	}
	copied := append([]Message(nil), snapshot...)
	e.spawnBG(ctx, observerBGTimeout, func(obsCtx context.Context) {
		var err error
		if cleared {
			err = e.observeSnapshot(obsCtx, channelID, copied)
		} else {
			_, err = e.RunObserver(obsCtx, channelID)
		}
		if err != nil {
			e.logObserver("reset-flush #%s failed: %v", channelID, err)
		}
	})
}

func (e *Engine) maybeScheduleIdleObserve(ctx context.Context, cfg Config, now time.Time) {
	if cfg.ObserverIdleMinutes <= 0 || !canRunObserver(cfg) {
		return
	}
	e.observerMu.Lock()
	if e.observerRun || (!e.observerErrAt.IsZero() && now.Sub(e.observerErrAt) < observerErrorBackoff) {
		e.observerMu.Unlock()
		return
	}
	e.observerRun = true
	e.observerMu.Unlock()

	e.spawnBG(ctx, observerBGTimeout, func(obsCtx context.Context) {
		defer func() {
			e.observerMu.Lock()
			e.observerRun = false
			e.observerMu.Unlock()
		}()
		e.runIdleObservePass(obsCtx, cfg, now)
	})
}

func (e *Engine) runIdleObservePass(ctx context.Context, cfg Config, now time.Time) {
	if cfg.ObserverIdleMinutes <= 0 || !canRunObserver(cfg) {
		return
	}
	msgs, err := e.store.ListUnobservedMessages("", 200)
	if err != nil {
		e.logObserver("idle-observe list failed: %v", err)
		return
	}
	idleChannels := findIdleUnobservedChannels(msgs, now, time.Duration(cfg.ObserverIdleMinutes)*time.Minute)
	if len(idleChannels) == 0 {
		return
	}
	for _, chID := range idleChannels {
		if _, runErr := e.RunObserver(ctx, chID); runErr != nil {
			e.observerMu.Lock()
			e.observerErrAt = now
			e.observerMu.Unlock()
			e.logObserver("idle-observe #%s failed: %v", chID, runErr)
			continue
		}
		e.observerMu.Lock()
		e.observerErrAt = time.Time{}
		e.observerMu.Unlock()
	}
}

func (e *Engine) observeSnapshot(ctx context.Context, channelID string, msgs []Message) error {
	e.memoryMu.Lock()
	defer e.memoryMu.Unlock()

	cfg, err := e.resolveRuntimeConfig(ctx)
	if err != nil {
		return err
	}
	_, err = e.observeMessagesLocked(ctx, cfg, channelID, msgs, false)
	return err
}

// RunObserver distills unobserved messages into _agent/memory/OBSERVATIONS.md and triggers Reflector when large.
func (e *Engine) RunObserver(ctx context.Context, channelID string) (map[string]any, error) {
	e.memoryMu.Lock()
	defer e.memoryMu.Unlock()

	cfg, err := e.resolveRuntimeConfig(ctx)
	if err != nil {
		return nil, err
	}
	msgs, err := e.store.ListUnobservedMessages(channelID, 200)
	if err != nil {
		return nil, err
	}
	return e.observeMessagesLocked(ctx, cfg, channelID, msgs, true)
}

func (e *Engine) observeMessagesLocked(ctx context.Context, cfg Config, channelID string, msgs []Message, markObserved bool) (map[string]any, error) {
	filtered := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		if m.ID > 0 && e.observedMaxID != nil {
			if prev := e.observedMaxID[m.ChannelID]; prev > 0 && m.ID <= prev {
				continue
			}
		}
		filtered = append(filtered, m)
	}
	if len(filtered) == 0 {
		return map[string]any{"observed": 0, "reflected": false}, nil
	}
	var transcript strings.Builder
	var maxID int64
	for _, m := range filtered {
		if m.ID > maxID {
			maxID = m.ID
		}
		fmt.Fprintf(&transcript, "[%s #%s %s] %s\n", m.CreatedAt, m.ChannelID, m.Role, m.Content)
	}
	sys := `You are the Observational Memory Observer.
Convert the raw transcript into concise, dated Markdown bullet observations using priority markers:
- 🔴 critical decisions, blockers, explicit user instructions
- 🟡 ongoing tasks, preferences, context
- 🟢 minor FYI facts
Omit small talk. Output only Markdown bullets.`
	obsText, err := e.llm.CompleteText(ctx, cfg, cfg.Models.Observer, sys, transcript.String(), 1024)
	if err != nil {
		return nil, err
	}
	trimmedObs := strings.TrimSpace(obsText)
	if trimmedObs == "" {
		return nil, errors.New("observer returned empty content")
	}
	v, err := e.vault()
	if err != nil {
		return nil, err
	}
	header := fmt.Sprintf("\n### %s\n%s\n", time.Now().UTC().Format("2006-01-02 15:04 UTC"), trimmedObs)
	if _, err := v.AppendFile("_agent/memory/OBSERVATIONS.md", header); err != nil {
		return nil, err
	}
	if markObserved && maxID > 0 {
		if err := e.store.MarkMessagesObserved(channelID, maxID); err != nil {
			return nil, err
		}
	}
	if e.observedMaxID == nil {
		e.observedMaxID = make(map[string]int64)
	}
	for _, m := range filtered {
		e.observedMaxID[m.ChannelID] = max(e.observedMaxID[m.ChannelID], m.ID)
	}
	v.MaybeGitCommitContext(ctx, cfg.GitAutoCommit, "assistant: observational memory pass")

	reflected := false
	if curObs, rErr := v.ReadFile("_agent/memory/OBSERVATIONS.md"); rErr == nil && len(curObs) > cfg.ReflectorThresholdBytes {
		if _, refErr := e.runReflectorLocked(ctx); refErr != nil {
			e.logObserver("auto-reflector failed: %v", refErr)
		} else {
			reflected = true
		}
	}
	e.logObserver("distilled %d message(s) for #%s (reflected=%v)", len(filtered), cmp.Or(channelID, "all"), reflected)
	return map[string]any{"observed": len(filtered), "reflected": reflected}, nil
}

// RunReflector condenses _agent/memory/OBSERVATIONS.md into _agent/memory/REFLECTIONS.md and archives old observations.
func (e *Engine) RunReflector(ctx context.Context) (string, error) {
	e.memoryMu.Lock()
	defer e.memoryMu.Unlock()
	return e.runReflectorLocked(ctx)
}

func (e *Engine) runReflectorLocked(ctx context.Context) (string, error) {
	cfg, err := e.resolveRuntimeConfig(ctx)
	if err != nil {
		return "", err
	}
	v, err := e.vault()
	if err != nil {
		return "", err
	}
	reflections, _ := v.ReadFile("_agent/memory/REFLECTIONS.md")
	observations, err := v.ReadFile("_agent/memory/OBSERVATIONS.md")
	if err != nil {
		return "", err
	}
	sys := `You are the Observational Memory Reflector.
Merge the existing REFLECTIONS.md with new OBSERVATIONS.md into an updated, compact REFLECTIONS.md.
Preserve durable preferences, active projects, open follow-ups, and key dates; drop resolved or obsolete items.
Output only the updated Markdown for REFLECTIONS.md.`
	prompt := "## Current REFLECTIONS.md\n" + reflections + "\n\n## New OBSERVATIONS.md\n" + observations
	nextRefl, err := e.llm.CompleteText(ctx, cfg, cfg.Models.Reflector, sys, prompt, 1500)
	if err != nil {
		return "", err
	}
	archiveRel := filepath.ToSlash(filepath.Join("_agent/memory/archive", time.Now().UTC().Format("2006-01")+".md"))
	_, _ = v.AppendFile(archiveRel, observations)
	if _, err := v.WriteFile("_agent/memory/REFLECTIONS.md", strings.TrimSpace(nextRefl)+"\n"); err != nil {
		return "", err
	}
	if _, err := v.WriteFile("_agent/memory/OBSERVATIONS.md", "# Observations\n_Append-only dated observation log maintained by the background Observer._\n"); err != nil {
		return "", err
	}
	v.MaybeGitCommitContext(ctx, cfg.GitAutoCommit, "assistant: reflector compaction")
	return nextRefl, nil
}
