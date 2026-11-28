package assistant

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

func (e *Engine) maybeAutoObserve(ctx context.Context, cfg Config, channelID string) {
	msgs, err := e.store.ListUnobservedMessages(channelID, 200)
	if err != nil || len(msgs) < 6 {
		return
	}
	var chars int
	for _, m := range msgs {
		chars += len(m.Content)
	}
	if chars/4 < cfg.ObserverThresholdTokens {
		return
	}
	_, _ = e.RunObserver(ctx, channelID)
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
	if len(msgs) == 0 {
		return map[string]any{"observed": 0, "reflected": false}, nil
	}
	var transcript strings.Builder
	var maxID int64
	for _, m := range msgs {
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
	v, err := e.vault()
	if err != nil {
		return nil, err
	}
	header := fmt.Sprintf("\n### %s\n%s\n", time.Now().UTC().Format("2006-01-02 15:04 UTC"), strings.TrimSpace(obsText))
	if _, err := v.AppendFile("_agent/memory/OBSERVATIONS.md", header); err != nil {
		return nil, err
	}
	if err := e.store.MarkMessagesObserved(channelID, maxID); err != nil {
		return nil, err
	}
	v.MaybeGitCommitContext(ctx, cfg.GitAutoCommit, "assistant: observational memory pass")

	reflected := false
	if curObs, rErr := v.ReadFile("_agent/memory/OBSERVATIONS.md"); rErr == nil && len(curObs) > cfg.ReflectorThresholdBytes {
		if _, refErr := e.runReflectorLocked(ctx); refErr == nil {
			reflected = true
		}
	}
	return map[string]any{"observed": len(msgs), "reflected": reflected}, nil
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
