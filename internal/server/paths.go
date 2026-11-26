package server

import (
	"cmp"
	"fmt"
	"net/http"
	"path"
	"slices"
	"strings"

	"github.com/bdatdo0601/fleet/internal/agents"
	"github.com/bdatdo0601/fleet/internal/core"
	"github.com/bdatdo0601/fleet/internal/registry"
	"github.com/bdatdo0601/fleet/internal/slacker"
)

func isSkippableWorkspaceParent(dir string) bool {
	if dir == "" || dir == "." || dir == "/" || dir == "~" ||
		dir == "/Users" || dir == "/home" || dir == "/tmp" || dir == "/var" ||
		dir == "/private" || dir == "/private/tmp" || dir == "/private/var" {
		return true
	}
	if strings.HasPrefix(dir, "/tmp/") || strings.HasPrefix(dir, "/var/") || strings.HasPrefix(dir, "/private/var/") {
		return true
	}
	return agents.ToTildePath(dir) == "~"
}

func appendUniqueStr(dst []string, val string) []string {
	if val == "" || slices.Contains(dst, val) {
		return dst
	}
	return append(dst, val)
}

func buildWorkspaceSuggestions(cfg slacker.Config, list []agents.TrackedPath, deviceFilter string) map[string][]string {
	out := map[string][]string{}
	for dev, dc := range cfg.Devices {
		if deviceFilter != "" && dev != deviceFilter {
			continue
		}
		if dc.WorkspaceDir != "" {
			out[dev] = appendUniqueStr(out[dev], dc.WorkspaceDir)
		}
	}
	for _, item := range list {
		parent := agents.NormalizeTrackedPath(path.Dir(item.Path))
		if isSkippableWorkspaceParent(parent) {
			continue
		}
		if tilde := agents.ToTildePath(parent); tilde != "" {
			out[item.Device] = appendUniqueStr(out[item.Device], tilde)
		}
		out[item.Device] = appendUniqueStr(out[item.Device], parent)
	}
	return out
}

func resolvePathTargetDevice(fleet *core.Fleet, caller *registry.Device, requested string, validateActive bool) (string, error) {
	dev := strings.TrimSpace(requested)
	if !core.IsAdmin(caller) {
		if dev != "" && dev != caller.Name {
			return "", &core.FleetError{Msg: fmt.Sprintf("%s cannot manage paths on %s", caller.Name, dev), Status: http.StatusForbidden}
		}
		return caller.Name, nil
	}
	dev = cmp.Or(dev, caller.Name)
	if validateActive {
		if active, err := fleet.ActiveDeviceNames(); err == nil && len(active) > 0 && !active[dev] {
			return "", &core.FleetError{Msg: fmt.Sprintf("unknown or revoked device %q", dev), Status: http.StatusBadRequest}
		}
	}
	return dev, nil
}

func handleAgentPathsGet(fleet *core.Fleet) func(http.ResponseWriter, *http.Request, *registry.Device) error {
	return func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		q := r.URL.Query()
		dev := strings.TrimSpace(q.Get("device"))
		list, err := fleet.Agents.ListPaths(dev, q.Get("q"))
		if err != nil {
			return err
		}
		cfg, _ := slacker.LoadConfig(fleet.SlackerConfigPath)
		for i := range list {
			list[i].TildePath = agents.ToTildePath(list[i].Path)
			list[i].Subpath = agents.WorkspaceSubpath(cfg.Devices[list[i].Device].WorkspaceDir, list[i].Path)
		}
		repos, err := fleet.Agents.KnownRepos(dev)
		if err != nil {
			return err
		}
		if repos == nil {
			repos = []string{}
		}
		workspaces := buildWorkspaceSuggestions(cfg, list, dev)
		writeJSON(w, http.StatusOK, map[string]any{
			"paths":      list,
			"repos":      repos,
			"workspaces": workspaces,
		})
		return nil
	}
}

func handleAgentPathsPost(fleet *core.Fleet) func(http.ResponseWriter, *http.Request, *registry.Device) error {
	return func(w http.ResponseWriter, r *http.Request, c *registry.Device) error {
		var req struct {
			Device string `json:"device"`
			Path   string `json:"path"`
			Repo   string `json:"repo"`
		}
		if err := readJSONBody(r, &req); err != nil {
			return err
		}
		dev, err := resolvePathTargetDevice(fleet, c, req.Device, true)
		if err != nil {
			return err
		}
		rawPath := strings.TrimSpace(req.Path)
		if rawPath == "" {
			return &core.FleetError{Msg: "path is required", Status: http.StatusBadRequest}
		}
		cfg, _ := slacker.LoadConfig(fleet.SlackerConfigPath)
		if !strings.HasPrefix(rawPath, "/") && rawPath != "~" && !strings.HasPrefix(rawPath, "~/") {
			resolved, rErr := cfg.ResolveRepoPath(dev, rawPath)
			if rErr != nil {
				return rErr
			}
			rawPath = resolved
		}
		norm := agents.NormalizeTrackedPath(rawPath)
		if norm == "" {
			return &core.FleetError{Msg: "valid directory path is required", Status: http.StatusBadRequest}
		}
		repo := cmp.Or(strings.TrimSpace(req.Repo), agents.RepoNameFromPath(norm))
		if err := fleet.Agents.RecordPath(dev, norm, repo, "manual", true); err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":         true,
			"device":     dev,
			"path":       norm,
			"tilde_path": agents.ToTildePath(norm),
			"subpath":    agents.WorkspaceSubpath(cfg.Devices[dev].WorkspaceDir, norm),
			"repo":       repo,
		})
		return nil
	}
}

func handleAgentPathsDelete(fleet *core.Fleet) func(http.ResponseWriter, *http.Request, *registry.Device) error {
	return func(w http.ResponseWriter, r *http.Request, c *registry.Device) error {
		q := r.URL.Query()
		dev := strings.TrimSpace(q.Get("device"))
		rawPath := strings.TrimSpace(q.Get("path"))
		if rawPath == "" && r.ContentLength != 0 {
			var req struct {
				Device string `json:"device"`
				Path   string `json:"path"`
			}
			if err := readJSONBody(r, &req); err != nil {
				return err
			}
			dev = cmp.Or(strings.TrimSpace(req.Device), dev)
			rawPath = strings.TrimSpace(req.Path)
		}
		resolvedDev, err := resolvePathTargetDevice(fleet, c, dev, false)
		if err != nil {
			return err
		}
		deleted, err := fleet.Agents.DeletePath(resolvedDev, rawPath)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": deleted})
		return nil
	}
}

func registerAgentPathRoutes(
	mux *http.ServeMux,
	fleet *core.Fleet,
	route func(bool, func(http.ResponseWriter, *http.Request, *registry.Device) error) http.HandlerFunc,
) {
	mux.HandleFunc("GET /v1/agents/paths", route(false, handleAgentPathsGet(fleet)))
	mux.HandleFunc("POST /v1/agents/paths", route(false, handleAgentPathsPost(fleet)))
	mux.HandleFunc("DELETE /v1/agents/paths", route(false, handleAgentPathsDelete(fleet)))
}
