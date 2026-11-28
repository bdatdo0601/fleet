package server

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bdatdo0601/fleet/internal/core"
	"github.com/bdatdo0601/fleet/internal/events"
	"github.com/bdatdo0601/fleet/internal/registry"
)

func registerEventRoutes(
	mux *http.ServeMux,
	fleet *core.Fleet,
	route func(bool, func(http.ResponseWriter, *http.Request, *registry.Device) error) http.HandlerFunc,
) {
	mux.HandleFunc("GET /v1/events", route(false, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		if fleet.Events == nil {
			return &core.FleetError{Msg: "event bus unavailable", Status: http.StatusServiceUnavailable}
		}
		q := r.URL.Query()
		var afterID int64
		if raw := cmp.Or(q.Get("after_id"), q.Get("since_id")); raw != "" {
			if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n > 0 {
				afterID = n
			}
		}
		list, err := fleet.Events.List(events.Filter{
			TypePrefix: strings.TrimSpace(q.Get("type")),
			Severity:   strings.TrimSpace(q.Get("severity")),
			Source:     strings.TrimSpace(q.Get("source")),
			Subject:    strings.TrimSpace(q.Get("subject")),
			SinceID:    afterID,
			Limit:      queryInt(r, "limit"),
		})
		if err != nil {
			return err
		}
		if list == nil {
			list = []events.Event{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"events": list})
		return nil
	}))

	mux.HandleFunc("POST /v1/events", route(false, func(w http.ResponseWriter, r *http.Request, c *registry.Device) error {
		if fleet.Events == nil {
			return &core.FleetError{Msg: "event bus unavailable", Status: http.StatusServiceUnavailable}
		}
		var req struct {
			Type        string         `json:"type"`
			Severity    string         `json:"severity"`
			Source      string         `json:"source"`
			Actor       string         `json:"actor"`
			Subject     string         `json:"subject"`
			ChannelHint string         `json:"channel_hint"`
			DedupeKey   string         `json:"dedupe_key"`
			Title       string         `json:"title"`
			Summary     string         `json:"summary"`
			Payload     map[string]any `json:"payload"`
		}
		if err := readJSONBody(r, &req); err != nil {
			return err
		}
		title := strings.TrimSpace(req.Title)
		summary := strings.TrimSpace(req.Summary)
		if title == "" && summary == "" {
			return &core.FleetError{Msg: "title or summary is required", Status: http.StatusBadRequest}
		}
		ev, created, err := fleet.Events.Publish(events.Event{
			Type:        cmp.Or(strings.TrimSpace(req.Type), "external.event"),
			Severity:    events.NormalizeSeverity(req.Severity),
			Source:      cmp.Or(strings.TrimSpace(req.Source), "device:"+c.Name),
			Actor:       cmp.Or(strings.TrimSpace(req.Actor), c.Name),
			Subject:     strings.TrimSpace(req.Subject),
			ChannelHint: strings.TrimSpace(req.ChannelHint),
			DedupeKey:   strings.TrimSpace(req.DedupeKey),
			Title:       cmp.Or(title, summary),
			Summary:     summary,
			Payload:     req.Payload,
		})
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "event": ev, "created": created})
		return nil
	}))

	mux.HandleFunc("GET /v1/events/stream", route(false, handleEventSSE(fleet)))
}

func handleEventSSE(fleet *core.Fleet) func(http.ResponseWriter, *http.Request, *registry.Device) error {
	return func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		if fleet.Events == nil {
			return &core.FleetError{Msg: "event bus unavailable", Status: http.StatusServiceUnavailable}
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			return &core.FleetError{Msg: "streaming unsupported", Status: http.StatusInternalServerError}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)

		sub := fleet.Events.Subscribe()
		defer fleet.Events.Unsubscribe(sub)

		_, _ = fmt.Fprintf(w, "event: ready\ndata: {}\n\n")
		flusher.Flush()

		keepalive := time.NewTicker(25 * time.Second)
		defer keepalive.Stop()
		for {
			select {
			case <-r.Context().Done():
				return nil
			case <-keepalive.C:
				_, _ = fmt.Fprintf(w, ": keepalive\n\n")
				flusher.Flush()
			case ev, open := <-sub:
				if !open {
					return nil
				}
				b, err := json.Marshal(ev)
				if err != nil {
					continue
				}
				_, _ = fmt.Fprintf(w, "event: event\ndata: %s\n\n", b)
				flusher.Flush()
			}
		}
	}
}
