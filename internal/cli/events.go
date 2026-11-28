package cli

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bdatdo0601/fleet/internal/client"
	"github.com/bdatdo0601/fleet/internal/events"
)

func eventsUsageText() string {
	return `usage: fleet events <command> [options]

Commands:
  ls     [--type PREFIX] [--severity S] [--source SRC] [--subject SUB] [--limit N] [--json]
  tail   (stream live events over SSE)
  emit   [--type T] [--severity info|warn|error|critical] [--subject SUB] [--channel CH] [--title TITLE] [--summary BODY] [--json]
`
}

func cmdEvents(args []string, cio *IO) error {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		switch args[0] {
		case "help":
			cio.printf("%s", eventsUsageText())
			return nil
		case "ls", "list":
			return cmdEventsList(args[1:], cio)
		case "tail", "stream":
			return cmdEventsTail(args[1:], cio)
		case "emit", "publish":
			return cmdEventsEmit(args[1:], cio)
		default:
			return fmt.Errorf("unknown events subcommand %q\n\n%s", args[0], eventsUsageText())
		}
	}
	return cmdEventsList(args, cio)
}

func cmdEventsList(args []string, cio *IO) error {
	fs := flag.NewFlagSet("events ls", flag.ContinueOnError)
	typePrefix := fs.String("type", "", "event type prefix filter")
	severity := fs.String("severity", "", "severity filter")
	source := fs.String("source", "", "source filter")
	subject := fs.String("subject", "", "subject filter")
	limit := fs.Int("limit", 40, "max events")
	jsonOut := fs.Bool("json", false, "output JSON")
	pos, err := parseFlagsInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return fmt.Errorf("unexpected positional args: %s", strings.Join(pos, " "))
	}
	api, err := client.ResolveAPI("")
	if err != nil {
		return err
	}
	q := url.Values{"limit": {strconv.Itoa(max(*limit, 1))}}
	for k, v := range map[string]string{"type": *typePrefix, "severity": *severity, "source": *source, "subject": *subject} {
		if strings.TrimSpace(v) != "" {
			q.Set(k, strings.TrimSpace(v))
		}
	}
	var res struct {
		Events []events.Event `json:"events"`
	}
	if err := client.Call(api, http.MethodGet, "/v1/events?"+q.Encode(), nil, 15*time.Second, nil, &res); err != nil {
		return err
	}
	if *jsonOut {
		return writeJSONOut(cio.Stdout, res)
	}
	if len(res.Events) == 0 {
		cio.printf("No matching events.\n")
		return nil
	}
	for _, ev := range res.Events {
		cio.printf("#%-5d %-20s [%-8s] %-16s %s\n", ev.ID, ev.CreatedAt, ev.Severity, ev.Type, cmp.Or(ev.Title, ev.Summary))
	}
	return nil
}

func cmdEventsEmit(args []string, cio *IO) error {
	fs := flag.NewFlagSet("events emit", flag.ContinueOnError)
	evType := fs.String("type", "external.event", "event type")
	severity := fs.String("severity", "info", "info|warn|error|critical")
	subject := fs.String("subject", "", "event subject")
	channel := fs.String("channel", "", "preferred assistant channel")
	title := fs.String("title", "", "event title")
	summary := fs.String("summary", "", "event summary")
	jsonOut := fs.Bool("json", false, "output JSON")
	pos, err := parseFlagsInterspersed(fs, args)
	if err != nil {
		return err
	}
	t := strings.TrimSpace(*title)
	if t == "" && len(pos) > 0 {
		t = strings.Join(pos, " ")
	}
	if t == "" && strings.TrimSpace(*summary) == "" {
		return fmt.Errorf("usage: fleet events emit --title TEXT [--summary TEXT] [--severity info|warn|error|critical]")
	}
	api, err := client.ResolveAPI("")
	if err != nil {
		return err
	}
	var res map[string]any
	if err := client.Call(api, http.MethodPost, "/v1/events", map[string]any{
		"type":         *evType,
		"severity":     *severity,
		"subject":      *subject,
		"channel_hint": *channel,
		"title":        t,
		"summary":      *summary,
	}, 15*time.Second, nil, &res); err != nil {
		return err
	}
	if *jsonOut {
		return writeJSONOut(cio.Stdout, res)
	}
	cio.printf("event emitted (%s)\n", *evType)
	return nil
}

func cmdEventsTail(args []string, cio *IO) error {
	if err := parseFlagsNoPositional("events tail", args); err != nil {
		return err
	}
	api, err := client.ResolveAPI("")
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(api, "/")+"/v1/events/stream", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET /v1/events/stream status %d", resp.StatusCode)
	}
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		raw := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if raw == "" || raw == "{}" {
			continue
		}
		var ev events.Event
		if json.Unmarshal([]byte(raw), &ev) == nil && ev.Type != "" {
			cio.printf("#%-5d %-20s [%-8s] %-16s %s\n", ev.ID, ev.CreatedAt, ev.Severity, ev.Type, cmp.Or(ev.Title, ev.Summary))
		}
	}
	return nil
}
