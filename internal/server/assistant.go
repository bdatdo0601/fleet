package server

import (
	"cmp"
	"encoding/json"
	"fmt"
	"html"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bdatdo0601/fleet/internal/assistant"
	"github.com/bdatdo0601/fleet/internal/core"
	"github.com/bdatdo0601/fleet/internal/registry"
)

const assistantWidgetHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>
:root { color-scheme: light dark; --bg: transparent; --text: #0f172a; --muted: #475569; --border: #cbd5e1; --accent: #0284c7; --surface: #f8fafc; }
:root[data-theme="dark"] { --text: #f1f5f9; --muted: #94a3b8; --border: #334155; --accent: #38bdf8; --surface: #0f172a; }
*, *::before, *::after { box-sizing: border-box; }
html, body { margin: 0; padding: 0; background: var(--bg); color: var(--text); font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif; font-size: 13px; line-height: 1.45; overflow-wrap: anywhere; word-break: break-word; }
#root { padding: 8px 10px; max-width: 100%; overflow-x: auto; }
button { font: inherit; cursor: pointer; border-radius: 6px; border: 1px solid var(--border); background: var(--surface); color: var(--text); padding: 4px 10px; max-width: 100%; }
button:hover { border-color: var(--accent); }
pre, code { white-space: pre-wrap; overflow-wrap: anywhere; word-break: break-word; max-width: 100%; }
img { max-width: 100%; height: auto; }
svg, canvas { max-width: 100%; display: block; }
svg[viewBox] { height: auto; }
table { width: 100%; border-collapse: collapse; }
th, td { text-align: left; padding: 4px 6px; border-bottom: 1px solid var(--border); }
.widget-err { color: #ef4444; font-size: 12px; padding: 6px 8px; border: 1px solid #ef4444; border-radius: 6px; }
</style>
</head>
<body>
<div id="root"></div>
<script>
(() => {
const root = document.getElementById("root");
let hasRendered = false;
function applyTheme(theme) {
  if (theme === "dark") document.documentElement.setAttribute("data-theme", "dark");
  else document.documentElement.removeAttribute("data-theme");
}
function sendHeight() {
  const h = Math.max(48, Math.ceil(Math.max(root.scrollHeight, root.getBoundingClientRect().height)));
  parent.postMessage({ type: "fleet-widget-resize", height: h }, "*");
}
function showWidgetError(msg) {
  const box = document.createElement("div");
  box.className = "widget-err";
  box.textContent = "Widget error: " + String(msg || "unknown error");
  root.appendChild(box);
  sendHeight();
}
window.addEventListener("error", (ev) => {
  showWidgetError(ev.message || "script error");
});
window.addEventListener("unhandledrejection", (ev) => {
  const r = ev.reason;
  showWidgetError(r && r.message ? r.message : r || "unhandled rejection");
});
window.addEventListener("message", (ev) => {
  if (!ev.data) return;
  if (ev.data.type === "theme") {
    applyTheme(ev.data.theme);
    return;
  }
  if (ev.data.type !== "render") return;
  applyTheme(ev.data.theme);
  if (hasRendered) {
    parent.postMessage({ type: "fleet-widget-rendered" }, "*");
    sendHeight();
    return;
  }
  hasRendered = true;
  root.innerHTML = String(ev.data.html || "");
  for (const sc of Array.from(root.querySelectorAll("script"))) {
    const srcAttr = (sc.getAttribute("src") || "").trim();
    if (srcAttr) {
      showWidgetError("External script blocked by offline widget CSP: " + srcAttr);
      sc.remove();
      continue;
    }
    const rawType = (sc.getAttribute("type") || "").trim();
    const type = rawType.toLowerCase();
    if (type && !["text/javascript", "application/javascript", "module", "text/ecmascript", "application/ecmascript"].includes(type)) {
      continue;
    }
    try {
      const s = document.createElement("script");
      if (rawType) s.type = rawType;
      s.textContent = sc.textContent;
      sc.replaceWith(s);
    } catch (err) {
      showWidgetError(err && err.message ? err.message : err);
    }
  }
  try {
    document.dispatchEvent(new Event("DOMContentLoaded"));
  } catch (err) {
    showWidgetError(err && err.message ? err.message : err);
  }
  try {
    window.dispatchEvent(new Event("load"));
  } catch (err) {
    showWidgetError(err && err.message ? err.message : err);
  }
  parent.postMessage({ type: "fleet-widget-rendered" }, "*");
  sendHeight();
  requestAnimationFrame(sendHeight);
  setTimeout(sendHeight, 60);
});
root.addEventListener("click", (ev) => {
  const btn = ev.target && ev.target.closest("[data-fleet-action]");
  if (!btn) return;
  parent.postMessage({ type: "fleet-widget-action", action: btn.getAttribute("data-fleet-action") || "" }, "*");
});
if (typeof ResizeObserver !== "undefined") {
  new ResizeObserver(sendHeight).observe(root);
}
parent.postMessage({ type: "fleet-widget-ready" }, "*");
})();
</script>
</body>
</html>`

func buildAssistantSummary(fleet *core.Fleet) map[string]any {
	if fleet.Assistant == nil {
		return map[string]any{"enabled": false, "configured": false}
	}
	view := fleet.Assistant.ConfigView()
	sc := fleet.Assistant.Store().Summary()
	skillsCount := 0
	if v, err := fleet.Assistant.Vault(); err == nil {
		if list, lErr := v.ListSkills(); lErr == nil {
			skillsCount = len(list)
		}
	}
	return map[string]any{
		"enabled":           view.Enabled,
		"configured":        view.Configured,
		"default_model":     view.Models.DefaultChat,
		"routing_mode":      view.Routing.Mode,
		"zdr":               view.ZDR,
		"channels":          sc.Channels,
		"pending_approvals": sc.PendingApprovals,
		"enabled_timers":    sc.EnabledTimers,
		"skills":            skillsCount,
		"sandbox_enabled":   view.Sandbox.Enabled,
	}
}

func registerAssistantRoutes(
	mux *http.ServeMux,
	fleet *core.Fleet,
	route func(bool, func(http.ResponseWriter, *http.Request, *registry.Device) error) http.HandlerFunc,
) {
	if fleet.Assistant == nil {
		return
	}
	registerAssistantAdminRoutes(mux, fleet, route)
	registerAssistantChatRoutes(mux, fleet, route)
	registerAssistantSkillRoutes(mux, fleet, route)
	registerAssistantVaultRoutes(mux, fleet, route)
	registerAssistantSandboxRoutes(mux, fleet, route)
	registerAssistantMCPRoutes(mux, fleet, route)
}

func registerAssistantAdminRoutes(
	mux *http.ServeMux,
	fleet *core.Fleet,
	route func(bool, func(http.ResponseWriter, *http.Request, *registry.Device) error) http.HandlerFunc,
) {
	mux.HandleFunc("GET /v1/assistant/status", route(true, func(w http.ResponseWriter, _ *http.Request, _ *registry.Device) error {
		writeJSON(w, http.StatusOK, fleet.Assistant.Status())
		return nil
	}))
	mux.HandleFunc("GET /v1/assistant/config", route(true, func(w http.ResponseWriter, _ *http.Request, _ *registry.Device) error {
		writeJSON(w, http.StatusOK, map[string]any{"config": fleet.Assistant.ConfigView()})
		return nil
	}))
	mux.HandleFunc("POST /v1/assistant/config", route(true, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		var req struct {
			assistant.Config
			ClearAPIKey bool  `json:"clear_api_key"`
			Test        *bool `json:"test"`
		}
		if err := readJSONBody(r, &req); err != nil {
			return err
		}
		view, err := fleet.Assistant.UpdateConfig(req.Config, req.ClearAPIKey)
		if err != nil {
			return err
		}
		if req.Test != nil && *req.Test {
			if tErr := fleet.Assistant.TestConnection(r.Context()); tErr != nil {
				return &core.FleetError{Msg: "saved config but connection test failed: " + tErr.Error(), Status: http.StatusBadRequest}
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"config": view})
		return nil
	}))
	mux.HandleFunc("POST /v1/assistant/test", route(true, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		if err := fleet.Assistant.TestConnection(r.Context()); err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return nil
	}))
	mux.HandleFunc("GET /v1/assistant/doctor", route(true, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		writeJSON(w, http.StatusOK, fleet.Assistant.Doctor(r.Context()))
		return nil
	}))
	mux.HandleFunc("POST /v1/assistant/discord/sync", route(true, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		res, err := fleet.Assistant.SyncDiscordChannels(r.Context())
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "sync": res})
		return nil
	}))
	mux.HandleFunc("POST /v1/assistant/op-token", route(true, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		var req struct {
			Token string `json:"token"`
			Clear bool   `json:"clear"`
		}
		if err := readJSONBody(r, &req); err != nil {
			return err
		}
		tok := strings.TrimSpace(req.Token)
		if req.Clear {
			tok = ""
		} else if tok == "" {
			return &core.FleetError{Msg: "token or clear=true is required", Status: http.StatusBadRequest}
		}
		if err := fleet.Assistant.SaveOPServiceAccountToken(tok); err != nil {
			return err
		}
		rep := fleet.Assistant.Doctor(r.Context())
		var opCheck assistant.DoctorCheck
		for _, c := range rep.Checks {
			if c.Name == "onepassword" {
				opCheck = c
				break
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": opCheck.Status == "ok", "check": opCheck})
		return nil
	}))
	mux.HandleFunc("GET /assistant-widget", route(true, func(w http.ResponseWriter, _ *http.Request, _ *registry.Device) error {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Del("X-Frame-Options")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src data:; frame-ancestors 'self'")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(assistantWidgetHTML))
		return nil
	}))
}

func registerAssistantChatRoutes(
	mux *http.ServeMux,
	fleet *core.Fleet,
	route func(bool, func(http.ResponseWriter, *http.Request, *registry.Device) error) http.HandlerFunc,
) {
	mux.HandleFunc("GET /v1/assistant/channels", route(true, func(w http.ResponseWriter, _ *http.Request, _ *registry.Device) error {
		chs, err := fleet.Assistant.Store().ListChannels()
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{"channels": chs})
		return nil
	}))
	mux.HandleFunc("POST /v1/assistant/channels", route(true, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		var req struct {
			ID          string `json:"id"`
			Title       string `json:"title"`
			Description string `json:"description"`
			Model       string `json:"model"`
		}
		if err := readJSONBody(r, &req); err != nil {
			return err
		}
		ch, err := fleet.Assistant.UpsertChannel(r.Context(), req.ID, req.Title, req.Description, req.Model)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{"channel": ch})
		return nil
	}))
	mux.HandleFunc("PATCH /v1/assistant/channels/{id}", route(true, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		id := r.PathValue("id")
		cur, err := fleet.Assistant.Store().GetChannel(id)
		if err != nil {
			return err
		}
		var req struct {
			Title       *string `json:"title"`
			Description *string `json:"description"`
			Model       *string `json:"model"`
		}
		if err := readJSONBody(r, &req); err != nil {
			return err
		}
		title := cur.Title
		if req.Title != nil {
			title = *req.Title
		}
		desc := cur.Description
		if req.Description != nil {
			desc = *req.Description
		}
		model := cur.Model
		if req.Model != nil {
			model = *req.Model
		}
		ch, err := fleet.Assistant.Store().UpsertChannel(id, title, desc, model)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{"channel": ch})
		return nil
	}))
	mux.HandleFunc("POST /v1/assistant/channels/{id}/reset", route(true, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		var req struct {
			ClearHistory *bool `json:"clear_history"`
		}
		if err := readJSONBody(r, &req); err != nil {
			return err
		}
		clearHistory := true
		if req.ClearHistory != nil {
			clearHistory = *req.ClearHistory
		}
		ch, err := fleet.Assistant.ResetChannel(r.PathValue("id"), clearHistory)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"channel":                    ch,
			"cleared_history":            clearHistory,
			"long_term_memory_preserved": true,
		})
		return nil
	}))
	mux.HandleFunc("GET /v1/assistant/messages", route(true, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		ch := cmp.Or(strings.TrimSpace(r.URL.Query().Get("channel")), "general")
		msgs, err := fleet.Assistant.Store().ListMessages(ch, queryInt(r, "limit"))
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"channel":  ch,
			"messages": msgs,
			"grant":    fleet.Assistant.ActiveFrontierGrant(ch),
		})
		return nil
	}))
	mux.HandleFunc("POST /v1/assistant/messages", route(true, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		var req struct {
			Channel string   `json:"channel"`
			Content string   `json:"content"`
			Model   string   `json:"model"`
			FileIDs []string `json:"file_ids"`
		}
		if err := readJSONBody(r, &req); err != nil {
			return err
		}
		msg, ev, err := fleet.Assistant.EnqueueUserMessageWithFiles(req.Channel, req.Content, req.Model, req.FileIDs)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"message": msg, "event": ev})
		return nil
	}))
	mux.HandleFunc("GET /v1/assistant/stream", route(true, handleAssistantSSE(fleet)))
	registerAssistantRoutingRoutes(mux, fleet, route)
	registerAssistantActionRoutes(mux, fleet, route)
}

func registerAssistantRoutingRoutes(
	mux *http.ServeMux,
	fleet *core.Fleet,
	route func(bool, func(http.ResponseWriter, *http.Request, *registry.Device) error) http.HandlerFunc,
) {
	mux.HandleFunc("POST /v1/assistant/channels/{id}/retry-frontier", route(true, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		var req struct {
			ReplaceMessageID int64  `json:"replace_message_id"`
			Prompt           string `json:"prompt"`
			GrantTopic       bool   `json:"grant_topic"`
		}
		if err := readJSONBody(r, &req); err != nil {
			return err
		}
		ev, err := fleet.Assistant.RetryWithFrontier(r.PathValue("id"), req.ReplaceMessageID, req.Prompt, req.GrantTopic)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"event": ev})
		return nil
	}))
	mux.HandleFunc("DELETE /v1/assistant/channels/{id}/grant", route(true, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		if err := fleet.Assistant.RevokeFrontierGrant(r.PathValue("id")); err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return nil
	}))
}

func handleAssistantSSE(fleet *core.Fleet) func(http.ResponseWriter, *http.Request, *registry.Device) error {
	return func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		flusher, ok := w.(http.Flusher)
		if !ok {
			return &core.FleetError{Msg: "streaming unsupported", Status: http.StatusInternalServerError}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)

		sub := fleet.Assistant.Subscribe()
		defer fleet.Assistant.Unsubscribe(sub)

		_, _ = fmt.Fprintf(w, "event: ready\ndata: {}\n\n")
		flusher.Flush()

		keepalive := time.NewTicker(25 * time.Second)
		defer keepalive.Stop()
		for {
			select {
			case <-r.Context().Done():
				return nil
			case <-keepalive.C:
				_, _ = fmt.Fprintf(w, ": keepalive\n\n")
				flusher.Flush()
			case ev := <-sub:
				b, err := json.Marshal(ev)
				if err != nil {
					continue
				}
				_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, b)
				flusher.Flush()
			}
		}
	}
}

func registerAssistantActionRoutes(
	mux *http.ServeMux,
	fleet *core.Fleet,
	route func(bool, func(http.ResponseWriter, *http.Request, *registry.Device) error) http.HandlerFunc,
) {
	mux.HandleFunc("GET /v1/assistant/approvals", route(true, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		list, err := fleet.Assistant.Store().ListApprovals(r.URL.Query().Get("status"), queryInt(r, "limit"))
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{"approvals": list})
		return nil
	}))
	mux.HandleFunc("POST /v1/assistant/approvals/{id}/decide", route(true, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		var req struct {
			Decision string `json:"decision"`
			Scope    string `json:"scope"`
			Note     string `json:"note"`
		}
		if err := readJSONBody(r, &req); err != nil {
			return err
		}
		dec := strings.ToLower(strings.TrimSpace(req.Decision))
		if dec != "approve" && dec != "deny" && dec != "approved" && dec != "denied" {
			return &core.FleetError{Msg: "decision must be approve or deny", Status: http.StatusBadRequest}
		}
		apr, err := fleet.Assistant.DecideApprovalWithScope(r.PathValue("id"), dec == "approve" || dec == "approved", req.Scope, req.Note)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{"approval": apr})
		return nil
	}))
	mux.HandleFunc("GET /v1/assistant/timers", route(true, func(w http.ResponseWriter, _ *http.Request, _ *registry.Device) error {
		list, err := fleet.Assistant.Store().ListTimers()
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{"timers": list})
		return nil
	}))
	mux.HandleFunc("POST /v1/assistant/timers", route(true, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		var req assistant.Timer
		if err := readJSONBody(r, &req); err != nil {
			return err
		}
		if strings.TrimSpace(req.Name) == "" || strings.TrimSpace(req.Prompt) == "" {
			return &core.FleetError{Msg: "name and prompt are required", Status: http.StatusBadRequest}
		}
		req.Enabled = true
		tmr, err := fleet.Assistant.Store().UpsertTimer(req)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{"timer": tmr})
		return nil
	}))
	mux.HandleFunc("DELETE /v1/assistant/timers/{id}", route(true, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		if err := fleet.Assistant.Store().DeleteTimer(r.PathValue("id")); err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return nil
	}))
	mux.HandleFunc("GET /v1/assistant/outbox", route(true, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		items, err := fleet.Assistant.Store().ListOutbox(queryInt(r, "limit"), queryBool(r, "undelivered"))
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
		return nil
	}))
	mux.HandleFunc("POST /v1/assistant/outbox/{id}/ack", route(true, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			return &core.FleetError{Msg: "invalid outbox id", Status: http.StatusBadRequest}
		}
		if err := fleet.Assistant.Store().MarkOutboxDelivered(id); err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return nil
	}))
	mux.HandleFunc("GET /v1/assistant/delegations", route(true, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		list, err := fleet.Assistant.Store().ListDelegations(queryInt(r, "limit"))
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{"delegations": list})
		return nil
	}))
	mux.HandleFunc("POST /v1/assistant/notifications/test", route(true, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		if err := fleet.Assistant.TestNotification(r.Context()); err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return nil
	}))
}

func registerAssistantSkillRoutes(
	mux *http.ServeMux,
	fleet *core.Fleet,
	route func(bool, func(http.ResponseWriter, *http.Request, *registry.Device) error) http.HandlerFunc,
) {
	mux.HandleFunc("GET /v1/assistant/skills", route(true, func(w http.ResponseWriter, _ *http.Request, _ *registry.Device) error {
		v, err := fleet.Assistant.Vault()
		if err != nil {
			return err
		}
		skills, err := v.ListSkills()
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{"skills": skills})
		return nil
	}))
	mux.HandleFunc("GET /v1/assistant/skills/{name}", route(true, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		v, err := fleet.Assistant.Vault()
		if err != nil {
			return err
		}
		sk, err := v.GetSkill(r.PathValue("name"))
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{"skill": sk})
		return nil
	}))
	mux.HandleFunc("PUT /v1/assistant/skills/{name}", route(true, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		var req struct {
			Content string `json:"content"`
		}
		if err := readJSONBody(r, &req); err != nil {
			return err
		}
		v, err := fleet.Assistant.Vault()
		if err != nil {
			return err
		}
		sk, err := v.SaveSkill(r.PathValue("name"), req.Content)
		if err != nil {
			return err
		}
		v.MaybeGitCommit(fleet.Assistant.Config().GitAutoCommit, "ui: save skill "+sk.Name)
		writeJSON(w, http.StatusOK, map[string]any{"skill": sk})
		return nil
	}))
	mux.HandleFunc("DELETE /v1/assistant/skills/{name}", route(true, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		v, err := fleet.Assistant.Vault()
		if err != nil {
			return err
		}
		if err := v.DeleteSkill(r.PathValue("name")); err != nil {
			return err
		}
		v.MaybeGitCommit(fleet.Assistant.Config().GitAutoCommit, "ui: delete skill "+r.PathValue("name"))
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return nil
	}))
}

func registerAssistantVaultRoutes(
	mux *http.ServeMux,
	fleet *core.Fleet,
	route func(bool, func(http.ResponseWriter, *http.Request, *registry.Device) error) http.HandlerFunc,
) {
	mux.HandleFunc("GET /v1/assistant/vault/file", route(true, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		rel := r.URL.Query().Get("path")
		v, mount, err := fleet.Assistant.ResolveVault(r.URL.Query().Get("vault"))
		if err != nil {
			return err
		}
		content, err := v.ReadFile(rel)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]string{"vault": mount.Name, "path": rel, "content": content})
		return nil
	}))
	mux.HandleFunc("PUT /v1/assistant/vault/file", route(true, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		var req struct {
			Vault   string `json:"vault"`
			Path    string `json:"path"`
			Content string `json:"content"`
		}
		if err := readJSONBody(r, &req); err != nil {
			return err
		}
		v, mount, err := fleet.Assistant.ResolveVault(req.Vault)
		if err != nil {
			return err
		}
		if mount.ReadOnly {
			return &core.FleetError{Msg: fmt.Sprintf("vault %q is read-only", mount.Name), Status: http.StatusForbidden}
		}
		norm, err := v.WriteFile(req.Path, req.Content)
		if err != nil {
			return err
		}
		v.MaybeGitCommit(fleet.Assistant.Config().GitAutoCommit, "ui: update "+norm)
		writeJSON(w, http.StatusOK, map[string]string{"vault": mount.Name, "path": norm, "status": "saved"})
		return nil
	}))
	mux.HandleFunc("GET /v1/assistant/vault/search", route(true, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		hits, err := fleet.Assistant.SearchVaults(r.URL.Query().Get("vault"), r.URL.Query().Get("q"), queryInt(r, "limit"))
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{"hits": hits})
		return nil
	}))
	mux.HandleFunc("POST /v1/assistant/memory/observe", route(true, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		var req struct {
			Channel string `json:"channel"`
			Reflect bool   `json:"reflect"`
		}
		if err := readJSONBody(r, &req); err != nil {
			return err
		}
		if req.Reflect {
			txt, err := fleet.Assistant.RunReflector(r.Context())
			if err != nil {
				return err
			}
			writeJSON(w, http.StatusOK, map[string]any{"reflected": true, "reflections": txt})
			return nil
		}
		res, err := fleet.Assistant.RunObserver(r.Context(), req.Channel)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, res)
		return nil
	}))
	mux.HandleFunc("GET /v1/assistant/vault/history", route(true, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		v, mount, err := fleet.Assistant.ResolveVault(r.URL.Query().Get("vault"))
		if err != nil {
			return err
		}
		commits, err := v.GitHistory(queryInt(r, "limit"))
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{"vault": mount.Name, "commits": commits})
		return nil
	}))
	mux.HandleFunc("GET /v1/assistant/vault/diff", route(true, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		v, mount, err := fleet.Assistant.ResolveVault(r.URL.Query().Get("vault"))
		if err != nil {
			return err
		}
		sha := r.URL.Query().Get("sha")
		diff, err := v.GitDiff(sha)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{"vault": mount.Name, "sha": sha, "diff": diff})
		return nil
	}))
	mux.HandleFunc("POST /v1/assistant/vault/rollback", route(true, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		var req struct {
			Vault string `json:"vault"`
			SHA   string `json:"sha"`
		}
		if err := readJSONBody(r, &req); err != nil {
			return err
		}
		v, mount, err := fleet.Assistant.ResolveVault(req.Vault)
		if err != nil {
			return err
		}
		if mount.ReadOnly {
			return &core.FleetError{Msg: fmt.Sprintf("vault %q is read-only", mount.Name), Status: http.StatusForbidden}
		}
		commit, err := v.GitRollback(req.SHA)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{"vault": mount.Name, "commit": commit})
		return nil
	}))
}

func registerAssistantSandboxRoutes(
	mux *http.ServeMux,
	fleet *core.Fleet,
	route func(bool, func(http.ResponseWriter, *http.Request, *registry.Device) error) http.HandlerFunc,
) {
	mux.HandleFunc("POST /v1/assistant/files", route(true, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		maxMB := fleet.Assistant.ConfigView().Sandbox.MaxFileMB
		if maxMB <= 0 {
			maxMB = assistant.DefaultSandboxMaxFileMB
		}
		maxLimit := int64(maxMB+1) << 20
		r.Body = http.MaxBytesReader(w, r.Body, maxLimit)
		if err := r.ParseMultipartForm(maxLimit); err != nil {
			return &core.FleetError{Msg: "invalid file upload: " + err.Error(), Status: http.StatusBadRequest}
		}
		defer func() {
			if r.MultipartForm != nil {
				_ = r.MultipartForm.RemoveAll()
			}
		}()
		ch := cmp.Or(strings.TrimSpace(r.FormValue("channel")), "general")
		fh, header, err := r.FormFile("file")
		if err != nil {
			return &core.FleetError{Msg: "multipart field 'file' is required", Status: http.StatusBadRequest}
		}
		defer func() { _ = fh.Close() }()
		rec, err := fleet.Assistant.SaveUploadedFile(ch, header.Filename, header.Header.Get("Content-Type"), fh)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusCreated, map[string]any{"file": rec})
		return nil
	}))
	mux.HandleFunc("GET /v1/assistant/files", route(true, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		ch := cmp.Or(strings.TrimSpace(r.URL.Query().Get("channel")), "general")
		files, err := fleet.Assistant.Store().ListChannelFiles(ch, queryInt(r, "limit"))
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{"channel": ch, "files": files})
		return nil
	}))
	mux.HandleFunc("GET /v1/assistant/files/{id}", route(true, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		f, fh, err := fleet.Assistant.OpenStoredFile(r.PathValue("id"))
		if err != nil {
			return err
		}
		defer func() { _ = fh.Close() }()
		w.Header().Set("Content-Type", cmp.Or(f.MimeType, "application/octet-stream"))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "private, no-store")
		disp := "attachment"
		if queryBool(r, "inline") && assistant.IsSafeInlinePreviewMIME(f.MimeType) {
			disp = "inline"
		}
		w.Header().Set("Content-Disposition", mime.FormatMediaType(disp, map[string]string{"filename": f.Name}))
		http.ServeContent(w, r, f.Name, time.Time{}, fh)
		return nil
	}))
	mux.HandleFunc("POST /v1/assistant/sandbox/build", route(true, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		var req struct {
			RebuildBase bool `json:"rebuild_base"`
		}
		if err := readJSONBody(r, &req); err != nil {
			return err
		}
		res, err := fleet.Assistant.BuildSandboxImages(r.Context(), req.RebuildBase)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, res)
		return nil
	}))
	mux.HandleFunc("POST /v1/assistant/sandbox/reset", route(true, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		var req struct {
			Channel     string `json:"channel"`
			ClearOutput bool   `json:"clear_output"`
		}
		if err := readJSONBody(r, &req); err != nil {
			return err
		}
		ch := cmp.Or(strings.TrimSpace(req.Channel), "general")
		if err := fleet.Assistant.ResetSandbox(r.Context(), ch, req.ClearOutput); err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "channel": ch, "clear_output": req.ClearOutput})
		return nil
	}))
}

func writeMCPOAuthCallbackHTML(w http.ResponseWriter, status int, title, detail string, payload map[string]any) {
	payloadJSON, _ := json.Marshal(payload)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>%s</title>
<style>body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif;max-width:520px;margin:48px auto;padding:0 16px;color:#0f172a;background:#f8fafc;}@media(prefers-color-scheme:dark){body{color:#f1f5f9;background:#0f172a;}.card{background:#1e293b;border-color:#334155;}}.card{background:#fff;border:1px solid #cbd5e1;border-radius:10px;padding:20px;}h2{margin-top:0;font-size:18px;}p{line-height:1.5;word-break:break-word;}</style>
</head><body><div class="card"><h2>%s</h2><p>%s</p><p>You can close this window and return to Fleet Console.</p></div>
<script>try{if(window.opener){window.opener.postMessage(%s,window.location.origin);}}catch(_){}</script>
</body></html>`,
		html.EscapeString(title),
		html.EscapeString(title),
		html.EscapeString(detail),
		string(payloadJSON),
	)
}

func registerAssistantMCPRoutes(
	mux *http.ServeMux,
	fleet *core.Fleet,
	route func(bool, func(http.ResponseWriter, *http.Request, *registry.Device) error) http.HandlerFunc,
) {
	mux.HandleFunc("GET /v1/assistant/mcp", route(true, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		servers, statuses := fleet.Assistant.ListMCPStatuses(r.Context())
		writeJSON(w, http.StatusOK, map[string]any{"servers": servers, "statuses": statuses})
		return nil
	}))
	mux.HandleFunc("POST /v1/assistant/mcp/{name}/oauth/start", route(true, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		var req struct {
			RedirectURI string `json:"redirect_uri"`
		}
		if err := readJSONBody(r, &req); err != nil {
			return err
		}
		res, err := fleet.Assistant.StartMCPOAuth(r.Context(), r.PathValue("name"), req.RedirectURI)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, res)
		return nil
	}))
	mux.HandleFunc("POST /v1/assistant/mcp/{name}/oauth/exchange", route(true, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		var req struct {
			State string `json:"state"`
			Code  string `json:"code"`
		}
		if err := readJSONBody(r, &req); err != nil {
			return err
		}
		res, err := fleet.Assistant.CompleteMCPOAuth(r.Context(), r.PathValue("name"), req.State, req.Code)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, res)
		return nil
	}))
	mux.HandleFunc("DELETE /v1/assistant/mcp/{name}/oauth", route(true, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		if err := fleet.Assistant.RevokeMCPOAuth(r.PathValue("name")); err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return nil
	}))
	mux.HandleFunc("GET /v1/assistant/mcp/oauth/callback", route(true, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		q := r.URL.Query()
		if oauthErr := strings.TrimSpace(q.Get("error")); oauthErr != "" {
			detail := cmp.Or(strings.TrimSpace(q.Get("error_description")), oauthErr)
			writeMCPOAuthCallbackHTML(w, http.StatusBadRequest, "MCP OAuth Authorization Failed", detail, map[string]any{
				"type":  "fleet-mcp-oauth-done",
				"ok":    false,
				"error": detail,
			})
			return nil
		}
		res, err := fleet.Assistant.CompleteMCPOAuthByState(r.Context(), q.Get("state"), q.Get("code"))
		if err != nil {
			writeMCPOAuthCallbackHTML(w, http.StatusBadRequest, "MCP OAuth Authorization Failed", err.Error(), map[string]any{
				"type":  "fleet-mcp-oauth-done",
				"ok":    false,
				"error": err.Error(),
			})
			return nil
		}
		detail := fmt.Sprintf("Authorized %s (%d tool(s) ready).", res.ServerName, res.ToolCount)
		if res.TestError != "" {
			detail = fmt.Sprintf("Authorized %s, but handshake test returned: %s", res.ServerName, res.TestError)
		}
		writeMCPOAuthCallbackHTML(w, http.StatusOK, "MCP OAuth Connected", detail, map[string]any{
			"type":       "fleet-mcp-oauth-done",
			"ok":         true,
			"server":     res.ServerName,
			"tool_count": res.ToolCount,
			"test_error": res.TestError,
		})
		return nil
	}))
}
