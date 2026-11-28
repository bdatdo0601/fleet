package assistant

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func buildSandboxPromptSection(cfg Config) string {
	if !cfg.Sandbox.Enabled {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n## Sandboxed Computer (/work)\n")
	b.WriteString("- Use `sandbox_*` tools for reading, analyzing, converting, or generating files (PDF, DOCX, XLSX, PPTX, CSV, static chart image files, OCR, archives); use `render_widget` for inline interactive Generative UI charts in `#chat`. Do NOT use `fleet_spawn_agent` for document/file tasks.\n")
	b.WriteString("- Uploaded chat attachments are staged in `/work/in/`. Write generated deliverables to `/work/out/` and call `sandbox_publish_file` so the user receives a downloadable card/preview in `#chat`.\n")
	b.WriteString("- Pre-installed offline tools: Python 3 (`pypdf`, `pdfplumber`, `pymupdf`, `weasyprint`, `reportlab`, `python-docx`, `openpyxl`, `python-pptx`, `pandas`, `duckdb`, `matplotlib`, `pillow`, `tabulate`), `pandoc`, `poppler-utils` (`pdftotext`, `pdftoppm`), `tesseract`, `sqlite3`, `jq`, `ripgrep`, `uv`.\n")
	if len(cfg.Sandbox.Profiles) > 0 {
		fmt.Fprintf(&b, "- Enabled sandbox profiles: [%s]\n", strings.Join(cfg.Sandbox.Profiles, ", "))
		for _, p := range cfg.Sandbox.Profiles {
			switch p {
			case "browser":
				b.WriteString("  - `browser`: `/usr/bin/chromium` + `playwright` installed (always launch Chromium with `--no-sandbox --disable-dev-shm-usage`).\n")
			case "office":
				b.WriteString("  - `office`: LibreOffice installed (`libreoffice --headless --convert-to pdf` for `.docx`/`.xlsx`/`.pptx`).\n")
			case "media":
				b.WriteString("  - `media`: `ffmpeg` installed for audio/video conversion and frame extraction.\n")
			}
		}
	}
	if len(cfg.Sandbox.ExtraAptPackages) > 0 || len(cfg.Sandbox.ExtraPipPackages) > 0 {
		fmt.Fprintf(&b, "- Extra installed packages: apt=[%s] pip=[%s]\n",
			strings.Join(cfg.Sandbox.ExtraAptPackages, ", "),
			strings.Join(cfg.Sandbox.ExtraPipPackages, ", "))
	}
	b.WriteString("- Sandbox runs offline by default; set `internet=true` (with `allowed_hosts` and `reason`) only when external network access is required (triggers user approval). If a needed package is missing, call `sandbox_install_packages` (user approves once and it is permanently baked into the sandbox image).\n")
	return b.String()
}

func sandboxToolSpecs() []ToolSpec {
	return []ToolSpec{
		makeToolSpec("sandbox_exec", "Run a Python or shell script/command inside the channel's isolated sandbox micro-VM (/work). Offline by default; internet=true triggers human approval.", schemaObj(map[string]any{
			"script":        strProp("Inline script body to execute (preferred for multi-line Python/sh)"),
			"command":       strProp("Shell command to execute via sh -c (alternative to script)"),
			"lang":          strProp("python (default) or sh"),
			"workdir":       strProp("Relative working directory inside /work (default .)"),
			"timeout_sec":   intProp("Execution timeout in seconds (default 30, max 300)"),
			"internet":      boolProp("Request approval-gated HTTP/HTTPS egress proxy for this run"),
			"allowed_hosts": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Optional public hostnames/wildcards allowed when internet=true"},
			"reason":        strProp("Short reason shown to the user if approval is required"),
		})),
		makeToolSpec("sandbox_list_files", "List files and directories inside sandbox /work (or a subfolder such as in or out).", schemaObj(map[string]any{
			"dir": strProp("Relative directory inside /work (default \".\")"),
		})),
		makeToolSpec("sandbox_read_file", "Read UTF-8 text from a file inside sandbox /work.", schemaObj(map[string]any{
			"path":      strProp("Path inside /work (e.g. out/summary.txt)"),
			"max_bytes": intProp("Max bytes to read (default 65536)"),
		}, "path")),
		makeToolSpec("sandbox_write_file", "Write UTF-8 text to a file inside sandbox /work.", schemaObj(map[string]any{
			"path":    strProp("Path inside /work (e.g. script.py or out/notes.md)"),
			"content": strProp("File text content"),
		}, "path", "content")),
		makeToolSpec("sandbox_publish_file", "Publish a generated file from sandbox /work (e.g. out/report.pdf) as a downloadable card/preview in #chat.", schemaObj(map[string]any{
			"path":  strProp("Relative or /work-prefixed path of the generated file"),
			"title": strProp("Optional card title"),
		}, "path")),
		makeToolSpec("sandbox_vault_bridge", "Copy a file between an Obsidian vault and sandbox /work (export respects vault write/approval policies).", schemaObj(map[string]any{
			"action":       strProp("import (vault -> /work/in) or export (/work -> vault)"),
			"vault":        strProp("Vault name (default \"agent\")"),
			"vault_path":   strProp("Relative path inside the Obsidian vault"),
			"sandbox_path": strProp("Path inside sandbox /work (required for export; optional for import)"),
		}, "action", "vault_path")),
		makeToolSpec("sandbox_install_packages", "Request one-time human approval to permanently add apt or pip packages to the sandbox image and rebuild it.", schemaObj(map[string]any{
			"apt_packages": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Debian apt package names"},
			"pip_packages": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "PyPI package specifiers"},
			"reason":       strProp("Why these packages are needed"),
		})),
		makeToolSpec("sandbox_reset", "Destroy and reset the channel sandbox micro-VM back to a clean state.", schemaObj(map[string]any{
			"clear_output": boolProp("Also clear /work/out/"),
		})),
	}
}

func (e *Engine) dispatchSandboxTool(ctx context.Context, name string, args map[string]any, rawArgs string, tctx *toolExecContext) (any, error) {
	if !tctx.Config.Sandbox.Enabled {
		return nil, newErr("sandbox is disabled; enable it in Assistant Settings", http.StatusBadRequest)
	}
	if e.sandbox == nil {
		return nil, newErr("sandbox manager unavailable", http.StatusServiceUnavailable)
	}
	switch name {
	case "sandbox_exec":
		return e.execSandboxExecTool(ctx, args, rawArgs, tctx)
	case "sandbox_list_files":
		return e.sandbox.ListFiles(tctx.ChannelID, strArg(args, "dir"))
	case "sandbox_read_file":
		content, size, truncated, err := e.sandbox.ReadTextFile(tctx.ChannelID, strArg(args, "path"), intArg(args, "max_bytes", 65536))
		if err != nil {
			return nil, err
		}
		return map[string]any{"path": strArg(args, "path"), "size_bytes": size, "truncated": truncated, "content": content}, nil
	case "sandbox_write_file":
		rel, err := e.sandbox.WriteTextFile(tctx.ChannelID, strArg(args, "path"), strArg(args, "content"))
		if err != nil {
			return nil, err
		}
		return map[string]any{"path": rel, "status": "written"}, nil
	case "sandbox_publish_file":
		f, w, err := e.PublishSandboxFile(tctx.ChannelID, strArg(args, "path"), strArg(args, "title"))
		if err != nil {
			return nil, err
		}
		if tctx.Widgets != nil {
			*tctx.Widgets = append(*tctx.Widgets, w)
		}
		return map[string]any{"published": true, "file": f}, nil
	case "sandbox_vault_bridge":
		return e.execSandboxVaultBridgeTool(ctx, args, tctx)
	case "sandbox_install_packages":
		return e.execSandboxInstallPackagesTool(ctx, args, rawArgs, tctx)
	case "sandbox_reset":
		if err := e.sandbox.Reset(ctx, tctx.ChannelID, boolArg(args, "clear_output")); err != nil {
			return nil, err
		}
		return map[string]any{"reset": true, "clear_output": boolArg(args, "clear_output")}, nil
	default:
		return nil, newErr("unknown sandbox tool "+name, http.StatusBadRequest)
	}
}

func (e *Engine) execSandboxExecTool(ctx context.Context, args map[string]any, rawArgs string, tctx *toolExecContext) (any, error) {
	req := SandboxExecRequest{
		Script:       strArg(args, "script"),
		Command:      strArg(args, "command"),
		Lang:         strArg(args, "lang"),
		Workdir:      strArg(args, "workdir"),
		TimeoutSec:   intArg(args, "timeout_sec", 30),
		Internet:     boolArg(args, "internet"),
		AllowedHosts: strSliceArg(args, "allowed_hosts"),
		Reason:       strArg(args, "reason"),
	}
	if req.Script == "" && req.Command == "" {
		return nil, newErr("either script or command is required", http.StatusBadRequest)
	}
	if !tctx.BypassHITL {
		if req.Internet {
			hosts := "public internet (ports 80/443)"
			if len(req.AllowedHosts) > 0 {
				hosts = strings.Join(req.AllowedHosts, ", ")
			}
			summary := fmt.Sprintf("Run sandbox command with internet access (%s)", hosts)
			if req.Reason != "" {
				summary += ": " + truncateText(req.Reason, 100)
			}
			return nil, &approvalRequiredError{
				ToolName:  "sandbox_exec",
				Summary:   summary,
				RiskLevel: "sandbox_internet",
				ArgsJSON:  cmp.Or(rawArgs, "{}"),
			}
		}
		if tctx.Source != "chat" && tctx.Source != "approval" {
			return nil, &approvalRequiredError{
				ToolName:  "sandbox_exec",
				Summary:   fmt.Sprintf("Run sandbox command during %s run", cmp.Or(tctx.Source, "background")),
				RiskLevel: "sandbox_exec",
				ArgsJSON:  cmp.Or(rawArgs, "{}"),
			}
		}
	}
	return e.sandbox.Exec(ctx, tctx.Config.Sandbox, tctx.ChannelID, req)
}

func (e *Engine) execSandboxVaultBridgeTool(ctx context.Context, args map[string]any, tctx *toolExecContext) (any, error) {
	action := strings.ToLower(strArg(args, "action"))
	vaultName := strArg(args, "vault")
	vaultPath := strArg(args, "vault_path")
	sandboxPath := strArg(args, "sandbox_path")
	if vaultPath == "" {
		return nil, newErr("vault_path is required", http.StatusBadRequest)
	}
	switch action {
	case "import":
		v, mount, err := e.ResolveVault(vaultName)
		if err != nil {
			return nil, err
		}
		data, err := v.ReadFileBytes(vaultPath, e.maxSandboxFileBytes())
		if err != nil {
			return nil, err
		}
		if sandboxPath == "" {
			sandboxPath = "in/" + SanitizeFileName(filepath.Base(vaultPath))
		}
		absDst, cleanRel, err := e.sandbox.ResolveWorkPath(tctx.ChannelID, sandboxPath)
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(absDst), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(absDst, data, 0o644); err != nil {
			return nil, err
		}
		return map[string]any{
			"status":       "imported",
			"vault":        mount.Name,
			"vault_path":   vaultPath,
			"sandbox_path": cleanRel,
			"size_bytes":   len(data),
		}, nil
	case "export":
		if sandboxPath == "" {
			return nil, newErr("sandbox_path is required for export", http.StatusBadRequest)
		}
		content, _, truncated, err := e.sandbox.ReadTextFile(tctx.ChannelID, sandboxPath, 256*1024)
		if err != nil {
			return nil, err
		}
		if truncated {
			return nil, newErr("sandbox file exceeds 256 KB text export limit; use sandbox_publish_file for binary/large files", http.StatusRequestEntityTooLarge)
		}
		writeArgs := map[string]any{
			"vault":   vaultName,
			"path":    vaultPath,
			"content": content,
		}
		rawWrite, _ := json.Marshal(writeArgs)
		return e.execVaultWriteTool(ctx, "vault_write", writeArgs, string(rawWrite), tctx)
	default:
		return nil, newErr("action must be \"import\" or \"export\"", http.StatusBadRequest)
	}
}

func (e *Engine) execSandboxInstallPackagesTool(ctx context.Context, args map[string]any, rawArgs string, tctx *toolExecContext) (any, error) {
	if tctx.Source != "chat" && tctx.Source != "approval" && !tctx.BypassHITL {
		return nil, newErr("sandbox_install_packages is only allowed during direct user chat", http.StatusForbidden)
	}
	aptPkgs := normalizeStringList(strSliceArg(args, "apt_packages"))
	pipPkgs := normalizeStringList(strSliceArg(args, "pip_packages"))
	if len(aptPkgs) == 0 && len(pipPkgs) == 0 {
		return nil, newErr("at least one apt_packages or pip_packages entry is required", http.StatusBadRequest)
	}
	for _, pkg := range aptPkgs {
		if err := ValidateAptPackageName(pkg); err != nil {
			return nil, err
		}
	}
	for _, spec := range pipPkgs {
		if err := ValidatePipPackageSpec(spec); err != nil {
			return nil, err
		}
	}
	if !tctx.BypassHITL {
		var parts []string
		if len(aptPkgs) > 0 {
			parts = append(parts, "apt: "+strings.Join(aptPkgs, ", "))
		}
		if len(pipPkgs) > 0 {
			parts = append(parts, "pip: "+strings.Join(pipPkgs, ", "))
		}
		summary := "Install sandbox packages (" + strings.Join(parts, "; ") + ")"
		if reason := strArg(args, "reason"); reason != "" {
			summary += " — " + truncateText(reason, 100)
		}
		return nil, &approvalRequiredError{
			ToolName:  "sandbox_install_packages",
			Summary:   summary,
			RiskLevel: "sandbox_packages",
			ArgsJSON:  cmp.Or(rawArgs, "{}"),
		}
	}

	e.configWriteMu.Lock()
	cur := e.Config()
	cur.Sandbox.ExtraAptPackages = normalizeStringList(append(cur.Sandbox.ExtraAptPackages, aptPkgs...))
	cur.Sandbox.ExtraPipPackages = normalizeStringList(append(cur.Sandbox.ExtraPipPackages, pipPkgs...))
	view, err := e.updateConfigLocked(cur, false)
	e.configWriteMu.Unlock()
	if err != nil {
		return nil, err
	}
	tctx.Config = e.Config()
	buildCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	res, err := e.sandbox.BuildImages(buildCtx, tctx.Config.Sandbox, false)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"status":             "installed",
		"image":              res.Image,
		"extra_apt_packages": view.Sandbox.ExtraAptPackages,
		"extra_pip_packages": view.Sandbox.ExtraPipPackages,
	}, nil
}
