// Package client provides a persistent HTTP client for the lighthouse API.
package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bdatdo0601/fleet/internal/paths"
	"github.com/bdatdo0601/fleet/internal/version"
)

const maxCallResponseBytes = 16 << 20 // 16 MiB

// NodeConfig mirrors /opt/homebrew/etc/nebula/fleet.json.
type NodeConfig struct {
	Name     string `json:"name"`
	API      string `json:"api"`
	Owner    string `json:"owner,omitempty"`
	Repo     string `json:"repo,omitempty"`
	StateDir string `json:"state_dir,omitempty"`
}

// LoadNode reads and decodes fleet.json from the Nebula config directory.
func LoadNode() (NodeConfig, error) {
	raw, err := os.ReadFile(paths.NodeFile())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return NodeConfig{}, &APIError{Msg: "this machine hasn't joined a fleet (fleet join NAME)"}
		}
		return NodeConfig{}, err
	}
	var nc NodeConfig
	if err := json.Unmarshal(raw, &nc); err != nil {
		return NodeConfig{}, err
	}
	return nc, nil
}

// ResolveAPI returns override if non-empty, otherwise reads api from fleet.json.
func ResolveAPI(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	n, err := LoadNode()
	if err != nil {
		return "", err
	}
	if n.API == "" {
		return "", &APIError{Msg: "invalid fleet.json (missing api)"}
	}
	return n.API, nil
}

// APIError represents an HTTP or transport failure talking to the lighthouse API.
type APIError struct {
	Msg        string
	StatusCode int
}

func (e *APIError) Error() string { return e.Msg }

var sharedTransport = &http.Transport{
	MaxIdleConns:        32,
	MaxIdleConnsPerHost: 16,
	IdleConnTimeout:     90 * time.Second,
}

var sharedClient = &http.Client{
	Transport: sharedTransport,
}

func parseErrorMsg(statusCode int, statusText string, body []byte) string {
	var parsed struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &parsed) == nil && parsed.Error != "" {
		return fmt.Sprintf("%s (HTTP %d)", parsed.Error, statusCode)
	}
	reason := strings.TrimPrefix(statusText, fmt.Sprintf("%d ", statusCode))
	if reason == "" {
		reason = http.StatusText(statusCode)
	}
	return fmt.Sprintf("%s (HTTP %d)", reason, statusCode)
}

// Call executes an HTTP JSON request against api+path and decodes the response into out (if non-nil).
func Call(api, method, path string, body any, timeout time.Duration, headers map[string]string, out any) error {
	return CallContext(context.Background(), api, method, path, body, timeout, headers, out)
}

// CallContext executes an HTTP JSON request with parent ctx against api+path and decodes the response into out (if non-nil).
func CallContext(ctx context.Context, api, method, path string, body any, timeout time.Duration, headers map[string]string, out any) error {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reqBody = bytes.NewReader(b)
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	url := strings.TrimRight(api, "/") + path
	req, err := http.NewRequestWithContext(reqCtx, method, url, reqBody)
	if err != nil {
		return &APIError{Msg: fmt.Sprintf("can't reach the lighthouse at %s: %v", api, err)}
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Fleet-Client", version.ClientID())
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := sharedClient.Do(req)
	if err != nil {
		return &APIError{Msg: fmt.Sprintf("can't reach the lighthouse at %s: %v", api, err)}
	}
	defer func() { _ = resp.Body.Close() }()

	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, maxCallResponseBytes+1))
	if err != nil {
		return &APIError{Msg: fmt.Sprintf("can't reach the lighthouse at %s: %v", api, err)}
	}
	if len(respBytes) > maxCallResponseBytes {
		return &APIError{
			Msg:        fmt.Sprintf("response from %s exceeds %d bytes", api, maxCallResponseBytes),
			StatusCode: resp.StatusCode,
		}
	}
	if resp.StatusCode >= 400 {
		return &APIError{
			Msg:        parseErrorMsg(resp.StatusCode, resp.Status, respBytes),
			StatusCode: resp.StatusCode,
		}
	}
	if out != nil && len(respBytes) > 0 {
		if err := json.Unmarshal(respBytes, out); err != nil {
			return err
		}
	}
	return nil
}

// Download streams a binary response from api+path to dstPath, computing and checking SHA-256.
func Download(api, path, dstPath string, timeout time.Duration) (string, int64, error) {
	return DownloadContext(context.Background(), api, path, dstPath, timeout)
}

// DownloadContext streams a binary response with parent ctx from api+path to dstPath atomically,
// verifying SHA-256 before replacing dstPath and cleaning up temporary files on error.
func DownloadContext(ctx context.Context, api, path, dstPath string, timeout time.Duration) (string, int64, error) {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	url := strings.TrimRight(api, "/") + path
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return "", 0, &APIError{Msg: fmt.Sprintf("can't reach the lighthouse at %s: %v", api, err)}
	}
	req.Header.Set("X-Fleet-Client", version.ClientID())
	resp, err := sharedClient.Do(req)
	if err != nil {
		return "", 0, &APIError{Msg: fmt.Sprintf("can't reach the lighthouse at %s: %v", api, err)}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return "", 0, &APIError{
			Msg:        parseErrorMsg(resp.StatusCode, resp.Status, b),
			StatusCode: resp.StatusCode,
		}
	}

	dir := filepath.Dir(dstPath)
	tmp, err := os.CreateTemp(dir, ".tmp-fleet-dl-*")
	if err != nil {
		return "", 0, err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o755); err != nil {
		_ = tmp.Close()
		return "", 0, err
	}
	h := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(tmp, h), resp.Body)
	closeErr := tmp.Close()
	if copyErr != nil {
		return "", 0, copyErr
	}
	if closeErr != nil {
		return "", 0, closeErr
	}
	shaHex := hex.EncodeToString(h.Sum(nil))
	if expected := strings.TrimSpace(resp.Header.Get("X-Fleet-SHA256")); expected != "" && !strings.EqualFold(expected, shaHex) {
		return "", 0, fmt.Errorf("downloaded binary SHA-256 mismatch: expected %s, got %s", expected, shaHex)
	}
	if err := os.Rename(tmpName, dstPath); err != nil {
		return "", 0, err
	}
	cleanup = false
	return shaHex, n, nil
}
