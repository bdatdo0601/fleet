package assistant

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
)

// ListPersonalMCPTools returns external MCP tools whose server explicitly opts into personal_devices exposure.
func (e *Engine) ListPersonalMCPTools(ctx context.Context) []ExternalTool {
	if e == nil || e.mcp == nil {
		return nil
	}
	allowedServers := make(map[string]bool)
	for _, srv := range e.Config().MCPServers {
		if srv.Enabled && NormalizeExposure(srv.Exposure) == ExposurePersonalDevices {
			allowedServers[strings.ToLower(srv.Name)] = true
		}
	}
	if len(allowedServers) == 0 {
		return nil
	}
	all := e.mcp.ListTools(ctx)
	out := make([]ExternalTool, 0, len(all))
	for _, t := range all {
		if allowedServers[strings.ToLower(t.ServerName)] {
			out = append(out, t)
		}
	}
	return out
}

// CallPersonalMCPTool invokes an external MCP tool exposed to personal devices, enforcing exposure and HITL approvals in #fleet-ops.
func (e *Engine) CallPersonalMCPTool(ctx context.Context, callerDevice, fullName string, args map[string]any) (any, error) {
	if e == nil || e.mcp == nil {
		return nil, newErr("MCP manager unavailable", http.StatusServiceUnavailable)
	}
	ext, ok := e.mcp.FindTool(ctx, fullName)
	if !ok {
		return nil, newErr(fmt.Sprintf("personal MCP tool %q not found", fullName), http.StatusNotFound)
	}
	srv, err := e.findMCPServer(ext.ServerName)
	if err != nil {
		return nil, err
	}
	if NormalizeExposure(srv.Exposure) != ExposurePersonalDevices {
		return nil, newErr(fmt.Sprintf("MCP server %q is restricted to assistant_only", srv.Name), http.StatusForbidden)
	}
	if !srv.Enabled {
		return nil, newErr(fmt.Sprintf("MCP server %q is disabled", srv.Name), http.StatusForbidden)
	}
	rawArgsBytes, _ := json.Marshal(cmp.Or[any](args, map[string]any{}))
	rawArgs := string(rawArgsBytes)

	cfg := e.Config()
	if ext.RequireApproval && !cfg.AutoApprove {
		return e.queuePersonalApproval(ctx, cfg, ext.FullName, ext.ServerName, "personal_mcp", fmt.Sprintf("[%s] Execute personal MCP tool %s", cmp.Or(callerDevice, "personal"), ext.FullName), rawArgs)
	}
	res, callErr := e.callExternalMCPWithElicitation(ctx, ext.FullName, args, cfg.AutoApprove)
	if callErr != nil {
		if aprReq, ok := errors.AsType[*approvalRequiredError](callErr); ok {
			return e.queuePersonalApproval(ctx, cfg, aprReq.ToolName, aprReq.ServerName, aprReq.RiskLevel, fmt.Sprintf("[%s] %s", cmp.Or(callerDevice, "personal"), aprReq.Summary), aprReq.ArgsJSON)
		}
		return nil, callErr
	}
	return res, nil
}

func (e *Engine) queuePersonalApproval(ctx context.Context, cfg Config, toolName, serverName, riskLevel, summary, argsJSON string) (map[string]any, error) {
	apr, err := e.store.CreateApproval(Approval{
		ChannelID:  "fleet-ops",
		ToolName:   toolName,
		ServerName: serverName,
		ArgsJSON:   cmp.Or(argsJSON, "{}"),
		Summary:    summary,
		RiskLevel:  cmp.Or(riskLevel, "personal_mcp"),
		Status:     "pending",
	})
	if err != nil {
		return nil, err
	}
	e.publishEvent(StreamEvent{Type: "approval", ChannelID: "fleet-ops", Data: apr})
	e.appendSystemChannelNotice("fleet-ops", "personal_approval", fmt.Sprintf("Personal device requested approval `%s`: **%s**\nApprove or deny in Fleet Console or Discord `#fleet-ops`.", apr.ID, apr.Summary))
	sentInteractive := e.sendApprovalToDiscord(ctx, cfg, Channel{ID: "fleet-ops", Title: "#fleet-ops"}, apr)
	if !sentInteractive && cfg.Notifications.Enabled && (cfg.Notifications.NotifyApprovals == nil || *cfg.Notifications.NotifyApprovals) {
		e.spawnBG(ctx, 10*time.Second, func(nCtx context.Context) {
			_ = e.dispatchNotification(nCtx, Notification{
				Category:  "approval",
				ChannelID: "fleet-ops",
				Priority:  "urgent",
				Title:     "Approval needed: " + apr.ToolName,
				Summary:   apr.Summary,
				Fields: []NotificationField{
					{Name: "Channel", Value: "#fleet-ops", Inline: true},
					{Name: "Risk", Value: apr.RiskLevel, Inline: true},
				},
			})
		})
	}
	return map[string]any{
		"status":      "approval_required",
		"approval_id": apr.ID,
		"channel_id":  "fleet-ops",
		"summary":     apr.Summary,
	}, nil
}

func (e *Engine) exposedVaultNames() map[string]bool {
	cfg := e.Config()
	out := make(map[string]bool)
	if NormalizeExposure(cfg.VaultExposure) == ExposurePersonalDevices {
		out[PrimaryVaultName] = true
	}
	for _, m := range cfg.ExtraVaults {
		if NormalizeExposure(m.Exposure) == ExposurePersonalDevices {
			out[strings.ToLower(m.Name)] = true
		}
	}
	return out
}

func (e *Engine) resolvePersonalVault(vaultName string) (*Vault, VaultMount, error) {
	clean := strings.ToLower(cmp.Or(strings.TrimSpace(vaultName), PrimaryVaultName))
	exposed := e.exposedVaultNames()
	if !exposed[clean] {
		return nil, VaultMount{}, newErr(fmt.Sprintf("vault %q is restricted to assistant_only", clean), http.StatusForbidden)
	}
	return e.ResolveVault(clean)
}

// PersonalVaultSearch searches Obsidian vaults whose exposure is set to personal_devices.
func (e *Engine) PersonalVaultSearch(ctx context.Context, query, vaultName string, limit int) (any, error) {
	cleanQuery := strings.TrimSpace(query)
	if cleanQuery == "" {
		return nil, newErr("query is required", http.StatusBadRequest)
	}
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	exposed := e.exposedVaultNames()
	if len(exposed) == 0 {
		return nil, newErr("no vaults have exposure=personal_devices enabled", http.StatusForbidden)
	}
	target := strings.ToLower(strings.TrimSpace(vaultName))
	var names []string
	if target == "" || target == "all" {
		for k := range exposed {
			names = append(names, k)
		}
		slices.Sort(names)
	} else {
		if !exposed[target] {
			return nil, newErr(fmt.Sprintf("vault %q is restricted to assistant_only", target), http.StatusForbidden)
		}
		names = []string{target}
	}
	type personalHit struct {
		Vault   string  `json:"vault"`
		Path    string  `json:"path"`
		Title   string  `json:"title"`
		Heading string  `json:"heading,omitempty"`
		Snippet string  `json:"snippet"`
		Score   float64 `json:"score,omitempty"`
	}
	var results []personalHit
	for _, name := range names {
		v, _, err := e.ResolveVault(name)
		if err != nil {
			continue
		}
		hits, sErr := v.Search(cleanQuery, limit)
		if sErr != nil {
			continue
		}
		for _, h := range hits {
			results = append(results, personalHit{
				Vault:   name,
				Path:    h.Path,
				Title:   h.Title,
				Heading: h.Heading,
				Snippet: h.Snippet,
				Score:   h.Score,
			})
			if len(results) >= limit {
				break
			}
		}
		if len(results) >= limit {
			break
		}
	}
	_ = ctx
	return map[string]any{"query": cleanQuery, "vaults": names, "hits": results}, nil
}

// PersonalVaultRead reads one Markdown note from a personal_devices-exposed vault.
func (e *Engine) PersonalVaultRead(vaultName, relPath string) (any, error) {
	v, mount, err := e.resolvePersonalVault(vaultName)
	if err != nil {
		return nil, err
	}
	content, err := v.ReadFile(relPath)
	if err != nil {
		return nil, err
	}
	return map[string]any{"vault": mount.Name, "path": relPath, "content": content}, nil
}

// PersonalVaultCapture writes or appends a note into an auto-writable folder of a personal_devices-exposed vault.
func (e *Engine) PersonalVaultCapture(ctx context.Context, vaultName, relPath, content string, appendMode bool) (any, error) {
	v, mount, err := e.resolvePersonalVault(vaultName)
	if err != nil {
		return nil, err
	}
	if mount.ReadOnly {
		return nil, newErr(fmt.Sprintf("vault %q is read-only", mount.Name), http.StatusForbidden)
	}
	if !IsAutoWritable(relPath, mount.AutoWriteDirs) {
		return nil, newErr(fmt.Sprintf("path %q is outside auto-write directories (%s) of vault %q", relPath, strings.Join(mount.AutoWriteDirs, ", "), mount.Name), http.StatusForbidden)
	}
	var clean string
	if appendMode {
		clean, err = v.AppendFile(relPath, content)
	} else {
		clean, err = v.WriteFile(relPath, content)
	}
	if err != nil {
		return nil, err
	}
	if mount.Name == PrimaryVaultName {
		v.MaybeGitCommitContext(ctx, e.Config().GitAutoCommit, "personal-mcp: update "+clean)
	}
	return map[string]any{"vault": mount.Name, "path": clean, "status": "saved"}, nil
}
