package mcp

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const slackToolTimeout = 15 * time.Second

func callSlackTool(name string, rawArgs json.RawMessage, api string) (any, error) {
	switch name {
	case "fleet_slack_channels":
		return callSlackChannels(api, rawArgs)
	case "fleet_slack_read":
		return callSlackRead(api, rawArgs)
	case "fleet_slack_send":
		return callSlackSend(api, rawArgs)
	default:
		return nil, fmt.Errorf("unknown slack tool %q", name)
	}
}

func callSlackChannels(api string, rawArgs json.RawMessage) (any, error) {
	a, err := decodeArgs[struct {
		Account    string `json:"account"`
		IncludeAll bool   `json:"include_all"`
		Limit      *int   `json:"limit"`
	}](rawArgs)
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	if acct := strings.TrimSpace(a.Account); acct != "" {
		q.Set("account", acct)
	}
	if a.IncludeAll {
		q.Set("include_all", "true")
	}
	if a.Limit != nil && *a.Limit > 0 {
		q.Set("limit", strconv.Itoa(*a.Limit))
	}
	qs := ""
	if enc := q.Encode(); enc != "" {
		qs = "?" + enc
	}
	return callJSON(api, "GET", "/v1/slacker/channels"+qs, nil, slackToolTimeout)
}

func callSlackRead(api string, rawArgs json.RawMessage) (any, error) {
	a, err := decodeArgs[struct {
		Account  string `json:"account"`
		Channel  string `json:"channel"`
		ThreadTS string `json:"thread_ts"`
		Query    string `json:"query"`
		Oldest   string `json:"oldest"`
		Latest   string `json:"latest"`
		Limit    *int   `json:"limit"`
	}](rawArgs)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(a.Channel) == "" && strings.TrimSpace(a.Query) == "" {
		return nil, fmt.Errorf("channel or query is required")
	}
	q := url.Values{}
	if acct := strings.TrimSpace(a.Account); acct != "" {
		q.Set("account", acct)
	}
	if ch := strings.TrimSpace(a.Channel); ch != "" {
		q.Set("channel", ch)
	}
	if ts := strings.TrimSpace(a.ThreadTS); ts != "" {
		q.Set("thread_ts", ts)
	}
	if query := strings.TrimSpace(a.Query); query != "" {
		q.Set("query", query)
	}
	if oldest := strings.TrimSpace(a.Oldest); oldest != "" {
		q.Set("oldest", oldest)
	}
	if latest := strings.TrimSpace(a.Latest); latest != "" {
		q.Set("latest", latest)
	}
	if a.Limit != nil && *a.Limit > 0 {
		q.Set("limit", strconv.Itoa(*a.Limit))
	}
	return callJSON(api, "GET", "/v1/slacker/messages?"+q.Encode(), nil, slackToolTimeout)
}

func callSlackSend(api string, rawArgs json.RawMessage) (any, error) {
	a, err := decodeArgs[struct {
		Account  string `json:"account"`
		Channel  string `json:"channel"`
		Text     string `json:"text"`
		ThreadTS string `json:"thread_ts"`
	}](rawArgs)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(a.Channel) == "" || strings.TrimSpace(a.Text) == "" {
		return nil, fmt.Errorf("channel and text are required")
	}
	body := map[string]string{
		"channel": strings.TrimSpace(a.Channel),
		"text":    a.Text,
	}
	if acct := strings.TrimSpace(a.Account); acct != "" {
		body["account"] = acct
	}
	if ts := strings.TrimSpace(a.ThreadTS); ts != "" {
		body["thread_ts"] = ts
	}
	return callJSON(api, "POST", "/v1/slacker/send", body, slackToolTimeout)
}
