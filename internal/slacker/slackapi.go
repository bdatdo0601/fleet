package slacker

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const DefaultSlackBaseURL = "https://slack.com/api"

// SlackClient calls Slack Web API endpoints using user, bot, or app-level tokens.
type SlackClient struct {
	BaseURL    string
	HTTPClient *http.Client
}

// AuthTestResult holds the identity returned by Slack's `auth.test`.
type AuthTestResult struct {
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
	UserID string `json:"user_id"`
	User   string `json:"user"`
	TeamID string `json:"team_id"`
	Team   string `json:"team"`
	URL    string `json:"url"`
}

// SlackMessage represents one message from conversations.history or conversations.replies.
type SlackMessage struct {
	Type     string `json:"type"`
	User     string `json:"user"`
	Username string `json:"username,omitempty"`
	BotID    string `json:"bot_id,omitempty"`
	Text     string `json:"text"`
	TS       string `json:"ts"`
	ThreadTS string `json:"thread_ts,omitempty"`
}

// FetchedMessage holds the exact reacted Slack message along with optional thread parent context.
type FetchedMessage struct {
	ChannelID   string
	ChannelName string
	Message     SlackMessage
	Parent      *SlackMessage
	AuthorName  string
	Permalink   string
}

// ReactionEvent represents a normalized Slack `reaction_added` event.
type ReactionEvent struct {
	EventID   string
	User      string
	Reaction  string
	ChannelID string
	MessageTS string
}

// SlackChannelInfo summarizes one Slack channel returned by users.conversations.
type SlackChannelInfo struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	IsPrivate bool   `json:"is_private"`
}

// EnrichedMessage represents a Slack message returned to agents for context gathering.
type EnrichedMessage struct {
	ChannelID   string `json:"channel_id,omitempty"`
	ChannelName string `json:"channel_name,omitempty"`
	User        string `json:"user,omitempty"`
	AuthorName  string `json:"author_name,omitempty"`
	TS          string `json:"ts"`
	ThreadTS    string `json:"thread_ts,omitempty"`
	Text        string `json:"text"`
	Permalink   string `json:"permalink,omitempty"`
}

// ReadMessagesOpts configures SlackClient.ReadMessages.
type ReadMessagesOpts struct {
	ChannelID string
	ThreadTS  string
	Query     string
	Oldest    string
	Latest    string
	Limit     int
}

// ReadMessagesResult holds messages read from channel history, thread replies, or search.
type ReadMessagesResult struct {
	Account     string            `json:"account"`
	ChannelID   string            `json:"channel_id,omitempty"`
	ChannelName string            `json:"channel_name,omitempty"`
	ThreadTS    string            `json:"thread_ts,omitempty"`
	Mode        string            `json:"mode"`
	Messages    []EnrichedMessage `json:"messages"`
}

// PostedMessage describes the Slack message created by PostMessage.
type PostedMessage struct {
	OK        bool   `json:"ok"`
	Account   string `json:"account,omitempty"`
	Channel   string `json:"channel"`
	TS        string `json:"ts"`
	ThreadTS  string `json:"thread_ts,omitempty"`
	Permalink string `json:"permalink,omitempty"`
}

func (c *SlackClient) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func (c *SlackClient) apiBase() string {
	return strings.TrimRight(cmp.Or(c.BaseURL, DefaultSlackBaseURL), "/")
}

func (c *SlackClient) doAPI(ctx context.Context, method, path, token string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.apiBase()+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	return c.execSlackReq(req, path, out)
}

func (c *SlackClient) doJSONPost(ctx context.Context, path, token string, payload any, out any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiBase()+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	return c.execSlackReq(req, path, out)
}

func (c *SlackClient) execSlackReq(req *http.Request, path string, out any) error {
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("slack API %s HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var env struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("slack API %s invalid JSON: %w", path, err)
	}
	if !env.OK {
		return fmt.Errorf("slack API %s error: %s", path, cmp.Or(env.Error, "unknown"))
	}
	if out != nil {
		return json.Unmarshal(body, out)
	}
	return nil
}

// AuthTest validates a user/bot OAuth token via `auth.test`.
func (c *SlackClient) AuthTest(ctx context.Context, token string) (AuthTestResult, error) {
	var res AuthTestResult
	err := c.doAPI(ctx, http.MethodPost, "/auth.test", token, &res)
	return res, err
}

// OpenSocketURL exchanges a Slack app-level token (`xapp-...`) for a Socket Mode WebSocket URL.
func (c *SlackClient) OpenSocketURL(ctx context.Context, appToken string) (string, error) {
	var res struct {
		URL string `json:"url"`
	}
	if err := c.doAPI(ctx, http.MethodPost, "/apps.connections.open", appToken, &res); err != nil {
		return "", err
	}
	if res.URL == "" {
		return "", fmt.Errorf("slack apps.connections.open returned empty url")
	}
	return res.URL, nil
}

// PostMessage posts a message (or thread reply when threadTS != "") via `chat.postMessage`.
func (c *SlackClient) PostMessage(ctx context.Context, token, channelID, text, threadTS string) (PostedMessage, error) {
	if strings.TrimSpace(channelID) == "" {
		return PostedMessage{}, newSlackerErr("channel is required", 400)
	}
	if strings.TrimSpace(text) == "" {
		return PostedMessage{}, newSlackerErr("text is required", 400)
	}
	payload := map[string]any{
		"channel": channelID,
		"text":    text,
	}
	if strings.TrimSpace(threadTS) != "" {
		payload["thread_ts"] = strings.TrimSpace(threadTS)
	}
	var res struct {
		Channel string `json:"channel"`
		TS      string `json:"ts"`
		Message struct {
			ThreadTS string `json:"thread_ts"`
		} `json:"message"`
	}
	if err := c.doJSONPost(ctx, "/chat.postMessage", token, payload, &res); err != nil {
		return PostedMessage{}, err
	}
	ch := cmp.Or(res.Channel, channelID)
	out := PostedMessage{
		OK:       true,
		Channel:  ch,
		TS:       res.TS,
		ThreadTS: cmp.Or(res.Message.ThreadTS, strings.TrimSpace(threadTS)),
	}
	if out.TS != "" {
		out.Permalink = c.fetchPermalink(ctx, token, ch, out.TS)
	}
	return out, nil
}

// ListUserChannels returns channels accessible to userToken via `users.conversations`.
func (c *SlackClient) ListUserChannels(ctx context.Context, userToken string, limit int) ([]SlackChannelInfo, error) {
	if limit <= 0 {
		limit = 100
	}
	limit = min(limit, 500)
	q := url.Values{
		"types":            {"public_channel,private_channel"},
		"exclude_archived": {"true"},
		"limit":            {strconv.Itoa(limit)},
	}
	var res struct {
		Channels []SlackChannelInfo `json:"channels"`
	}
	if err := c.doAPI(ctx, http.MethodGet, "/users.conversations?"+q.Encode(), userToken, &res); err != nil {
		return nil, err
	}
	return res.Channels, nil
}

// ReadMessages reads channel history, thread replies, or search results for agent context gathering.
func (c *SlackClient) ReadMessages(ctx context.Context, userToken string, opts ReadMessagesOpts) (ReadMessagesResult, error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = 20
	}
	limit = min(limit, 100)
	chID := strings.TrimSpace(opts.ChannelID)
	threadTS := strings.TrimSpace(opts.ThreadTS)
	query := strings.TrimSpace(opts.Query)

	if chID == "" && query == "" {
		return ReadMessagesResult{}, newSlackerErr("channel or query is required", 400)
	}
	if threadTS != "" && chID == "" {
		return ReadMessagesResult{}, newSlackerErr("channel is required when thread_ts is set", 400)
	}

	if chID == "" {
		return c.searchMessages(ctx, userToken, query, limit)
	}

	res := ReadMessagesResult{
		ChannelID:   chID,
		ChannelName: c.fetchChannelName(ctx, userToken, chID),
		ThreadTS:    threadTS,
	}
	var rawMsgs []SlackMessage
	if threadTS != "" {
		res.Mode = "thread"
		var rep struct {
			Messages []SlackMessage `json:"messages"`
		}
		rq := url.Values{"channel": {chID}, "ts": {threadTS}, "limit": {strconv.Itoa(limit)}}
		if opts.Oldest != "" {
			rq.Set("oldest", opts.Oldest)
		}
		if opts.Latest != "" {
			rq.Set("latest", opts.Latest)
		}
		if err := c.doAPI(ctx, http.MethodGet, "/conversations.replies?"+rq.Encode(), userToken, &rep); err != nil {
			return res, err
		}
		rawMsgs = rep.Messages
	} else {
		res.Mode = "history"
		var hist struct {
			Messages []SlackMessage `json:"messages"`
		}
		hq := url.Values{"channel": {chID}, "limit": {strconv.Itoa(limit)}}
		if opts.Oldest != "" {
			hq.Set("oldest", opts.Oldest)
		}
		if opts.Latest != "" {
			hq.Set("latest", opts.Latest)
		}
		if err := c.doAPI(ctx, http.MethodGet, "/conversations.history?"+hq.Encode(), userToken, &hist); err != nil {
			return res, err
		}
		rawMsgs = hist.Messages
	}

	userCache := map[string]string{}
	queryLower := strings.ToLower(query)
	for _, m := range rawMsgs {
		if queryLower != "" && !strings.Contains(strings.ToLower(m.Text), queryLower) {
			continue
		}
		author := c.cachedUserName(ctx, userToken, m.User, m.Username, userCache)
		res.Messages = append(res.Messages, EnrichedMessage{
			ChannelID:   chID,
			ChannelName: res.ChannelName,
			User:        m.User,
			AuthorName:  author,
			TS:          m.TS,
			ThreadTS:    m.ThreadTS,
			Text:        m.Text,
		})
	}
	if res.Messages == nil {
		res.Messages = []EnrichedMessage{}
	}
	return res, nil
}

func (c *SlackClient) searchMessages(ctx context.Context, userToken, query string, limit int) (ReadMessagesResult, error) {
	q := url.Values{
		"query":    {query},
		"count":    {strconv.Itoa(limit)},
		"sort":     {"timestamp"},
		"sort_dir": {"desc"},
	}
	var resp struct {
		Messages struct {
			Matches []struct {
				Channel struct {
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"channel"`
				User      string `json:"user"`
				Username  string `json:"username"`
				TS        string `json:"ts"`
				ThreadTS  string `json:"thread_ts"`
				Text      string `json:"text"`
				Permalink string `json:"permalink"`
			} `json:"matches"`
		} `json:"messages"`
	}
	if err := c.doAPI(ctx, http.MethodGet, "/search.messages?"+q.Encode(), userToken, &resp); err != nil {
		return ReadMessagesResult{}, err
	}
	out := ReadMessagesResult{
		Mode:     "search",
		Messages: make([]EnrichedMessage, 0, len(resp.Messages.Matches)),
	}
	for _, m := range resp.Messages.Matches {
		out.Messages = append(out.Messages, EnrichedMessage{
			ChannelID:   m.Channel.ID,
			ChannelName: m.Channel.Name,
			User:        m.User,
			AuthorName:  m.Username,
			TS:          m.TS,
			ThreadTS:    m.ThreadTS,
			Text:        m.Text,
			Permalink:   m.Permalink,
		})
	}
	return out, nil
}

func (c *SlackClient) cachedUserName(ctx context.Context, userToken, userID, fallback string, cache map[string]string) string {
	if userID == "" {
		return fallback
	}
	if v, ok := cache[userID]; ok {
		return v
	}
	name := cmp.Or(c.fetchUserName(ctx, userToken, userID), fallback, userID)
	cache[userID] = name
	return name
}

// FetchMessage fetches the exact message for (channelID, messageTS), checking conversations.history and
// falling back to conversations.replies so thread replies are resolved accurately.
func (c *SlackClient) FetchMessage(ctx context.Context, userToken, channelID, messageTS string) (FetchedMessage, error) {
	out := FetchedMessage{ChannelID: channelID}

	var histResp struct {
		Messages []SlackMessage `json:"messages"`
	}
	hq := url.Values{
		"channel":   {channelID},
		"latest":    {messageTS},
		"oldest":    {messageTS},
		"inclusive": {"true"},
		"limit":     {"5"},
	}
	if err := c.doAPI(ctx, http.MethodGet, "/conversations.history?"+hq.Encode(), userToken, &histResp); err != nil {
		return out, err
	}
	var found *SlackMessage
	for i := range histResp.Messages {
		if histResp.Messages[i].TS == messageTS {
			m := histResp.Messages[i]
			found = &m
			break
		}
	}

	if found == nil {
		var repResp struct {
			Messages []SlackMessage `json:"messages"`
		}
		rq := url.Values{
			"channel": {channelID},
			"ts":      {messageTS},
			"limit":   {"200"},
		}
		if err := c.doAPI(ctx, http.MethodGet, "/conversations.replies?"+rq.Encode(), userToken, &repResp); err != nil {
			return out, err
		}
		for i := range repResp.Messages {
			if repResp.Messages[i].TS == messageTS {
				m := repResp.Messages[i]
				found = &m
				break
			}
		}
		if found == nil {
			return out, fmt.Errorf("slack message %s in %s not found", messageTS, channelID)
		}
		if found.ThreadTS != "" && found.ThreadTS != found.TS {
			for i := range repResp.Messages {
				if repResp.Messages[i].TS == found.ThreadTS {
					p := repResp.Messages[i]
					out.Parent = &p
					break
				}
			}
			if out.Parent == nil {
				out.Parent = c.fetchThreadParent(ctx, userToken, channelID, found.ThreadTS)
			}
		}
	}

	out.Message = *found
	out.Permalink = c.fetchPermalink(ctx, userToken, channelID, messageTS)
	out.ChannelName = c.fetchChannelName(ctx, userToken, channelID)
	out.AuthorName = c.fetchUserName(ctx, userToken, found.User)
	return out, nil
}

func (c *SlackClient) fetchThreadParent(ctx context.Context, userToken, channelID, threadTS string) *SlackMessage {
	var rep struct {
		Messages []SlackMessage `json:"messages"`
	}
	rq := url.Values{"channel": {channelID}, "ts": {threadTS}, "limit": {"5"}}
	if err := c.doAPI(ctx, http.MethodGet, "/conversations.replies?"+rq.Encode(), userToken, &rep); err != nil {
		return nil
	}
	for i := range rep.Messages {
		if rep.Messages[i].TS == threadTS {
			p := rep.Messages[i]
			return &p
		}
	}
	return nil
}

func (c *SlackClient) fetchPermalink(ctx context.Context, userToken, channelID, messageTS string) string {
	var res struct {
		Permalink string `json:"permalink"`
	}
	q := url.Values{"channel": {channelID}, "message_ts": {messageTS}}
	if err := c.doAPI(ctx, http.MethodGet, "/chat.getPermalink?"+q.Encode(), userToken, &res); err != nil {
		return ""
	}
	return res.Permalink
}

func (c *SlackClient) fetchChannelName(ctx context.Context, userToken, channelID string) string {
	var res struct {
		Channel struct {
			Name string `json:"name"`
		} `json:"channel"`
	}
	q := url.Values{"channel": {channelID}}
	if err := c.doAPI(ctx, http.MethodGet, "/conversations.info?"+q.Encode(), userToken, &res); err != nil {
		return ""
	}
	return res.Channel.Name
}

func (c *SlackClient) fetchUserName(ctx context.Context, userToken, userID string) string {
	if userID == "" {
		return ""
	}
	var res struct {
		User struct {
			Name    string `json:"name"`
			Profile struct {
				DisplayName string `json:"display_name"`
				RealName    string `json:"real_name"`
			} `json:"profile"`
		} `json:"user"`
	}
	q := url.Values{"user": {userID}}
	if err := c.doAPI(ctx, http.MethodGet, "/users.info?"+q.Encode(), userToken, &res); err != nil {
		return ""
	}
	return cmp.Or(res.User.Profile.DisplayName, res.User.Profile.RealName, res.User.Name)
}

// FormatTaskPrompt builds the task prompt delivered to `fleet session ... start-task`.
func FormatTaskPrompt(account string, ev ReactionEvent, msg FetchedMessage, ch ChannelMapping) string {
	var b strings.Builder

	body := strings.TrimSpace(msg.Message.Text)
	if body == "" {
		body = "Inspect this Slack message and handle the request."
	}
	b.WriteString(body)
	b.WriteString("\n")

	if msg.Parent != nil && strings.TrimSpace(msg.Parent.Text) != "" {
		b.WriteString("\nThread parent:\n")
		b.WriteString(strings.TrimSpace(msg.Parent.Text))
		b.WriteString("\n")
	}

	b.WriteString("\n---\n")
	b.WriteString("Slack request")
	if author := cmp.Or(msg.AuthorName, msg.Message.User); author != "" {
		fmt.Fprintf(&b, " from %s", author)
	}
	chanName := cmp.Or(msg.ChannelName, ch.ChannelName)
	var meta []string
	if chanName != "" && chanName != ev.ChannelID {
		fmt.Fprintf(&b, " in #%s", chanName)
		meta = append(meta, "`"+ev.ChannelID+"`")
	} else {
		fmt.Fprintf(&b, " in channel `%s`", ev.ChannelID)
	}
	if ev.MessageTS != "" {
		meta = append(meta, "ts `"+ev.MessageTS+"`")
	}
	if msg.Message.ThreadTS != "" && msg.Message.ThreadTS != ev.MessageTS {
		meta = append(meta, "thread `"+msg.Message.ThreadTS+"`")
	}
	acct := cmp.Or(account, DefaultAccountID)
	if acct != "" && acct != DefaultAccountID {
		meta = append(meta, "account `"+acct+"`")
	}
	if len(meta) > 0 {
		fmt.Fprintf(&b, " (%s)", strings.Join(meta, ", "))
	}
	b.WriteString(".\n")

	if msg.Permalink != "" {
		fmt.Fprintf(&b, "Link: %s\n", msg.Permalink)
	}
	b.WriteString("Use `fleet_slack_read` for more channel/thread context or `fleet_slack_send` to reply.\n")
	return b.String()
}
