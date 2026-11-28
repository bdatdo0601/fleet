package server

import (
	"cmp"
	"net/http"

	"github.com/bdatdo0601/fleet/internal/core"
	"github.com/bdatdo0601/fleet/internal/registry"
)

func requirePersonalCaller(fleet *core.Fleet, c *registry.Device) error {
	if !core.IsPersonalDevice(c) {
		return &core.FleetError{Msg: "personal profile required; device is configured as work", Status: http.StatusForbidden}
	}
	if fleet.Assistant == nil {
		return &core.FleetError{Msg: "assistant is unavailable", Status: http.StatusServiceUnavailable}
	}
	return nil
}

func registerPersonalRoutes(
	mux *http.ServeMux,
	fleet *core.Fleet,
	route func(bool, func(http.ResponseWriter, *http.Request, *registry.Device) error) http.HandlerFunc,
) {
	mux.HandleFunc("GET /v1/personal/mcp/tools", route(false, func(w http.ResponseWriter, r *http.Request, c *registry.Device) error {
		if err := requirePersonalCaller(fleet, c); err != nil {
			return err
		}
		list := fleet.Assistant.ListPersonalMCPTools(r.Context())
		type toolDTO struct {
			FullName        string         `json:"full_name"`
			ServerName      string         `json:"server_name"`
			RemoteName      string         `json:"remote_name"`
			Description     string         `json:"description"`
			InputSchema     map[string]any `json:"input_schema,omitempty"`
			RequireApproval bool           `json:"require_approval"`
		}
		out := make([]toolDTO, 0, len(list))
		for _, t := range list {
			out = append(out, toolDTO{
				FullName:        t.FullName,
				ServerName:      t.ServerName,
				RemoteName:      t.RemoteName,
				Description:     t.Description,
				InputSchema:     t.InputSchema,
				RequireApproval: t.RequireApproval,
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{"tools": out})
		return nil
	}))

	mux.HandleFunc("POST /v1/personal/mcp/call", route(false, func(w http.ResponseWriter, r *http.Request, c *registry.Device) error {
		if err := requirePersonalCaller(fleet, c); err != nil {
			return err
		}
		var req struct {
			Tool      string         `json:"tool"`
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := readJSONBody(r, &req); err != nil {
			return err
		}
		fullName := cmp.Or(req.Tool, req.Name)
		if fullName == "" {
			return &core.FleetError{Msg: "tool name is required", Status: http.StatusBadRequest}
		}
		res, err := fleet.Assistant.CallPersonalMCPTool(r.Context(), c.Name, fullName, req.Arguments)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{"result": res})
		return nil
	}))

	mux.HandleFunc("GET /v1/personal/vault/search", route(false, func(w http.ResponseWriter, r *http.Request, c *registry.Device) error {
		if err := requirePersonalCaller(fleet, c); err != nil {
			return err
		}
		q := r.URL.Query()
		res, err := fleet.Assistant.PersonalVaultSearch(r.Context(), cmp.Or(q.Get("q"), q.Get("query")), q.Get("vault"), queryInt(r, "limit"))
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, res)
		return nil
	}))

	mux.HandleFunc("GET /v1/personal/vault/read", route(false, func(w http.ResponseWriter, r *http.Request, c *registry.Device) error {
		if err := requirePersonalCaller(fleet, c); err != nil {
			return err
		}
		q := r.URL.Query()
		res, err := fleet.Assistant.PersonalVaultRead(q.Get("vault"), q.Get("path"))
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, res)
		return nil
	}))

	mux.HandleFunc("POST /v1/personal/vault/capture", route(false, func(w http.ResponseWriter, r *http.Request, c *registry.Device) error {
		if err := requirePersonalCaller(fleet, c); err != nil {
			return err
		}
		var req struct {
			Vault   string `json:"vault"`
			Path    string `json:"path"`
			Content string `json:"content"`
			Append  bool   `json:"append"`
		}
		if err := readJSONBody(r, &req); err != nil {
			return err
		}
		res, err := fleet.Assistant.PersonalVaultCapture(r.Context(), req.Vault, req.Path, req.Content, req.Append)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, res)
		return nil
	}))
}
