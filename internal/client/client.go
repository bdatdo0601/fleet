// Package client provides a persistent HTTP client for the lighthouse API.
package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/bdatdo0601/fleet/internal/version"
)

// ApiError represents an HTTP or transport failure talking to the lighthouse API.
type ApiError struct {
	Msg        string
	StatusCode int
}

func (e *ApiError) Error() string { return e.Msg }

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
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	url := strings.TrimRight(api, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
	if err != nil {
		return &ApiError{Msg: fmt.Sprintf("can't reach the lighthouse at %s: %v", api, err)}
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
		return &ApiError{Msg: fmt.Sprintf("can't reach the lighthouse at %s: %v", api, err)}
	}
	defer func() { _ = resp.Body.Close() }()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return &ApiError{Msg: fmt.Sprintf("can't reach the lighthouse at %s: %v", api, err)}
	}
	if resp.StatusCode >= 400 {
		return &ApiError{
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
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	url := strings.TrimRight(api, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", 0, &ApiError{Msg: fmt.Sprintf("can't reach the lighthouse at %s: %v", api, err)}
	}
	req.Header.Set("X-Fleet-Client", version.ClientID())
	resp, err := sharedClient.Do(req)
	if err != nil {
		return "", 0, &ApiError{Msg: fmt.Sprintf("can't reach the lighthouse at %s: %v", api, err)}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return "", 0, &ApiError{
			Msg:        parseErrorMsg(resp.StatusCode, resp.Status, b),
			StatusCode: resp.StatusCode,
		}
	}

	f, err := os.OpenFile(dstPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(f, h), resp.Body)
	closeErr := f.Close()
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
	return shaHex, n, nil
}
