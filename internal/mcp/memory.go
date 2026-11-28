package mcp

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/bdatdo0601/fleet/internal/memory"
)

func resolveRepoSlug(ctx context.Context, api, cwd, sessionID string) string {
	if sid := strings.TrimSpace(sessionID); sid != "" {
		if sess := fetchAgentSession(ctx, api, sid); sess != nil {
			if sess.CWD != "" {
				if fi, err := os.Stat(sess.CWD); err == nil && fi.IsDir() {
					if repo := memory.CanonicalRepoContext(ctx, sess.CWD); repo != "" {
						return repo
					}
				}
			}
			if repo := memory.NormalizeSlug(sess.Repo, 80); repo != "" {
				return repo
			}
			if sess.CWD != "" {
				if repo := memory.NormalizeSlug(filepath.Base(sess.CWD), 80); repo != "" {
					return repo
				}
			}
		}
	}
	return memory.CanonicalRepoContext(ctx, cwd)
}

func defaultRepoScope(ctx context.Context, api, cwd, sessionID string) string {
	if repo := resolveRepoSlug(ctx, api, cwd, sessionID); repo != "" {
		return "repo:" + repo
	}
	return "global"
}

func splitScopeKey(scope, key string, fallbackFn func() string) (string, string) {
	scope = strings.TrimSpace(scope)
	key = strings.TrimSpace(key)
	if scope == "" {
		if idx := strings.IndexByte(key, '/'); idx > 0 {
			scope = key[:idx]
			key = key[idx+1:]
		} else if fallbackFn != nil {
			scope = fallbackFn()
		}
	}
	return scope, key
}

func callMemorySearch(ctx context.Context, api, cwd string, rawArgs json.RawMessage) (any, error) {
	a, err := decodeArgs[struct {
		Query     string `json:"query"`
		Scope     string `json:"scope"`
		Kind      string `json:"kind"`
		Tier      string `json:"tier"`
		Limit     *int   `json:"limit"`
		SessionID string `json:"session_id"`
	}](rawArgs)
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	if strings.TrimSpace(a.Query) != "" {
		q.Set("q", strings.TrimSpace(a.Query))
	}
	scopeArg := strings.TrimSpace(a.Scope)
	switch scopeArg {
	case "", "current":
		if repo := resolveRepoSlug(ctx, api, cwd, a.SessionID); repo != "" {
			q.Set("scopes", "global,repo:"+repo)
		} else {
			q.Set("scope", "global")
		}
	case "all":
	default:
		q.Set("scope", scopeArg)
	}
	if a.Kind != "" {
		q.Set("kind", a.Kind)
	}
	if a.Tier != "" {
		q.Set("tier", a.Tier)
	}
	if a.Limit != nil && *a.Limit > 0 {
		q.Set("limit", strconv.Itoa(*a.Limit))
	}
	qs := ""
	if enc := q.Encode(); enc != "" {
		qs = "?" + enc
	}
	return callJSON(ctx, api, http.MethodGet, "/v1/memory"+qs, nil, 15*time.Second)
}

func callMemoryGet(ctx context.Context, api string, rawArgs json.RawMessage) (any, error) {
	a, err := decodeArgs[struct {
		Scope          string `json:"scope"`
		Key            string `json:"key"`
		IncludeHistory bool   `json:"include_history"`
	}](rawArgs)
	if err != nil {
		return nil, err
	}
	scope, key := splitScopeKey(a.Scope, a.Key, func() string { return "global" })
	q := url.Values{}
	q.Set("scope", scope)
	q.Set("key", key)
	if a.IncludeHistory {
		q.Set("history", "true")
	}
	return callJSON(ctx, api, http.MethodGet, "/v1/memory/item?"+q.Encode(), nil, 15*time.Second)
}

func callMemorySet(ctx context.Context, api, harness, cwd string, rawArgs json.RawMessage) (any, error) {
	a, err := decodeArgs[struct {
		Scope     string `json:"scope"`
		Key       string `json:"key"`
		Kind      string `json:"kind"`
		Tier      string `json:"tier"`
		Summary   string `json:"summary"`
		Body      string `json:"body"`
		SessionID string `json:"session_id"`
	}](rawArgs)
	if err != nil {
		return nil, err
	}
	scope, key := splitScopeKey(a.Scope, a.Key, func() string {
		return defaultRepoScope(ctx, api, cwd, a.SessionID)
	})
	body := map[string]any{
		"scope":   scope,
		"key":     key,
		"kind":    cmp.Or(a.Kind, "project"),
		"tier":    a.Tier,
		"summary": a.Summary,
		"body":    a.Body,
		"source":  "fleet",
		"harness": harness,
	}
	if a.SessionID != "" {
		body["session_id"] = a.SessionID
	}
	return callJSON(ctx, api, http.MethodPost, "/v1/memory", body, 15*time.Second)
}

func callMemoryDelete(ctx context.Context, api, harness, cwd string, rawArgs json.RawMessage) (any, error) {
	a, err := decodeArgs[struct {
		Scope     string `json:"scope"`
		Key       string `json:"key"`
		SessionID string `json:"session_id"`
	}](rawArgs)
	if err != nil {
		return nil, err
	}
	scope, key := splitScopeKey(a.Scope, a.Key, func() string {
		return defaultRepoScope(ctx, api, cwd, a.SessionID)
	})
	q := url.Values{}
	q.Set("scope", scope)
	q.Set("key", key)
	q.Set("harness", harness)
	if a.SessionID != "" {
		q.Set("session_id", a.SessionID)
	}
	return callJSON(ctx, api, http.MethodDelete, "/v1/memory/item?"+q.Encode(), nil, 15*time.Second)
}

func callMemoryTool(ctx context.Context, name string, rawArgs json.RawMessage, api, harness, cwd string) (any, error) {
	switch name {
	case "fleet_memory_search":
		return callMemorySearch(ctx, api, cwd, rawArgs)
	case "fleet_memory_get":
		return callMemoryGet(ctx, api, rawArgs)
	case "fleet_memory_set":
		return callMemorySet(ctx, api, harness, cwd, rawArgs)
	case "fleet_memory_delete":
		return callMemoryDelete(ctx, api, harness, cwd, rawArgs)
	default:
		return nil, fmt.Errorf("unknown tool %q", name)
	}
}
