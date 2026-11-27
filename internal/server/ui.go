package server

import (
	"embed"
	"io/fs"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/bdatdo0601/fleet/internal/core"
	"github.com/bdatdo0601/fleet/internal/registry"
)

//go:embed web
var webFS embed.FS

const webCSP = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; connect-src 'self'; frame-src 'self'; manifest-src 'self'; worker-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

func assetContentType(ext string) string {
	switch strings.ToLower(ext) {
	case ".js":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	case ".webmanifest":
		return "application/manifest+json; charset=utf-8"
	case ".json":
		return "application/json; charset=utf-8"
	case ".html":
		return "text/html; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}

func serveEmbeddedAsset(
	w http.ResponseWriter,
	path string,
	contentType string,
	extraHeaders map[string]string,
) error {
	data, err := webFS.ReadFile(path)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-cache")
	for k, v := range extraHeaders {
		w.Header().Set(k, v)
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
	return nil
}

func registerWebRoutes(
	mux *http.ServeMux,
	route func(bool, func(http.ResponseWriter, *http.Request, *registry.Device) error) http.HandlerFunc,
) {
	mux.HandleFunc("GET /{$}", route(false, func(w http.ResponseWriter, _ *http.Request, _ *registry.Device) error {
		return serveEmbeddedAsset(w, "web/index.html", "text/html; charset=utf-8", map[string]string{
			"Content-Security-Policy": webCSP,
		})
	}))

	mux.HandleFunc("GET /favicon.ico", route(false, func(w http.ResponseWriter, _ *http.Request, _ *registry.Device) error {
		return serveEmbeddedAsset(w, "web/favicon.svg", "image/svg+xml", nil)
	}))

	mux.HandleFunc("GET /apple-touch-icon.png", route(false, func(w http.ResponseWriter, _ *http.Request, _ *registry.Device) error {
		return serveEmbeddedAsset(w, "web/apple-touch-icon.png", "image/png", nil)
	}))

	mux.HandleFunc("GET /manifest.webmanifest", route(false, func(w http.ResponseWriter, _ *http.Request, _ *registry.Device) error {
		return serveEmbeddedAsset(w, "web/manifest.webmanifest", "application/manifest+json; charset=utf-8", nil)
	}))

	mux.HandleFunc("GET /sw.js", route(false, func(w http.ResponseWriter, _ *http.Request, _ *registry.Device) error {
		return serveEmbeddedAsset(w, "web/sw.js", "text/javascript; charset=utf-8", map[string]string{
			"Service-Worker-Allowed": "/",
		})
	}))

	mux.HandleFunc("GET /assets/{file...}", route(false, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		file := r.PathValue("file")
		if file == "" || strings.Contains(file, "/") || strings.Contains(file, "\\") || strings.Contains(file, "..") {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return nil
		}
		fullPath := "web/" + file
		if !fs.ValidPath(fullPath) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return nil
		}
		if _, err := webFS.ReadFile(fullPath); err != nil {
			return &core.FleetError{Msg: "not found", Status: http.StatusNotFound}
		}
		return serveEmbeddedAsset(w, fullPath, assetContentType(filepath.Ext(file)), nil)
	}))
}
