package client

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCallContextErrorParsingAndSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/json-err":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"missing field"}`))
		case "/plain-err":
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("upstream down"))
		case "/ok":
			_, _ = w.Write([]byte(`{"ok":true,"n":42}`))
		}
	}))
	defer srv.Close()

	err := CallContext(t.Context(), srv.URL, http.MethodGet, "/json-err", nil, 2*time.Second, nil, nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest || apiErr.Error() != "missing field (HTTP 400)" {
		t.Fatalf("unexpected JSON APIError: %+v (err=%v)", apiErr, err)
	}

	err = CallContext(t.Context(), srv.URL, http.MethodGet, "/plain-err", nil, 2*time.Second, nil, nil)
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadGateway || !strings.Contains(apiErr.Error(), "HTTP 502") {
		t.Fatalf("unexpected plain APIError: %+v (err=%v)", apiErr, err)
	}

	var out struct {
		OK bool `json:"ok"`
		N  int  `json:"n"`
	}
	if err := CallContext(t.Context(), srv.URL, http.MethodGet, "/ok", nil, 2*time.Second, nil, &out); err != nil {
		t.Fatalf("unexpected error on /ok: %v", err)
	}
	if !out.OK || out.N != 42 {
		t.Fatalf("unexpected decoded response: %+v", out)
	}
}

func TestCallContextBodyLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		chunk := make([]byte, 1024*1024)
		for range 17 {
			_, _ = w.Write(chunk)
		}
	}))
	defer srv.Close()

	err := CallContext(t.Context(), srv.URL, http.MethodGet, "/huge", nil, 5*time.Second, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected response size limit error, got %v", err)
	}
}

func TestDownloadContextSHA256MismatchCleanupAndAtomicSuccess(t *testing.T) {
	dir := t.TempDir()
	dstPath := filepath.Join(dir, "fleet-bin")
	if err := os.WriteFile(dstPath, []byte("existing-good-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	goodPayload := []byte("new-release-binary-bytes")
	goodSum := sha256.Sum256(goodPayload)
	goodHex := hex.EncodeToString(goodSum[:])

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bad-sha":
			w.Header().Set("X-Fleet-SHA256", strings.Repeat("0", 64))
			_, _ = w.Write([]byte("corrupted-bytes"))
		case "/not-found":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"missing artifact"}`))
		case "/good":
			w.Header().Set("X-Fleet-SHA256", goodHex)
			_, _ = w.Write(goodPayload)
		}
	}))
	defer srv.Close()

	// SHA-256 mismatch must leave existing file intact and clean up temp file.
	_, _, err := DownloadContext(t.Context(), srv.URL, "/bad-sha", dstPath, 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "SHA-256 mismatch") {
		t.Fatalf("expected SHA-256 mismatch error, got %v", err)
	}
	gotExisting, err := os.ReadFile(dstPath)
	if err != nil || string(gotExisting) != "existing-good-binary" {
		t.Fatalf("existing dstPath was clobbered: %q err=%v", gotExisting, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "fleet-bin" {
		t.Fatalf("expected no leftover temp files, got %+v err=%v", entries, err)
	}

	// HTTP 404 leaves existing file intact and no temp files.
	if _, _, err := DownloadContext(t.Context(), srv.URL, "/not-found", dstPath, 5*time.Second); err == nil {
		t.Fatal("expected 404 error")
	}

	// Good download atomically replaces dstPath and leaves no temp files.
	sha, n, err := DownloadContext(t.Context(), srv.URL, "/good", dstPath, 5*time.Second)
	if err != nil || sha != goodHex || n != int64(len(goodPayload)) {
		t.Fatalf("DownloadContext good failed: sha=%q n=%d err=%v", sha, n, err)
	}
	gotNew, err := os.ReadFile(dstPath)
	if err != nil || string(gotNew) != string(goodPayload) {
		t.Fatalf("unexpected downloaded content: %q err=%v", gotNew, err)
	}
	entries, err = os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "fleet-bin" {
		t.Fatalf("expected only fleet-bin after atomic rename, got %+v err=%v", entries, err)
	}
}
