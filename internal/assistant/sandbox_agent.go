package assistant

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const launchdMismatchNeedle = "not registered with launchd"

func decodeAgentJSON(r *http.Request, dst any) error {
	defer func() { _ = r.Body.Close() }()
	return json.NewDecoder(io.LimitReader(r.Body, 1024*1024)).Decode(dst)
}

func writeAgentJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeAgentErr(w http.ResponseWriter, status int, err error) {
	if ae, ok := errors.AsType[*AssistantError](err); ok {
		status = ae.HTTPStatus()
	}
	writeAgentJSON(w, status, map[string]string{"error": err.Error()})
}

func buildSandboxAgentHandler(runner SandboxRunner) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		writeAgentJSON(w, http.StatusOK, map[string]any{"ok": true, "bin": runner.FindBinary()})
	})
	mux.HandleFunc("POST /system-status", func(w http.ResponseWriter, r *http.Request) {
		if err := runner.SystemStatus(r.Context()); err != nil {
			writeAgentErr(w, http.StatusServiceUnavailable, err)
			return
		}
		writeAgentJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /network-exists", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name string `json:"name"`
		}
		if err := decodeAgentJSON(r, &req); err != nil {
			writeAgentErr(w, http.StatusBadRequest, err)
			return
		}
		writeAgentJSON(w, http.StatusOK, map[string]bool{"exists": runner.NetworkExists(r.Context(), req.Name)})
	})
	mux.HandleFunc("POST /ensure-network", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name   string `json:"name"`
			Subnet string `json:"subnet"`
		}
		if err := decodeAgentJSON(r, &req); err != nil {
			writeAgentErr(w, http.StatusBadRequest, err)
			return
		}
		gw, err := runner.EnsureNetwork(r.Context(), req.Name, req.Subnet)
		if err != nil {
			writeAgentErr(w, http.StatusInternalServerError, err)
			return
		}
		writeAgentJSON(w, http.StatusOK, map[string]string{"gateway_ip": gw})
	})
	mux.HandleFunc("POST /image-exists", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Image string `json:"image"`
		}
		if err := decodeAgentJSON(r, &req); err != nil {
			writeAgentErr(w, http.StatusBadRequest, err)
			return
		}
		writeAgentJSON(w, http.StatusOK, map[string]bool{"exists": runner.ImageExists(r.Context(), req.Image)})
	})
	mux.HandleFunc("POST /build-image", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Tag               string `json:"tag"`
			ContainerfilePath string `json:"containerfile_path"`
			ContextDir        string `json:"context_dir"`
			CPUs              int    `json:"cpus"`
			MemoryMB          int    `json:"memory_mb"`
		}
		if err := decodeAgentJSON(r, &req); err != nil {
			writeAgentErr(w, http.StatusBadRequest, err)
			return
		}
		out, err := runner.BuildImage(r.Context(), req.Tag, req.ContainerfilePath, req.ContextDir, req.CPUs, req.MemoryMB)
		if err != nil {
			writeAgentErr(w, http.StatusInternalServerError, err)
			return
		}
		writeAgentJSON(w, http.StatusOK, map[string]string{"output": out})
	})
	mux.HandleFunc("POST /start-container", func(w http.ResponseWriter, r *http.Request) {
		var req SandboxStartOpts
		if err := decodeAgentJSON(r, &req); err != nil {
			writeAgentErr(w, http.StatusBadRequest, err)
			return
		}
		if err := runner.StartContainer(r.Context(), req); err != nil {
			writeAgentErr(w, http.StatusInternalServerError, err)
			return
		}
		writeAgentJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /exec-container", func(w http.ResponseWriter, r *http.Request) {
		var req SandboxExecOpts
		if err := decodeAgentJSON(r, &req); err != nil {
			writeAgentErr(w, http.StatusBadRequest, err)
			return
		}
		res, err := runner.ExecContainer(r.Context(), req)
		if err != nil {
			writeAgentErr(w, http.StatusInternalServerError, err)
			return
		}
		writeAgentJSON(w, http.StatusOK, res)
	})
	mux.HandleFunc("POST /stop-remove-container", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name string `json:"name"`
		}
		if err := decodeAgentJSON(r, &req); err != nil {
			writeAgentErr(w, http.StatusBadRequest, err)
			return
		}
		if err := runner.StopRemoveContainer(r.Context(), req.Name); err != nil {
			writeAgentErr(w, http.StatusInternalServerError, err)
			return
		}
		writeAgentJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	return mux
}

// RunSandboxAgent serves the SandboxRunner API over a 0600 Unix domain socket in the user's gui/<uid> session.
func RunSandboxAgent(ctx context.Context, sockPath string, runner SandboxRunner) error {
	if strings.TrimSpace(sockPath) == "" {
		return errors.New("sandbox socket path is required")
	}
	if runner == nil {
		runner = NewAppleContainerRunner()
	}
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
		return err
	}
	_ = os.Remove(sockPath)
	ln, err := (&net.ListenConfig{}).Listen(ctx, "unix", sockPath)
	if err != nil {
		return fmt.Errorf("listen sandbox socket %s: %w", sockPath, err)
	}
	if err := os.Chmod(sockPath, 0o600); err != nil {
		_ = ln.Close()
		_ = os.Remove(sockPath)
		return err
	}
	srv := &http.Server{
		Handler:           buildSandboxAgentHandler(runner),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	defer func() { _ = os.Remove(sockPath) }()
	err = srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func newUnixSocketClient(sockPath string) *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialer.DialContext(ctx, "unix", sockPath)
			},
		},
	}
}

// PingSandboxAgent checks whether the gui/<uid> sandbox LaunchAgent socket is listening.
func PingSandboxAgent(ctx context.Context, sockPath string) error {
	cli := newUnixSocketClient(sockPath)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://sandbox.local/health", nil)
	if err != nil {
		return err
	}
	resp, err := cli.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("sandbox agent health returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// SocketSandboxRunner delegates SandboxRunner calls over a gui/<uid> Unix socket when present, falling back to direct.
type SocketSandboxRunner struct {
	sockPath string
	direct   SandboxRunner
	client   *http.Client
}

// NewSocketSandboxRunner creates a SandboxRunner that bridges through sockPath when available.
func NewSocketSandboxRunner(sockPath string, direct SandboxRunner) *SocketSandboxRunner {
	if direct == nil {
		direct = NewAppleContainerRunner()
	}
	return &SocketSandboxRunner{
		sockPath: sockPath,
		direct:   direct,
		client:   newUnixSocketClient(sockPath),
	}
}

func (r *SocketSandboxRunner) hasSocket() bool {
	if r.sockPath == "" {
		return false
	}
	fi, err := os.Stat(r.sockPath)
	return err == nil && (fi.Mode()&os.ModeSocket) != 0
}

func (r *SocketSandboxRunner) call(ctx context.Context, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://sandbox.local"+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("sandbox-agent socket (%s): %w", r.sockPath, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		var errBody struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &errBody) == nil && errBody.Error != "" {
			return newErr(errBody.Error, resp.StatusCode)
		}
		return newErr(fmt.Sprintf("sandbox-agent HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw))), resp.StatusCode)
	}
	if out != nil && len(raw) > 0 {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// FindBinary locates the Apple container binary (or socket bridge).
func (r *SocketSandboxRunner) FindBinary() string {
	if bin := r.direct.FindBinary(); bin != "" {
		return bin
	}
	if r.hasSocket() {
		return r.sockPath
	}
	return ""
}

// SystemStatus checks container system availability via the gui/<uid> agent socket when present.
func (r *SocketSandboxRunner) SystemStatus(ctx context.Context) error {
	if r.hasSocket() {
		return r.call(ctx, "/system-status", struct{}{}, nil)
	}
	err := r.direct.SystemStatus(ctx)
	if err != nil && strings.Contains(err.Error(), launchdMismatchNeedle) {
		return newErr(err.Error()+"; run `fleet assistant sandbox-install` on the lighthouse so fleet serve can reach your gui/<uid> container session", http.StatusServiceUnavailable)
	}
	return err
}

// NetworkExists checks whether network name exists.
func (r *SocketSandboxRunner) NetworkExists(ctx context.Context, name string) bool {
	if r.hasSocket() {
		var out struct {
			Exists bool `json:"exists"`
		}
		return r.call(ctx, "/network-exists", map[string]string{"name": name}, &out) == nil && out.Exists
	}
	return r.direct.NetworkExists(ctx, name)
}

// EnsureNetwork inspects or creates an internal container network.
func (r *SocketSandboxRunner) EnsureNetwork(ctx context.Context, name, subnet string) (string, error) {
	if r.hasSocket() {
		var out struct {
			GatewayIP string `json:"gateway_ip"`
		}
		if err := r.call(ctx, "/ensure-network", map[string]string{"name": name, "subnet": subnet}, &out); err != nil {
			return "", err
		}
		return out.GatewayIP, nil
	}
	return r.direct.EnsureNetwork(ctx, name, subnet)
}

// ImageExists checks whether image is present locally.
func (r *SocketSandboxRunner) ImageExists(ctx context.Context, image string) bool {
	if r.hasSocket() {
		var out struct {
			Exists bool `json:"exists"`
		}
		return r.call(ctx, "/image-exists", map[string]string{"image": image}, &out) == nil && out.Exists
	}
	return r.direct.ImageExists(ctx, image)
}

// BuildImage builds an OCI image via the gui/<uid> agent socket when present.
func (r *SocketSandboxRunner) BuildImage(ctx context.Context, tag, containerfilePath, contextDir string, cpus, memoryMB int) (string, error) {
	if r.hasSocket() {
		var out struct {
			Output string `json:"output"`
		}
		if err := r.call(ctx, "/build-image", map[string]any{
			"tag":                tag,
			"containerfile_path": containerfilePath,
			"context_dir":        contextDir,
			"cpus":               cpus,
			"memory_mb":          memoryMB,
		}, &out); err != nil {
			return "", err
		}
		return out.Output, nil
	}
	return r.direct.BuildImage(ctx, tag, containerfilePath, contextDir, cpus, memoryMB)
}

// StartContainer starts a sandbox container via the gui/<uid> agent socket when present.
func (r *SocketSandboxRunner) StartContainer(ctx context.Context, opts SandboxStartOpts) error {
	if r.hasSocket() {
		return r.call(ctx, "/start-container", opts, nil)
	}
	return r.direct.StartContainer(ctx, opts)
}

// ExecContainer executes a command inside a running sandbox container.
func (r *SocketSandboxRunner) ExecContainer(ctx context.Context, opts SandboxExecOpts) (SandboxRawResult, error) {
	if r.hasSocket() {
		var res SandboxRawResult
		if err := r.call(ctx, "/exec-container", opts, &res); err != nil {
			return SandboxRawResult{}, err
		}
		return res, nil
	}
	return r.direct.ExecContainer(ctx, opts)
}

// StopRemoveContainer stops and removes a sandbox container.
func (r *SocketSandboxRunner) StopRemoveContainer(ctx context.Context, name string) error {
	if r.hasSocket() {
		return r.call(ctx, "/stop-remove-container", map[string]string{"name": name}, nil)
	}
	return r.direct.StopRemoveContainer(ctx, name)
}
