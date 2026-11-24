package server

import (
	"cmp"
	"net/http"
	"strconv"
	"strings"

	"github.com/bdatdo0601/fleet/internal/core"
	"github.com/bdatdo0601/fleet/internal/memory"
	"github.com/bdatdo0601/fleet/internal/registry"
)

func queryBool(r *http.Request, key string) bool {
	v := strings.ToLower(strings.TrimSpace(r.URL.Query().Get(key)))
	return v == "1" || v == "true" || v == "yes"
}

func queryInt(r *http.Request, key string) int {
	n, err := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get(key)))
	if err != nil {
		return 0
	}
	return n
}

func splitNonEmptyCSV(raw string) []string {
	var out []string
	for part := range strings.SplitSeq(raw, ",") {
		if t := strings.TrimSpace(part); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func registerMemoryRoutes(
	mux *http.ServeMux,
	fleet *core.Fleet,
	route func(bool, func(http.ResponseWriter, *http.Request, *registry.Device) error) http.HandlerFunc,
) {
	mux.HandleFunc("GET /v1/memory", route(false, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		q := r.URL.Query()
		items, err := fleet.Memory.Search(memory.SearchFilter{
			Query:           q.Get("q"),
			Scope:           q.Get("scope"),
			Scopes:          splitNonEmptyCSV(q.Get("scopes")),
			Kind:            q.Get("kind"),
			Tier:            q.Get("tier"),
			Status:          q.Get("status"),
			IncludeArchived: queryBool(r, "include_archived"),
			Limit:           queryInt(r, "limit"),
		})
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{"memories": items})
		return nil
	}))

	mux.HandleFunc("GET /v1/memory/context", route(false, func(w http.ResponseWriter, r *http.Request, c *registry.Device) error {
		q := r.URL.Query()
		ctxRes, err := fleet.Memory.RenderContext(q.Get("repo"), cmp.Or(q.Get("device"), c.Name))
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, ctxRes)
		return nil
	}))

	mux.HandleFunc("GET /v1/memory/item", route(false, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		q := r.URL.Query()
		detail, err := fleet.Memory.Get(q.Get("scope"), q.Get("key"), queryBool(r, "history"))
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, detail)
		return nil
	}))

	mux.HandleFunc("POST /v1/memory", route(false, func(w http.ResponseWriter, r *http.Request, c *registry.Device) error {
		var req memory.UpsertParams
		if err := readJSONBody(r, &req); err != nil {
			return err
		}
		req.Device = cmp.Or(req.Device, c.Name)
		res, err := fleet.Memory.Upsert(req)
		if err != nil {
			return err
		}
		if res.Queued && fleet.Curator != nil {
			fleet.Curator.Notify()
		}
		writeJSON(w, http.StatusOK, res)
		return nil
	}))

	mux.HandleFunc("DELETE /v1/memory/item", route(false, func(w http.ResponseWriter, r *http.Request, c *registry.Device) error {
		q := r.URL.Query()
		scope := q.Get("scope")
		key := q.Get("key")
		hard := queryBool(r, "hard")
		harness := q.Get("harness")
		sessionID := q.Get("session_id")
		reason := q.Get("reason")
		if scope == "" && key == "" && r.ContentLength != 0 {
			var body struct {
				Scope     string `json:"scope"`
				Key       string `json:"key"`
				Hard      bool   `json:"hard"`
				Harness   string `json:"harness"`
				SessionID string `json:"session_id"`
				Reason    string `json:"reason"`
			}
			if err := readJSONBody(r, &body); err != nil {
				return err
			}
			scope, key, hard = body.Scope, body.Key, body.Hard
			harness, sessionID, reason = body.Harness, body.SessionID, body.Reason
		}
		if err := fleet.Memory.Delete(scope, key, hard, c.Name, harness, sessionID, reason); err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return nil
	}))

	mux.HandleFunc("POST /v1/memory/sync", route(false, func(w http.ResponseWriter, r *http.Request, c *registry.Device) error {
		var req struct {
			Items []memory.UpsertParams `json:"items"`
		}
		if err := readJSONBody(r, &req); err != nil {
			return err
		}
		res, err := fleet.Memory.SyncBatch(c.Name, req.Items)
		if err != nil {
			return err
		}
		if res.AnyQueued && fleet.Curator != nil {
			fleet.Curator.Notify()
		}
		writeJSON(w, http.StatusOK, res)
		return nil
	}))

	registerMemoryHistoryRoutes(mux, fleet, route)
	registerMemoryCuratorRoutes(mux, fleet, route)
}

func registerMemoryHistoryRoutes(
	mux *http.ServeMux,
	fleet *core.Fleet,
	route func(bool, func(http.ResponseWriter, *http.Request, *registry.Device) error) http.HandlerFunc,
) {
	mux.HandleFunc("GET /v1/memory/history", route(false, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		q := r.URL.Query()
		revs, err := fleet.Memory.History(q.Get("scope"), q.Get("key"), queryInt(r, "limit"))
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{"revisions": revs})
		return nil
	}))

	mux.HandleFunc("POST /v1/memory/revert", route(false, func(w http.ResponseWriter, r *http.Request, c *registry.Device) error {
		var req struct {
			Scope    string `json:"scope"`
			Key      string `json:"key"`
			Revision int    `json:"revision"`
		}
		if err := readJSONBody(r, &req); err != nil {
			return err
		}
		item, err := fleet.Memory.Revert(req.Scope, req.Key, req.Revision, c.Name)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{"memory": item})
		return nil
	}))

	mux.HandleFunc("POST /v1/memory/promote", route(false, func(w http.ResponseWriter, r *http.Request, c *registry.Device) error {
		var req struct {
			FromScope string `json:"from_scope"`
			FromKey   string `json:"from_key"`
			ToScope   string `json:"to_scope"`
			ToKey     string `json:"to_key"`
			Tier      string `json:"tier"`
			Summary   string `json:"summary"`
			Lock      bool   `json:"lock"`
		}
		if err := readJSONBody(r, &req); err != nil {
			return err
		}
		item, err := fleet.Memory.Promote(req.FromScope, req.FromKey, req.ToScope, req.ToKey, req.Tier, req.Summary, c.Name, req.Lock)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{"memory": item})
		return nil
	}))

	mux.HandleFunc("GET /v1/memory/proposals", route(false, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		if idStr := strings.TrimSpace(r.URL.Query().Get("id")); idStr != "" {
			id, err := strconv.ParseInt(idStr, 10, 64)
			if err != nil || id <= 0 {
				return &memory.MemoryError{Msg: "invalid proposal id", Status: http.StatusBadRequest}
			}
			prop, err := fleet.Memory.GetProposal(id)
			if err != nil {
				return err
			}
			writeJSON(w, http.StatusOK, map[string]any{"proposal": prop})
			return nil
		}
		q := r.URL.Query()
		props, err := fleet.Memory.ListProposals(q.Get("status"), queryInt(r, "limit"))
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{"proposals": props})
		return nil
	}))

	mux.HandleFunc("POST /v1/memory/proposals/resolve", route(false, func(w http.ResponseWriter, r *http.Request, c *registry.Device) error {
		var req struct {
			ID       int64  `json:"id"`
			Decision string `json:"decision"`
		}
		if err := readJSONBody(r, &req); err != nil {
			return err
		}
		dec := strings.ToLower(strings.TrimSpace(req.Decision))
		if dec != "approve" && dec != "approved" && dec != "reject" && dec != "rejected" {
			return &memory.MemoryError{Msg: "decision must be approve or reject", Status: http.StatusBadRequest}
		}
		approve := dec == "approve" || dec == "approved"
		prop, item, err := fleet.Memory.ResolveProposal(req.ID, approve, c.Name)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{"proposal": prop, "memory": item})
		return nil
	}))
}

func registerMemoryCuratorRoutes(
	mux *http.ServeMux,
	fleet *core.Fleet,
	route func(bool, func(http.ResponseWriter, *http.Request, *registry.Device) error) http.HandlerFunc,
) {
	mux.HandleFunc("GET /v1/memory/curator", route(false, func(w http.ResponseWriter, _ *http.Request, _ *registry.Device) error {
		writeJSON(w, http.StatusOK, fleet.Curator.Status())
		return nil
	}))

	mux.HandleFunc("POST /v1/memory/curator", route(true, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		var req struct {
			memory.CuratorConfig
			Disable bool  `json:"disable"`
			Test    *bool `json:"test"`
		}
		if err := readJSONBody(r, &req); err != nil {
			return err
		}
		if req.Disable {
			st, err := fleet.Curator.Disable()
			if err != nil {
				return err
			}
			writeJSON(w, http.StatusOK, st)
			return nil
		}
		st, err := fleet.Curator.Configure(r.Context(), req.CuratorConfig, optBoolDefaultTrue(req.Test))
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, st)
		return nil
	}))

	mux.HandleFunc("POST /v1/memory/curator/run", route(true, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		var req struct {
			Limit      int  `json:"limit"`
			DryRun     bool `json:"dry_run"`
			IncludeAll bool `json:"include_all"`
		}
		if err := readJSONBody(r, &req); err != nil {
			return err
		}
		reps, err := fleet.Curator.RunPending(r.Context(), req.Limit, req.DryRun, req.IncludeAll)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"reports": reps,
			"status":  fleet.Curator.Status(),
		})
		return nil
	}))
}
