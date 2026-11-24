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

const webCSP = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

func assetContentType(ext string) string {
	switch strings.ToLower(ext) {
	case ".js":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".html":
		return "text/html; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}

func registerWebRoutes(
	mux *http.ServeMux,
	route func(bool, func(http.ResponseWriter, *http.Request, *registry.Device) error) http.HandlerFunc,
) {
	mux.HandleFunc("GET /{$}", route(false, func(w http.ResponseWriter, _ *http.Request, _ *registry.Device) error {
		data, err := webFS.ReadFile("web/index.html")
		if err != nil {
			return err
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Content-Security-Policy", webCSP)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
		return nil
	}))

	mux.HandleFunc("GET /favicon.ico", route(false, func(w http.ResponseWriter, _ *http.Request, _ *registry.Device) error {
		data, err := webFS.ReadFile("web/favicon.svg")
		if err != nil {
			return err
		}
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
		return nil
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
		data, err := webFS.ReadFile(fullPath)
		if err != nil {
			return &core.FleetError{Msg: "not found", Status: http.StatusNotFound}
		}
		w.Header().Set("Content-Type", assetContentType(filepath.Ext(file)))
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
		return nil
	}))
}
