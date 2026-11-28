package assistant

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	sandboxOutputCapBytes    = 16 * 1024
	defaultSandboxUpstreamOS = "ghcr.io/astral-sh/uv:python3.13-bookworm-slim"

	sandboxBaseSteps = `
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get -o Acquire::ForceIPv4=true update && apt-get install -y -o Acquire::ForceIPv4=true -o Acquire::Retries=3 --no-install-recommends \
    ca-certificates \
    curl \
    file \
    fonts-dejavu-core \
    jq \
    libffi8 \
    libgdk-pixbuf-2.0-0 \
    libpango-1.0-0 \
    libpangocairo-1.0-0 \
    pandoc \
    poppler-utils \
    ripgrep \
    shared-mime-info \
    sqlite3 \
    tesseract-ocr \
    unzip \
    zip \
    && rm -rf /var/lib/apt/lists/*

RUN UV_COMPILE_BYTECODE=0 UV_LINK_MODE=copy uv pip install --system --no-cache \
    pypdf \
    pdfplumber \
    pymupdf \
    weasyprint \
    reportlab \
    python-docx \
    openpyxl \
    python-pptx \
    pandas \
    duckdb \
    matplotlib \
    pillow \
    tabulate
`

	sandboxFooter = `
RUN groupadd -g 1000 sandbox && useradd -m -u 1000 -g 1000 -s /bin/sh sandbox \
    && mkdir -p /work/in /work/out && chown -R sandbox:sandbox /work

USER sandbox
WORKDIR /work
ENV HOME=/tmp MPLCONFIGDIR=/tmp/matplotlib
`

	// DefaultSandboxContainerfile defines the pre-baked document, data, OCR, and chart toolkit for the assistant sandbox.
	DefaultSandboxContainerfile = "FROM " + defaultSandboxUpstreamOS + "\n" + sandboxBaseSteps + sandboxFooter
)

// RenderCustomContainerfile builds a single-stage Containerfile directly from an upstream base image
// so BuildKit caches base layers without triggering local FROM oci-layout HTTP 500 lookups.
func RenderCustomContainerfile(baseImage string, extraApt, extraPip []string) (string, error) {
	base := strings.TrimSpace(baseImage)
	if base == "" || base == DefaultSandboxBaseImage {
		base = defaultSandboxUpstreamOS
	}
	if strings.ContainsAny(base, "\r\n\t ") {
		return "", newErr(fmt.Sprintf("invalid base image %q", base), http.StatusBadRequest)
	}
	var aptClean, pipClean []string
	for _, raw := range normalizeStringList(extraApt) {
		if err := ValidateAptPackageName(raw); err != nil {
			return "", err
		}
		aptClean = append(aptClean, raw)
	}
	for _, raw := range normalizeStringList(extraPip) {
		if err := ValidatePipPackageSpec(raw); err != nil {
			return "", err
		}
		pipClean = append(pipClean, raw)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "FROM %s\n", base)
	b.WriteString(sandboxBaseSteps)
	if len(aptClean) > 0 {
		fmt.Fprintf(&b, "RUN apt-get -o Acquire::ForceIPv4=true update && apt-get install -y -o Acquire::ForceIPv4=true -o Acquire::Retries=3 --no-install-recommends %s && rm -rf /var/lib/apt/lists/*\n", strings.Join(aptClean, " "))
	}
	if len(pipClean) > 0 {
		quoted := make([]string, len(pipClean))
		for i, p := range pipClean {
			quoted[i] = strconv.Quote(p)
		}
		fmt.Fprintf(&b, "RUN UV_COMPILE_BYTECODE=0 UV_LINK_MODE=copy uv pip install --system --no-cache %s\n", strings.Join(quoted, " "))
	}
	b.WriteString(sandboxFooter)
	return b.String(), nil
}

// SandboxStartOpts describes container launch arguments passed to a SandboxRunner.
type SandboxStartOpts struct {
	Name     string
	Image    string
	Network  string
	CPUs     int
	MemoryMB int
	WorkDir  string
}

// SandboxExecOpts describes a single command execution inside a running sandbox container.
type SandboxExecOpts struct {
	Name    string
	WorkDir string
	Command []string
	Env     []string
}

// SandboxRawResult holds exit code and capped stdout/stderr from container exec.
type SandboxRawResult struct {
	ExitCode  int
	Stdout    string
	Stderr    string
	Truncated bool
}

// SandboxExecRequest describes an assistant-requested script or shell execution.
type SandboxExecRequest struct {
	Script       string
	Command      string
	Lang         string
	Workdir      string
	TimeoutSec   int
	Internet     bool
	AllowedHosts []string
	Reason       string
}

// SandboxExecResult reports execution output, new files under /work/out, and contacted hosts.
type SandboxExecResult struct {
	ExitCode       int      `json:"exit_code"`
	Stdout         string   `json:"stdout"`
	Stderr         string   `json:"stderr"`
	Truncated      bool     `json:"truncated,omitempty"`
	NewFiles       []string `json:"new_files,omitempty"`
	ContactedHosts []string `json:"contacted_hosts,omitempty"`
}

// SandboxFileInfo describes one file inside /work.
type SandboxFileInfo struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
	IsDir     bool   `json:"is_dir,omitempty"`
	MimeType  string `json:"mime_type,omitempty"`
}

// SandboxBuildResult reports built sandbox images and installed custom packages.
type SandboxBuildResult struct {
	BaseImage        string   `json:"base_image"`
	Image            string   `json:"image"`
	Network          string   `json:"network"`
	BaseRebuilt      bool     `json:"base_rebuilt"`
	ExtraAptPackages []string `json:"extra_apt_packages,omitempty"`
	ExtraPipPackages []string `json:"extra_pip_packages,omitempty"`
}

// SandboxRunner abstracts container CLI operations so SandboxManager is unit-testable without a VM host.
type SandboxRunner interface {
	FindBinary() string
	SystemStatus(ctx context.Context) error
	NetworkExists(ctx context.Context, name string) bool
	EnsureNetwork(ctx context.Context, name, subnet string) (string, error)
	ImageExists(ctx context.Context, image string) bool
	BuildImage(ctx context.Context, tag, containerfilePath, contextDir string, cpus, memoryMB int, noCache bool) (string, error)
	StartContainer(ctx context.Context, opts SandboxStartOpts) error
	ExecContainer(ctx context.Context, opts SandboxExecOpts) (SandboxRawResult, error)
	StopRemoveContainer(ctx context.Context, name string) error
}

// AppleContainerRunner executes Apple's `container` CLI (macOS 26+).
type AppleContainerRunner struct {
	bin string
}

// NewAppleContainerRunner constructs a runner backed by Apple's `container` CLI.
func NewAppleContainerRunner() *AppleContainerRunner {
	return &AppleContainerRunner{}
}

func (r *AppleContainerRunner) binary() string {
	if r.bin != "" {
		return r.bin
	}
	return r.FindBinary()
}

// FindBinary locates Apple's `container` CLI in PATH or standard Homebrew locations.
func (r *AppleContainerRunner) FindBinary() string {
	if p, err := exec.LookPath("container"); err == nil {
		r.bin = p
		return p
	}
	for _, cand := range []string{"/opt/homebrew/bin/container", "/usr/local/bin/container"} {
		if fi, err := os.Stat(cand); err == nil && !fi.IsDir() {
			r.bin = cand
			return cand
		}
	}
	return ""
}

// SystemStatus verifies that `container system status` succeeds.
func (r *AppleContainerRunner) SystemStatus(ctx context.Context) error {
	bin := r.binary()
	if bin == "" {
		return newErr("Apple `container` CLI not found (install with `brew install container`)", http.StatusServiceUnavailable)
	}
	out, err := exec.CommandContext(ctx, bin, "system", "status").CombinedOutput()
	if err != nil {
		return newErr(fmt.Sprintf("container system is not running (%s): %s", err, strings.TrimSpace(string(out))), http.StatusServiceUnavailable)
	}
	return nil
}

// NetworkExists checks whether a named container network exists locally.
func (r *AppleContainerRunner) NetworkExists(ctx context.Context, name string) bool {
	bin := r.binary()
	if bin == "" {
		return false
	}
	return exec.CommandContext(ctx, bin, "network", "inspect", name).Run() == nil
}

// EnsureNetwork inspects or creates an internal host-only vmnet network and returns its gateway IP.
func (r *AppleContainerRunner) EnsureNetwork(ctx context.Context, name, subnet string) (string, error) {
	bin := r.binary()
	if bin == "" {
		return "", newErr("Apple `container` CLI not found", http.StatusServiceUnavailable)
	}
	if out, err := exec.CommandContext(ctx, bin, "network", "inspect", name).CombinedOutput(); err == nil {
		return parseNetworkGateway(out, subnet), nil
	}
	out, err := exec.CommandContext(ctx, bin, "network", "create", "--internal", "--subnet", subnet, name).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("create internal container network %q: %w (%s)", name, err, strings.TrimSpace(string(out)))
	}
	inspOut, _ := exec.CommandContext(ctx, bin, "network", "inspect", name).CombinedOutput()
	return parseNetworkGateway(inspOut, subnet), nil
}

func parseNetworkGateway(raw []byte, fallbackSubnet string) string {
	if len(raw) > 0 {
		var items []map[string]any
		if json.Unmarshal(raw, &items) == nil && len(items) > 0 {
			if gw := findGatewayString(items[0]); gw != "" {
				return gw
			}
		}
		var obj map[string]any
		if json.Unmarshal(raw, &obj) == nil {
			if gw := findGatewayString(obj); gw != "" {
				return gw
			}
		}
	}
	if pfx, err := netip.ParsePrefix(fallbackSubnet); err == nil && pfx.Addr().Is4() {
		return pfx.Addr().Next().String()
	}
	return "192.168.127.1"
}

func findGatewayString(m map[string]any) string {
	for k, v := range m {
		if strings.Contains(strings.ToLower(k), "gateway") {
			if s, ok := v.(string); ok {
				if addr, err := netip.ParseAddr(strings.TrimSpace(s)); err == nil && addr.IsValid() {
					return addr.String()
				}
			}
		}
		if sub, ok := v.(map[string]any); ok {
			if gw := findGatewayString(sub); gw != "" {
				return gw
			}
		}
	}
	return ""
}

// ImageExists checks whether image tag is present locally in `container image ls`.
func (r *AppleContainerRunner) ImageExists(ctx context.Context, image string) bool {
	bin := r.binary()
	if bin == "" {
		return false
	}
	return exec.CommandContext(ctx, bin, "image", "inspect", image).Run() == nil
}

// BuildImage builds an OCI image tag from containerfilePath using `container build` while streaming progress.
func (r *AppleContainerRunner) BuildImage(ctx context.Context, tag, containerfilePath, contextDir string, cpus, memoryMB int, noCache bool) (string, error) {
	bin := r.binary()
	if bin == "" {
		return "", newErr("Apple `container` CLI not found", http.StatusServiceUnavailable)
	}
	args := []string{
		"build",
		"-t", tag,
		"-f", containerfilePath,
		"-c", strconv.Itoa(max(cpus, 4)),
		"-m", fmt.Sprintf("%dM", max(memoryMB, 4096)),
		"--progress", "plain",
	}
	if noCache {
		args = append(args, "--no-cache")
	}
	args = append(args, contextDir)

	cmd := exec.CommandContext(ctx, bin, args...)
	var outBuf bytes.Buffer
	mw := io.MultiWriter(os.Stderr, &outBuf)
	cmd.Stdout = mw
	cmd.Stderr = mw
	if err := cmd.Run(); err != nil {
		out := outBuf.String()
		return out, fmt.Errorf("container build %s failed: %w\n%s", tag, err, truncateText(out, 1500))
	}
	return outBuf.String(), nil
}

// StartContainer starts a hardened, offline micro-VM container with bounded resources.
func (r *AppleContainerRunner) StartContainer(ctx context.Context, opts SandboxStartOpts) error {
	bin := r.binary()
	if bin == "" {
		return newErr("Apple `container` CLI not found", http.StatusServiceUnavailable)
	}
	_ = exec.CommandContext(ctx, bin, "rm", "-f", opts.Name).Run()
	args := []string{
		"run", "-d",
		"--name", opts.Name,
		"--network", opts.Network,
		"--no-dns",
		"--cpus", strconv.Itoa(max(opts.CPUs, 1)),
		"--memory", fmt.Sprintf("%dM", max(opts.MemoryMB, 256)),
		"--read-only",
		"--cap-drop", "ALL",
		"--user", "1000:1000",
		"--tmpfs", "/tmp",
		"-v", opts.WorkDir + ":/work",
		"-w", "/work",
		opts.Image,
		"sleep", "infinity",
	}
	out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("container run %s failed: %w (%s)", opts.Name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

type cappedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if c.buf.Len() < c.limit {
		avail := c.limit - c.buf.Len()
		if len(p) > avail {
			c.buf.Write(p[:avail])
			c.truncated = true
		} else {
			c.buf.Write(p)
		}
	} else if len(p) > 0 {
		c.truncated = true
	}
	return len(p), nil
}

// ExecContainer executes one command inside an already-running container.
func (r *AppleContainerRunner) ExecContainer(ctx context.Context, opts SandboxExecOpts) (SandboxRawResult, error) {
	bin := r.binary()
	if bin == "" {
		return SandboxRawResult{}, newErr("Apple `container` CLI not found", http.StatusServiceUnavailable)
	}
	args := []string{"exec"}
	for _, kv := range opts.Env {
		args = append(args, "-e", kv)
	}
	if opts.WorkDir != "" {
		args = append(args, "-w", opts.WorkDir)
	}
	args = append(args, opts.Name)
	args = append(args, opts.Command...)

	cmd := exec.CommandContext(ctx, bin, args...)
	stdout := &cappedBuffer{limit: sandboxOutputCapBytes}
	stderr := &cappedBuffer{limit: sandboxOutputCapBytes}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	err := cmd.Run()
	exitCode := 0
	if err != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return SandboxRawResult{}, err
		}
	}
	return SandboxRawResult{
		ExitCode:  exitCode,
		Stdout:    stdout.buf.String(),
		Stderr:    stderr.buf.String(),
		Truncated: stdout.truncated || stderr.truncated,
	}, nil
}

// StopRemoveContainer stops and removes a sandbox container.
func (r *AppleContainerRunner) StopRemoveContainer(ctx context.Context, name string) error {
	bin := r.binary()
	if bin == "" {
		return nil
	}
	_ = exec.CommandContext(ctx, bin, "stop", name).Run()
	_ = exec.CommandContext(ctx, bin, "rm", "-f", name).Run()
	return nil
}

type activeSandbox struct {
	channelID     string
	containerName string
	image         string
	network       string
	gatewayIP     string
	lastUsed      time.Time
}

// SandboxManager manages scale-to-zero per-channel sandbox micro-VMs and workspace staging.
type SandboxManager struct {
	mu      sync.Mutex
	rootDir string
	runner  SandboxRunner
	active  *activeSandbox
}

// NewSandboxManager creates a SandboxManager rooted at rootDir using runner (or AppleContainerRunner by default).
func NewSandboxManager(rootDir string, runner SandboxRunner) *SandboxManager {
	if runner == nil {
		runner = NewAppleContainerRunner()
	}
	return &SandboxManager{rootDir: rootDir, runner: runner}
}

func (m *SandboxManager) setRunner(r SandboxRunner) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r != nil {
		m.runner = r
	}
}

// WorkDir ensures and returns the host /work directory for channelID.
func (m *SandboxManager) WorkDir(channelID string) (string, error) {
	safeCh := SanitizeFileName(cmp.Or(strings.TrimSpace(channelID), "general"))
	workDir := filepath.Join(m.rootDir, safeCh, "work")
	for _, sub := range []string{"in", "out"} {
		if err := os.MkdirAll(filepath.Join(workDir, sub), 0o755); err != nil {
			return "", err
		}
	}
	return workDir, nil
}

// ResolveWorkPath resolves a relative or /work-prefixed path safely inside the channel sandbox workspace.
func (m *SandboxManager) ResolveWorkPath(channelID, rawPath string) (abs, cleanRel string, err error) {
	workDir, err := m.WorkDir(channelID)
	if err != nil {
		return "", "", err
	}
	if strings.ContainsRune(rawPath, 0) {
		return "", "", newErr("invalid sandbox path", http.StatusBadRequest)
	}
	trimmed := strings.TrimSpace(rawPath)
	trimmed = strings.TrimPrefix(trimmed, "/work/")
	if trimmed == "/work" || trimmed == "" || trimmed == "." {
		trimmed = "."
	}
	if filepath.IsAbs(trimmed) || strings.HasPrefix(trimmed, `\`) {
		return "", "", newErr("sandbox path must stay inside /work", http.StatusBadRequest)
	}
	clean := filepath.Clean(trimmed)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", "", newErr("sandbox path traversal rejected", http.StatusBadRequest)
	}
	full := filepath.Join(workDir, clean)
	rootReal, err := filepath.EvalSymlinks(workDir)
	if err != nil {
		rootReal = workDir
	}
	for _, cand := range []string{full, filepath.Dir(full)} {
		if resolved, rErr := filepath.EvalSymlinks(cand); rErr == nil {
			relCheck, relErr := filepath.Rel(rootReal, resolved)
			if relErr != nil || relCheck == ".." || strings.HasPrefix(relCheck, ".."+string(filepath.Separator)) {
				return "", "", newErr("symlink escapes sandbox /work", http.StatusForbidden)
			}
		}
	}
	return full, filepath.ToSlash(clean), nil
}

// ListFiles lists files inside /work (or a relative subdirectory such as in or out).
func (m *SandboxManager) ListFiles(channelID, relDir string) ([]SandboxFileInfo, error) {
	absDir, cleanRel, err := m.ResolveWorkPath(channelID, cmp.Or(strings.TrimSpace(relDir), "."))
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(absDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []SandboxFileInfo{}, nil
		}
		return nil, err
	}
	out := make([]SandboxFileInfo, 0, len(entries))
	for _, ent := range entries {
		if strings.HasPrefix(ent.Name(), ".fleet_run_") {
			continue
		}
		info, iErr := ent.Info()
		if iErr != nil {
			continue
		}
		rel := ent.Name()
		if cleanRel != "." {
			rel = cleanRel + "/" + ent.Name()
		}
		item := SandboxFileInfo{
			Path:      rel,
			SizeBytes: info.Size(),
			IsDir:     ent.IsDir(),
		}
		if !ent.IsDir() {
			item.MimeType = detectFileMIME(ent.Name(), "", readHeadSample(filepath.Join(absDir, ent.Name()), 256))
		}
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// ReadTextFile reads UTF-8 text from a file inside /work up to maxBytes.
func (m *SandboxManager) ReadTextFile(channelID, relPath string, maxBytes int) (string, int64, bool, error) {
	abs, _, err := m.ResolveWorkPath(channelID, relPath)
	if err != nil {
		return "", 0, false, err
	}
	if maxBytes <= 0 || maxBytes > 256*1024 {
		maxBytes = 64 * 1024
	}
	info, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return "", 0, false, newErr(fmt.Sprintf("sandbox file %q not found", relPath), http.StatusNotFound)
		}
		return "", 0, false, err
	}
	if info.IsDir() {
		return "", 0, false, newErr(fmt.Sprintf("%q is a directory", relPath), http.StatusBadRequest)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return "", 0, false, err
	}
	truncated := len(data) > maxBytes
	if truncated {
		data = data[:maxBytes]
	}
	return string(data), info.Size(), truncated, nil
}

// WriteTextFile writes UTF-8 content inside /work.
func (m *SandboxManager) WriteTextFile(channelID, relPath, content string) (string, error) {
	abs, cleanRel, err := m.ResolveWorkPath(channelID, relPath)
	if err != nil {
		return "", err
	}
	if cleanRel == "." {
		return "", newErr("file path is required", http.StatusBadRequest)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		return "", err
	}
	return cleanRel, nil
}

func (m *SandboxManager) ensureActiveLocked(ctx context.Context, cfg SandboxConfig, channelID string) (*activeSandbox, error) {
	norm := normalizeSandboxConfig(cfg)
	if m.active != nil && (m.active.channelID != channelID || m.active.image != norm.Image || m.active.network != norm.Network) {
		_ = m.runner.StopRemoveContainer(ctx, m.active.containerName)
		m.active = nil
	}
	if m.active != nil {
		m.active.lastUsed = time.Now()
		return m.active, nil
	}
	if err := m.runner.SystemStatus(ctx); err != nil {
		return nil, err
	}
	if !m.runner.ImageExists(ctx, norm.Image) {
		return nil, newErr(fmt.Sprintf("sandbox image %q is not built yet — run `fleet assistant sandbox-build` or click Build Sandbox Image in Settings", norm.Image), http.StatusFailedDependency)
	}
	gw, err := m.runner.EnsureNetwork(ctx, norm.Network, DefaultSandboxSubnet)
	if err != nil {
		return nil, err
	}
	workDir, err := m.WorkDir(channelID)
	if err != nil {
		return nil, err
	}
	cName := "fleet-sb-" + SanitizeFileName(channelID)
	if err := m.runner.StartContainer(ctx, SandboxStartOpts{
		Name:     cName,
		Image:    norm.Image,
		Network:  norm.Network,
		CPUs:     norm.CPUs,
		MemoryMB: norm.MemoryMB,
		WorkDir:  workDir,
	}); err != nil {
		return nil, err
	}
	m.active = &activeSandbox{
		channelID:     channelID,
		containerName: cName,
		image:         norm.Image,
		network:       norm.Network,
		gatewayIP:     gw,
		lastUsed:      time.Now(),
	}
	return m.active, nil
}

func snapshotDirMtimes(dir string) map[string]int64 {
	out := make(map[string]int64)
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d == nil {
			return filepath.SkipDir
		}
		if d.IsDir() {
			return nil
		}
		if info, iErr := d.Info(); iErr == nil {
			if rel, rErr := filepath.Rel(dir, path); rErr == nil {
				out[filepath.ToSlash(rel)] = info.ModTime().UnixNano()
			}
		}
		return nil
	})
	return out
}

func randomScriptTag() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Exec runs an inline script or shell command inside the channel sandbox micro-VM.
func (m *SandboxManager) Exec(ctx context.Context, cfg SandboxConfig, channelID string, req SandboxExecRequest) (SandboxExecResult, error) {
	if !cfg.Enabled {
		return SandboxExecResult{}, newErr("sandbox is disabled; enable it in Assistant Settings", http.StatusBadRequest)
	}
	channelID = cmp.Or(strings.TrimSpace(channelID), "general")

	m.mu.Lock()
	act, err := m.ensureActiveLocked(ctx, cfg, channelID)
	if err != nil {
		m.mu.Unlock()
		return SandboxExecResult{}, err
	}
	containerName := act.containerName
	gatewayIP := cmp.Or(act.gatewayIP, "192.168.127.1")
	m.mu.Unlock()

	workDir, err := m.WorkDir(channelID)
	if err != nil {
		return SandboxExecResult{}, err
	}
	execWorkdir := "/work"
	if wd := strings.TrimSpace(req.Workdir); wd != "" {
		_, cleanWD, wdErr := m.ResolveWorkPath(channelID, wd)
		if wdErr != nil {
			return SandboxExecResult{}, wdErr
		}
		if cleanWD != "." {
			execWorkdir = "/work/" + cleanWD
		}
	}

	var envVars []string
	var proxy *EgressProxy
	if req.Internet {
		proxy, err = StartEgressProxy(net.JoinHostPort(gatewayIP, "0"), req.AllowedHosts, nil)
		if err != nil {
			return SandboxExecResult{}, fmt.Errorf("start sandbox egress proxy: %w", err)
		}
		defer func() { _ = proxy.Close() }()
		pURL := proxy.ProxyURL()
		envVars = append(envVars,
			"HTTP_PROXY="+pURL, "HTTPS_PROXY="+pURL,
			"http_proxy="+pURL, "https_proxy="+pURL,
			"NO_PROXY=localhost,127.0.0.1", "no_proxy=localhost,127.0.0.1",
		)
	}

	cmdArgs, cleanup, err := prepareSandboxCommand(workDir, req)
	if err != nil {
		return SandboxExecResult{}, err
	}
	if cleanup != nil {
		defer cleanup()
	}

	outDir := filepath.Join(workDir, "out")
	before := snapshotDirMtimes(outDir)

	timeoutSec := req.TimeoutSec
	if timeoutSec <= 0 {
		timeoutSec = 30
	}
	timeoutSec = min(timeoutSec, 120)
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()

	raw, execErr := m.runner.ExecContainer(runCtx, SandboxExecOpts{
		Name:    containerName,
		WorkDir: execWorkdir,
		Command: cmdArgs,
		Env:     envVars,
	})
	m.mu.Lock()
	if m.active != nil && m.active.containerName == containerName {
		m.active.lastUsed = time.Now()
	}
	m.mu.Unlock()
	if execErr != nil {
		return SandboxExecResult{}, execErr
	}

	after := snapshotDirMtimes(outDir)
	var newFiles []string
	for rel, mt := range after {
		if prev, ok := before[rel]; !ok || prev != mt {
			newFiles = append(newFiles, "out/"+rel)
		}
	}
	sort.Strings(newFiles)

	res := SandboxExecResult{
		ExitCode:  raw.ExitCode,
		Stdout:    raw.Stdout,
		Stderr:    raw.Stderr,
		Truncated: raw.Truncated,
		NewFiles:  newFiles,
	}
	if proxy != nil {
		res.ContactedHosts = proxy.ContactedHosts()
	}
	return res, nil
}

func prepareSandboxCommand(workDir string, req SandboxExecRequest) (cmdArgs []string, cleanup func(), err error) {
	if script := strings.TrimSpace(req.Script); script != "" {
		lang := strings.ToLower(cmp.Or(strings.TrimSpace(req.Lang), "python"))
		ext := ".py"
		interp := "python3"
		if lang == "sh" || lang == "bash" {
			ext = ".sh"
			interp = "sh"
		} else if lang != "python" && lang != "python3" {
			return nil, nil, newErr(fmt.Sprintf("unsupported script lang %q (use python or sh)", req.Lang), http.StatusBadRequest)
		}
		name := ".fleet_run_" + randomScriptTag() + ext
		hostScript := filepath.Join(workDir, name)
		if wErr := os.WriteFile(hostScript, []byte(req.Script), 0o644); wErr != nil {
			return nil, nil, wErr
		}
		return []string{interp, "/work/" + name}, func() { _ = os.Remove(hostScript) }, nil
	}
	command := strings.TrimSpace(req.Command)
	if command == "" {
		return nil, nil, newErr("either script or command is required", http.StatusBadRequest)
	}
	return []string{"sh", "-c", command}, nil, nil
}

// BuildImages ensures the offline network exists and builds a single-stage sandbox OCI image with BuildKit layer caching.
func (m *SandboxManager) BuildImages(ctx context.Context, cfg SandboxConfig, rebuildBase bool) (SandboxBuildResult, error) {
	norm := normalizeSandboxConfig(cfg)
	if err := validateSandboxPackages(norm); err != nil {
		return SandboxBuildResult{}, err
	}
	if err := m.runner.SystemStatus(ctx); err != nil {
		return SandboxBuildResult{}, err
	}
	if _, err := m.runner.EnsureNetwork(ctx, norm.Network, DefaultSandboxSubnet); err != nil {
		return SandboxBuildResult{}, err
	}
	if err := os.MkdirAll(m.rootDir, 0o755); err != nil {
		return SandboxBuildResult{}, err
	}
	tmpDir, err := os.MkdirTemp(m.rootDir, ".build-*")
	if err != nil {
		return SandboxBuildResult{}, err
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	cfBody, err := RenderCustomContainerfile("", norm.ExtraAptPackages, norm.ExtraPipPackages)
	if err != nil {
		return SandboxBuildResult{}, err
	}
	cfPath := filepath.Join(tmpDir, "Containerfile")
	if err := os.WriteFile(cfPath, []byte(cfBody), 0o644); err != nil {
		return SandboxBuildResult{}, err
	}
	if _, err := m.runner.BuildImage(ctx, norm.Image, cfPath, tmpDir, max(norm.CPUs, 4), max(norm.MemoryMB, 4096), rebuildBase); err != nil {
		return SandboxBuildResult{}, err
	}

	m.mu.Lock()
	if m.active != nil {
		_ = m.runner.StopRemoveContainer(ctx, m.active.containerName)
		m.active = nil
	}
	m.mu.Unlock()

	return SandboxBuildResult{
		BaseImage:        defaultSandboxUpstreamOS,
		Image:            norm.Image,
		Network:          norm.Network,
		BaseRebuilt:      rebuildBase,
		ExtraAptPackages: append([]string(nil), norm.ExtraAptPackages...),
		ExtraPipPackages: append([]string(nil), norm.ExtraPipPackages...),
	}, nil
}

// Reset stops/removes any active container for channelID and optionally clears /work/out and temp files.
func (m *SandboxManager) Reset(ctx context.Context, channelID string, clearOutput bool) error {
	channelID = cmp.Or(strings.TrimSpace(channelID), "general")
	cName := "fleet-sb-" + SanitizeFileName(channelID)

	m.mu.Lock()
	if m.active != nil && m.active.channelID == channelID {
		cName = m.active.containerName
		m.active = nil
	}
	m.mu.Unlock()

	_ = m.runner.StopRemoveContainer(ctx, cName)
	if clearOutput {
		workDir, err := m.WorkDir(channelID)
		if err != nil {
			return err
		}
		outDir := filepath.Join(workDir, "out")
		_ = os.RemoveAll(outDir)
		if err := os.MkdirAll(outDir, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// ReapIdle stops and removes an active sandbox container when idle for >= idleMinutes.
func (m *SandboxManager) ReapIdle(ctx context.Context, idleMinutes int) {
	if idleMinutes <= 0 {
		idleMinutes = DefaultSandboxIdleMinutes
	}
	m.mu.Lock()
	act := m.active
	if act == nil || time.Since(act.lastUsed) < time.Duration(idleMinutes)*time.Minute {
		m.mu.Unlock()
		return
	}
	m.active = nil
	m.mu.Unlock()
	_ = m.runner.StopRemoveContainer(ctx, act.containerName)
}

// Close stops and removes any active sandbox container during shutdown.
func (m *SandboxManager) Close(ctx context.Context) {
	m.mu.Lock()
	act := m.active
	m.active = nil
	m.mu.Unlock()
	if act != nil {
		_ = m.runner.StopRemoveContainer(ctx, act.containerName)
	}
}
