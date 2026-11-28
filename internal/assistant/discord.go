package assistant

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/bdatdo0601/fleet/internal/ws"
)

const (
	defaultDiscordAPIBase = "https://discord.com/api/v10"
	// Intents: GUILDS (1<<0) | GUILD_MESSAGES (1<<9) | DIRECT_MESSAGES (1<<12) | MESSAGE_CONTENT (1<<15) = 37377
	discordGatewayIntents = 37377
)

type discordAPI struct {
	baseURL    string
	httpClient *http.Client
}

type discordUser struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Bot      bool   `json:"bot"`
}

type discordMessage struct {
	ID        string      `json:"id"`
	ChannelID string      `json:"channel_id"`
	Content   string      `json:"content"`
	Author    discordUser `json:"author"`
}

type discordInteraction struct {
	ID     string      `json:"id"`
	Type   int         `json:"type"`
	Token  string      `json:"token"`
	User   discordUser `json:"user"`
	Member *struct {
		User discordUser `json:"user"`
	} `json:"member"`
	Data struct {
		CustomID string `json:"custom_id"`
	} `json:"data"`
}

type gatewayPayload struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d,omitempty"`
	S  *int64          `json:"s,omitempty"`
	T  string          `json:"t,omitempty"`
}

func newDiscordAPI(baseURL string) *discordAPI {
	return &discordAPI{
		baseURL:    strings.TrimRight(cmp.Or(strings.TrimSpace(baseURL), defaultDiscordAPIBase), "/"),
		httpClient: &http.Client{Timeout: 15 * time.Second},
	}
}

func (d *discordAPI) doJSON(ctx context.Context, token, method, path string, body, out any) error {
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		bodyReader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, d.baseURL+path, bodyReader)
	if err != nil {
		return errors.New(scrubSecret(err.Error(), token))
	}
	req.Header.Set("User-Agent", "DiscordBot (https://github.com/bdatdo0601/fleet, 1.0)")
	if token != "" {
		req.Header.Set("Authorization", "Bot "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := d.httpClient.Do(req)
	if err != nil {
		return errors.New(scrubSecret(err.Error(), token))
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil {
		return errors.New(scrubSecret(err.Error(), token))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return errors.New(scrubSecret(fmt.Sprintf("discord API %s %s HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw))), token))
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return err
		}
	}
	return nil
}

func (d *discordAPI) GetMe(ctx context.Context, token string) (discordUser, error) {
	var u discordUser
	err := d.doJSON(ctx, token, http.MethodGet, "/users/@me", nil, &u)
	return u, err
}

func (d *discordAPI) GetGatewayBot(ctx context.Context, token string) (string, error) {
	var res struct {
		URL string `json:"url"`
	}
	if err := d.doJSON(ctx, token, http.MethodGet, "/gateway/bot", nil, &res); err != nil {
		return "", err
	}
	if strings.TrimSpace(res.URL) == "" {
		return "", errors.New("discord /gateway/bot returned empty url")
	}
	sep := "?"
	if strings.Contains(res.URL, "?") {
		sep = "&"
	}
	return res.URL + sep + "v=10&encoding=json", nil
}

func (d *discordAPI) GetChannelMessagesAfter(ctx context.Context, token, channelID, afterID string) ([]discordMessage, error) {
	path := fmt.Sprintf("/channels/%s/messages?limit=50", url.PathEscape(channelID))
	if afterID != "" {
		path += "&after=" + url.QueryEscape(afterID)
	}
	var msgs []discordMessage
	if err := d.doJSON(ctx, token, http.MethodGet, path, nil, &msgs); err != nil {
		return nil, err
	}
	// Discord returns newest first; reverse to chronological order.
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}
	return msgs, nil
}

func (d *discordAPI) CreateMessage(ctx context.Context, token, channelID string, payload map[string]any) (string, error) {
	var res struct {
		ID string `json:"id"`
	}
	err := d.doJSON(ctx, token, http.MethodPost, "/channels/"+url.PathEscape(channelID)+"/messages", payload, &res)
	return res.ID, err
}

func (d *discordAPI) TriggerTyping(ctx context.Context, token, channelID string) {
	_ = d.doJSON(ctx, token, http.MethodPost, "/channels/"+url.PathEscape(channelID)+"/typing", nil, nil)
}

func (d *discordAPI) RespondInteraction(ctx context.Context, token, interactionID, interactionToken string, payload map[string]any) error {
	path := fmt.Sprintf("/interactions/%s/%s/callback", url.PathEscape(interactionID), url.PathEscape(interactionToken))
	return d.doJSON(ctx, token, http.MethodPost, path, payload, nil)
}

func (e *Engine) discordClient() *discordAPI {
	return newDiscordAPI(e.discordAPIBase)
}

func (e *Engine) resolveDiscordBotToken(ctx context.Context, cfg DiscordConfig) (string, error) {
	if !strings.HasPrefix(strings.TrimSpace(cfg.BotTokenOPRef), "op://") {
		return "", newErr("discord.bot_token_op_ref must be an op:// reference", http.StatusBadRequest)
	}
	reader := e.opReader
	if reader == nil {
		reader = defaultOPReader
	}
	tok, err := reader(ctx, strings.TrimSpace(cfg.BotTokenOPRef))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(tok), nil
}

func (e *Engine) setDiscordState(connected bool, botUserID string) {
	e.discordMu.Lock()
	e.discordConnected = connected
	if botUserID != "" {
		e.discordBotUserID = botUserID
	}
	e.discordMu.Unlock()
}

func (e *Engine) notifyDiscordReload() {
	select {
	case e.discordReloadCh <- struct{}{}:
	default:
	}
}

func (e *Engine) runDiscordGateway(ctx context.Context) {
	defer e.wg.Done()
	backoff := 2 * time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		cfg := e.Config()
		if !cfg.Discord.Enabled || ValidateDiscordConfig(cfg.Discord) != nil {
			e.setDiscordState(false, "")
			select {
			case <-ctx.Done():
				return
			case <-e.discordReloadCh:
			case <-time.After(10 * time.Second):
			}
			backoff = 2 * time.Second
			continue
		}
		err := e.runDiscordSession(ctx, cfg.Discord)
		e.setDiscordState(false, "")
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			backoff = 2 * time.Second
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-e.discordReloadCh:
			backoff = 2 * time.Second
		case <-time.After(backoff):
			backoff = min(backoff*2, 30*time.Second)
		}
	}
}

func (e *Engine) runDiscordSession(ctx context.Context, cfg DiscordConfig) error {
	tok, err := e.resolveDiscordBotToken(ctx, cfg)
	if err != nil {
		return err
	}
	gwURL := strings.TrimSpace(e.discordGatewayURL)
	if gwURL == "" {
		gwURL, err = e.discordClient().GetGatewayBot(ctx, tok)
		if err != nil {
			return err
		}
	}
	dialCtx, cancelDial := context.WithTimeout(ctx, 15*time.Second)
	conn, err := ws.DialWS(dialCtx, gwURL, nil)
	cancelDial()
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	sessCtx, cancelSess := context.WithCancel(ctx)
	defer cancelSess()
	go func() {
		select {
		case <-sessCtx.Done():
		case <-e.discordReloadCh:
			_ = conn.Close()
		}
	}()

	var seqMu sync.Mutex
	var lastSeq *int64
	var hbCancel context.CancelFunc
	defer func() {
		if hbCancel != nil {
			hbCancel()
		}
	}()

	for {
		raw, err := conn.ReadText(sessCtx)
		if err != nil {
			return err
		}
		var pkt gatewayPayload
		if json.Unmarshal(raw, &pkt) != nil {
			continue
		}
		if pkt.S != nil {
			seqMu.Lock()
			s := *pkt.S
			lastSeq = &s
			seqMu.Unlock()
		}
		reconnect, hErr := e.handleGatewayOp(sessCtx, conn, tok, cfg, pkt, &seqMu, &lastSeq, &hbCancel)
		if hErr != nil {
			return hErr
		}
		if reconnect {
			return nil
		}
	}
}

func (e *Engine) handleGatewayOp(
	ctx context.Context,
	conn *ws.WSConn,
	tok string,
	cfg DiscordConfig,
	pkt gatewayPayload,
	seqMu *sync.Mutex,
	lastSeq **int64,
	hbCancel *context.CancelFunc,
) (bool, error) {
	switch pkt.Op {
	case 10: // Hello
		var hello struct {
			HeartbeatInterval int `json:"heartbeat_interval"`
		}
		_ = json.Unmarshal(pkt.D, &hello)
		interval := time.Duration(max(hello.HeartbeatInterval, 1000)) * time.Millisecond
		if *hbCancel != nil {
			(*hbCancel)()
		}
		hbCtx, cancel := context.WithCancel(ctx)
		*hbCancel = cancel
		go runDiscordHeartbeat(hbCtx, conn, interval, seqMu, lastSeq)
		return false, sendDiscordIdentify(conn, tok)
	case 1: // Heartbeat request
		seqMu.Lock()
		s := *lastSeq
		seqMu.Unlock()
		_ = sendGatewayFrame(conn, 1, s)
	case 7, 9: // Reconnect / Invalid Session
		return true, nil
	case 0: // Dispatch
		e.handleDiscordDispatch(ctx, tok, cfg, pkt.T, pkt.D)
	}
	return false, nil
}

func sendGatewayFrame(conn *ws.WSConn, op int, d any) error {
	b, err := json.Marshal(map[string]any{"op": op, "d": d})
	if err != nil {
		return err
	}
	return conn.WriteText(b)
}

func sendDiscordIdentify(conn *ws.WSConn, tok string) error {
	return sendGatewayFrame(conn, 2, map[string]any{
		"token":   tok,
		"intents": discordGatewayIntents,
		"properties": map[string]string{
			"os":      "darwin",
			"browser": "fleet",
			"device":  "fleet-lighthouse",
		},
	})
}

func runDiscordHeartbeat(ctx context.Context, conn *ws.WSConn, interval time.Duration, seqMu *sync.Mutex, lastSeq **int64) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			seqMu.Lock()
			s := *lastSeq
			seqMu.Unlock()
			if err := sendGatewayFrame(conn, 1, s); err != nil {
				_ = conn.Close()
				return
			}
		}
	}
}

func (e *Engine) handleDiscordDispatch(ctx context.Context, tok string, cfg DiscordConfig, eventType string, raw json.RawMessage) {
	switch eventType {
	case "READY":
		var ready struct {
			User discordUser `json:"user"`
		}
		if json.Unmarshal(raw, &ready) == nil {
			e.setDiscordState(true, ready.User.ID)
			go e.catchupDiscordChannels(ctx, cfg, tok)
		}
	case "MESSAGE_CREATE":
		var msg discordMessage
		if json.Unmarshal(raw, &msg) == nil {
			e.handleDiscordMessageCreate(ctx, tok, cfg, msg)
		}
	case "INTERACTION_CREATE":
		var inter discordInteraction
		if json.Unmarshal(raw, &inter) == nil {
			e.handleDiscordInteraction(ctx, tok, cfg, inter)
		}
	}
}

func findFleetChannelForDiscord(cfg DiscordConfig, discordChannelID string) (string, bool) {
	for fleetCh, discID := range cfg.Channels {
		if discID == discordChannelID {
			return fleetCh, true
		}
	}
	return "", false
}

func (e *Engine) catchupDiscordChannels(ctx context.Context, cfg DiscordConfig, tok string) {
	api := e.discordClient()
	for fleetCh, discID := range cfg.Channels {
		cursor, err := e.store.GetDiscordCursor(fleetCh)
		if err != nil || cursor == "" {
			continue
		}
		msgs, err := api.GetChannelMessagesAfter(ctx, tok, discID, cursor)
		if err != nil {
			continue
		}
		for _, msg := range msgs {
			e.handleDiscordMessageCreate(ctx, tok, cfg, msg)
		}
	}
}

func (e *Engine) handleDiscordMessageCreate(ctx context.Context, tok string, cfg DiscordConfig, msg discordMessage) {
	e.discordMu.RLock()
	botID := e.discordBotUserID
	e.discordMu.RUnlock()
	if msg.Author.Bot || (botID != "" && msg.Author.ID == botID) || msg.Author.ID != cfg.AuthorizedUserID {
		return
	}
	fleetCh, ok := findFleetChannelForDiscord(cfg, msg.ChannelID)
	if !ok {
		return
	}
	_ = e.store.SetDiscordCursor(fleetCh, msg.ChannelID, msg.ID)
	content := strings.TrimSpace(msg.Content)
	if content == "" {
		return
	}
	if strings.EqualFold(content, "/new") || strings.EqualFold(content, "/reset") {
		if _, err := e.resetChannelInternal(ctx, fleetCh, false, "discord"); err == nil {
			_, _ = e.discordClient().CreateMessage(ctx, tok, msg.ChannelID, map[string]any{
				"content": "── *New context started (long-term memory preserved)* ──",
			})
		}
		return
	}
	if _, _, inserted, err := e.EnqueueExternalMessage(fleetCh, content, "discord", msg.ID); err == nil && inserted {
		go e.discordClient().TriggerTyping(context.WithoutCancel(ctx), tok, msg.ChannelID)
	}
}

func (e *Engine) handleDiscordInteraction(ctx context.Context, tok string, cfg DiscordConfig, inter discordInteraction) {
	if inter.Type != 3 {
		return
	}
	actorID := inter.User.ID
	if actorID == "" && inter.Member != nil {
		actorID = inter.Member.User.ID
	}
	api := e.discordClient()
	if actorID == "" || actorID != cfg.AuthorizedUserID {
		_ = api.RespondInteraction(ctx, tok, inter.ID, inter.Token, map[string]any{
			"type": 4,
			"data": map[string]any{"content": "Unauthorized Discord user.", "flags": 64},
		})
		return
	}
	if cfg.AllowApprovals != nil && !*cfg.AllowApprovals {
		_ = api.RespondInteraction(ctx, tok, inter.ID, inter.Token, map[string]any{
			"type": 4,
			"data": map[string]any{"content": "Discord approvals are disabled; decide in Fleet Console.", "flags": 64},
		})
		return
	}
	rest, ok := strings.CutPrefix(inter.Data.CustomID, "fleet_apr:")
	if !ok {
		return
	}
	action, aprID, ok := strings.Cut(rest, ":")
	if !ok || (action != "approve" && action != "deny") || aprID == "" {
		return
	}
	apr, err := e.DecideApproval(aprID, action == "approve", "decided via Discord")
	if err != nil {
		existing, gErr := e.store.GetApproval(aprID)
		if gErr != nil || existing == nil {
			_ = api.RespondInteraction(ctx, tok, inter.ID, inter.Token, map[string]any{
				"type": 4,
				"data": map[string]any{"content": "Approval lookup failed: " + err.Error(), "flags": 64},
			})
			return
		}
		apr = *existing
	}
	deepLink := buildFleetDeepLink(cfg.FleetBaseURL, apr.ChannelID)
	_ = api.RespondInteraction(ctx, tok, inter.ID, inter.Token, map[string]any{
		"type": 7,
		"data": buildDecidedApprovalDiscordPayload(apr, deepLink),
	})
}

func splitDiscordContent(text string, maxLen int) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if maxLen <= 0 {
		maxLen = 1900
	}
	var chunks []string
	for len(text) > maxLen {
		cut := strings.LastIndex(text[:maxLen], "\n")
		if cut < maxLen/2 {
			cut = strings.LastIndex(text[:maxLen], " ")
		}
		if cut < maxLen/2 {
			cut = maxLen
		}
		chunks = append(chunks, strings.TrimSpace(text[:cut]))
		text = strings.TrimSpace(text[cut:])
	}
	if text != "" {
		chunks = append(chunks, text)
	}
	return chunks
}

func widgetToDiscordEmbed(w WidgetPayload) (map[string]any, bool) {
	if w.Kind == "html" {
		return nil, true
	}
	title := truncateText(cmp.Or(strings.TrimSpace(w.Title), strings.ToUpper(w.Kind)), 120)
	dataMap, _ := w.Data.(map[string]any)
	var desc string
	switch w.Kind {
	case "markdown":
		if s, ok := w.Data.(string); ok {
			desc = s
		} else if dataMap != nil {
			desc = fmt.Sprint(cmp.Or(dataMap["markdown"], dataMap["content"], dataMap["text"]))
		}
	case "key_value":
		var lines []string
		for k, v := range dataMap {
			lines = append(lines, fmt.Sprintf("**%s:** %v", k, v))
		}
		desc = strings.Join(lines, "\n")
	case "choices":
		if raw, ok := dataMap["choices"].([]any); ok {
			var opts []string
			for _, it := range raw {
				if m, ok := it.(map[string]any); ok {
					opts = append(opts, fmt.Sprint(cmp.Or(m["label"], m["value"])))
				} else {
					opts = append(opts, fmt.Sprint(it))
				}
			}
			desc = "Choices: " + strings.Join(opts, " · ")
		}
	case "agents":
		if raw, ok := dataMap["agents"].([]any); ok {
			var rows []string
			for _, it := range raw {
				if m, ok := it.(map[string]any); ok {
					rows = append(rows, fmt.Sprintf("`%v` (%v) — %v", m["name"], m["device"], m["state"]))
				}
			}
			desc = strings.Join(rows, "\n")
		}
	case "agenda":
		if raw, ok := dataMap["items"].([]any); ok {
			var rows []string
			for _, it := range raw {
				if m, ok := it.(map[string]any); ok {
					rows = append(rows, fmt.Sprintf("`%v` %v", m["time"], m["title"]))
				}
			}
			desc = strings.Join(rows, "\n")
		}
	}
	if strings.TrimSpace(desc) == "" {
		return nil, false
	}
	return map[string]any{
		"title":       title,
		"description": truncateText(strings.TrimSpace(desc), 1500),
		"color":       0x0284c7,
	}, false
}

func widgetsToDiscordEmbeds(widgets []WidgetPayload) ([]map[string]any, bool) {
	var embeds []map[string]any
	hasHTML := false
	for _, w := range widgets {
		emb, isHTML := widgetToDiscordEmbed(w)
		if isHTML {
			hasHTML = true
		}
		if emb != nil && len(embeds) < 5 {
			embeds = append(embeds, emb)
		}
	}
	return embeds, hasHTML
}

func buildApprovalDiscordPayload(apr Approval, deepLink string) map[string]any {
	desc := truncateText(strings.TrimSpace(apr.Summary), 350)
	if deepLink != "" {
		desc += fmt.Sprintf("\n\n[Open in Fleet Console](%s)", deepLink)
	}
	return map[string]any{
		"embeds": []map[string]any{{
			"title":       truncateText("Approval needed: "+apr.ToolName, 120),
			"description": desc,
			"color":       0xef4444,
			"fields": []map[string]any{
				{"name": "Channel", "value": "#" + apr.ChannelID, "inline": true},
				{"name": "Risk", "value": cmp.Or(apr.RiskLevel, "write"), "inline": true},
			},
			"footer":    map[string]string{"text": "Fleet Assistant · ID " + apr.ID},
			"timestamp": time.Now().UTC().Format(time.RFC3339),
		}},
		"components": []map[string]any{{
			"type": 1,
			"components": []map[string]any{
				{"type": 2, "style": 3, "label": "Approve", "custom_id": "fleet_apr:approve:" + apr.ID},
				{"type": 2, "style": 4, "label": "Deny", "custom_id": "fleet_apr:deny:" + apr.ID},
			},
		}},
	}
}

func buildDecidedApprovalDiscordPayload(apr Approval, deepLink string) map[string]any {
	approved := apr.Status == "approved" || apr.Status == "executed"
	prefix := "❌ Denied: "
	color := 0x64748b
	if approved {
		prefix = "✅ Approved: "
		color = 0x22c55e
	}
	desc := truncateText(strings.TrimSpace(apr.Summary), 350)
	if apr.DecisionNote != "" {
		desc += "\n\n*" + truncateText(apr.DecisionNote, 120) + "*"
	}
	if deepLink != "" {
		desc += fmt.Sprintf("\n\n[Open in Fleet Console](%s)", deepLink)
	}
	return map[string]any{
		"embeds": []map[string]any{{
			"title":       truncateText(prefix+apr.ToolName, 120),
			"description": desc,
			"color":       color,
			"fields": []map[string]any{
				{"name": "Channel", "value": "#" + apr.ChannelID, "inline": true},
				{"name": "Status", "value": apr.Status, "inline": true},
			},
			"footer":    map[string]string{"text": "Fleet Assistant · ID " + apr.ID},
			"timestamp": time.Now().UTC().Format(time.RFC3339),
		}},
		"components": []any{},
	}
}

func (e *Engine) mirrorTurnToDiscord(ctx context.Context, cfg Config, ch Channel, opts turnOptions, content string, widgets []WidgetPayload) {
	if !cfg.Discord.Enabled {
		return
	}
	discChannelID := cfg.Discord.Channels[ch.ID]
	if discChannelID == "" {
		return
	}
	isDiscordOrigin := opts.TriggerSource == "discord"
	if !isDiscordOrigin && cfg.Discord.MirrorWebTurns != nil && !*cfg.Discord.MirrorWebTurns {
		return
	}
	tok, err := e.resolveDiscordBotToken(ctx, cfg.Discord)
	if err != nil {
		return
	}
	var out strings.Builder
	if !isDiscordOrigin && strings.TrimSpace(opts.TriggerUserText) != "" {
		label := "You (Fleet Web)"
		if opts.TriggerSource != "chat" && opts.TriggerSource != "web" && opts.TriggerSource != "" {
			label = "System (" + opts.TriggerSource + ")"
		}
		fmt.Fprintf(&out, "> **%s:** %s\n\n", label, truncateText(strings.ReplaceAll(strings.TrimSpace(opts.TriggerUserText), "\n", " "), 240))
	}
	out.WriteString(strings.TrimSpace(content))
	embeds, hasHTML := widgetsToDiscordEmbeds(widgets)
	if hasHTML {
		deepLink := buildFleetDeepLink(cfg.Discord.FleetBaseURL, ch.ID)
		fmt.Fprintf(&out, "\n\n*Interactive widget attached — [Open in Fleet Console](%s)*", deepLink)
	}
	chunks := splitDiscordContent(out.String(), 1900)
	if len(chunks) == 0 && len(embeds) > 0 {
		chunks = []string{""}
	}
	api := e.discordClient()
	for i, chunk := range chunks {
		payload := map[string]any{"content": chunk}
		if i == 0 && opts.ReplyToExternalID != "" {
			payload["message_reference"] = map[string]string{"message_id": opts.ReplyToExternalID}
		}
		if i == len(chunks)-1 && len(embeds) > 0 {
			payload["embeds"] = embeds
		}
		if msgID, pErr := api.CreateMessage(ctx, tok, discChannelID, payload); pErr == nil && msgID != "" {
			_ = e.store.SetDiscordCursor(ch.ID, discChannelID, msgID)
		}
	}
}

func (e *Engine) sendApprovalToDiscord(cfg Config, ch Channel, apr Approval) bool {
	if !cfg.Discord.Enabled {
		return false
	}
	discChannelID := cfg.Discord.Channels[ch.ID]
	if discChannelID == "" || (cfg.Discord.AllowApprovals != nil && !*cfg.Discord.AllowApprovals) {
		return false
	}
	tok, err := e.resolveDiscordBotToken(context.Background(), cfg.Discord)
	if err != nil {
		return false
	}
	deepLink := buildFleetDeepLink(cfg.Discord.FleetBaseURL, ch.ID)
	payload := buildApprovalDiscordPayload(apr, deepLink)
	msgID, err := e.discordClient().CreateMessage(context.Background(), tok, discChannelID, payload)
	if err != nil {
		return false
	}
	if msgID != "" {
		_ = e.store.SetDiscordCursor(ch.ID, discChannelID, msgID)
	}
	return true
}

func (e *Engine) postDiscordDivider(ctx context.Context, cfg Config, channelID, text string) {
	if !cfg.Discord.Enabled {
		return
	}
	discChannelID := cfg.Discord.Channels[channelID]
	if discChannelID == "" {
		return
	}
	tok, err := e.resolveDiscordBotToken(ctx, cfg.Discord)
	if err != nil {
		return
	}
	if msgID, pErr := e.discordClient().CreateMessage(ctx, tok, discChannelID, map[string]any{"content": text}); pErr == nil && msgID != "" {
		_ = e.store.SetDiscordCursor(channelID, discChannelID, msgID)
	}
}
