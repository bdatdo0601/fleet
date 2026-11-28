package assistant

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
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

	defaultDiscordMaxUploadBytes int64 = 10 * 1024 * 1024
	discordUploadSafetyMargin    int64 = 256 * 1024
	discordMaxFilesPerMessage          = 10
)

type discordAPI struct {
	baseURL      string
	httpClient   *http.Client
	uploadClient *http.Client
}

type discordUser struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Bot      bool   `json:"bot"`
}

type discordGuild struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type discordChannel struct {
	ID       string `json:"id"`
	GuildID  string `json:"guild_id"`
	Type     int    `json:"type"`
	Name     string `json:"name"`
	ParentID string `json:"parent_id"`
	Topic    string `json:"topic"`
}

// DiscordSyncResult summarizes automatic Fleet <-> Discord channel provisioning.
type DiscordSyncResult struct {
	GuildID          string            `json:"guild_id"`
	CategoryID       string            `json:"category_id,omitempty"`
	CategoryName     string            `json:"category_name,omitempty"`
	CreatedInDiscord []string          `json:"created_in_discord,omitempty"`
	AdoptedInDiscord []string          `json:"adopted_in_discord,omitempty"`
	CreatedInFleet   []string          `json:"created_in_fleet,omitempty"`
	Channels         map[string]string `json:"channels"`
}

type discordAttachment struct {
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type,omitempty"`
	Size        int64  `json:"size,omitempty"`
	URL         string `json:"url"`
}

type discordUploadFile struct {
	ID       string
	Name     string
	MimeType string
	Size     int64
	Data     []byte
}

type discordFileRef struct {
	ID   string
	Name string
	Size int64
}

type discordMessage struct {
	ID          string              `json:"id"`
	ChannelID   string              `json:"channel_id"`
	Content     string              `json:"content"`
	Author      discordUser         `json:"author"`
	Attachments []discordAttachment `json:"attachments,omitempty"`
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
		baseURL:      strings.TrimRight(cmp.Or(strings.TrimSpace(baseURL), defaultDiscordAPIBase), "/"),
		httpClient:   &http.Client{Timeout: 15 * time.Second},
		uploadClient: &http.Client{Timeout: 60 * time.Second},
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

func (d *discordAPI) CreateMessageWithFiles(ctx context.Context, token, channelID string, payload map[string]any, files []discordUploadFile) (string, error) {
	if len(files) == 0 {
		return d.CreateMessage(ctx, token, channelID, payload)
	}
	payloadCopy := maps.Clone(payload)
	if payloadCopy == nil {
		payloadCopy = make(map[string]any)
	}
	attMeta := make([]map[string]any, 0, len(files))
	for i, f := range files {
		attMeta = append(attMeta, map[string]any{
			"id":       i,
			"filename": f.Name,
		})
	}
	payloadCopy["attachments"] = attMeta
	payloadBytes, err := json.Marshal(payloadCopy)
	if err != nil {
		return "", err
	}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)

	jsonHdr := textproto.MIMEHeader{}
	jsonHdr.Set("Content-Disposition", `form-data; name="payload_json"`)
	jsonHdr.Set("Content-Type", "application/json")
	jsonPart, err := mw.CreatePart(jsonHdr)
	if err != nil {
		return "", err
	}
	if _, err := jsonPart.Write(payloadBytes); err != nil {
		return "", err
	}

	for i, f := range files {
		fileHdr := textproto.MIMEHeader{}
		fileHdr.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{
			"name":     fmt.Sprintf("files[%d]", i),
			"filename": f.Name,
		}))
		fileHdr.Set("Content-Type", cmp.Or(strings.TrimSpace(f.MimeType), "application/octet-stream"))
		part, pErr := mw.CreatePart(fileHdr)
		if pErr != nil {
			return "", pErr
		}
		if _, wErr := part.Write(f.Data); wErr != nil {
			return "", wErr
		}
	}
	if err := mw.Close(); err != nil {
		return "", err
	}

	path := "/channels/" + url.PathEscape(channelID) + "/messages"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.baseURL+path, &body)
	if err != nil {
		return "", errors.New(scrubSecret(err.Error(), token))
	}
	req.Header.Set("User-Agent", "DiscordBot (https://github.com/bdatdo0601/fleet, 1.0)")
	if token != "" {
		req.Header.Set("Authorization", "Bot "+token)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	client := d.uploadClient
	if client == nil {
		client = d.httpClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", errors.New(scrubSecret(err.Error(), token))
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil {
		return "", errors.New(scrubSecret(err.Error(), token))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", errors.New(scrubSecret(fmt.Sprintf("discord API POST %s HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(raw))), token))
	}
	var res struct {
		ID string `json:"id"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &res); err != nil {
			return "", err
		}
	}
	return res.ID, nil
}

func (d *discordAPI) TriggerTyping(ctx context.Context, token, channelID string) {
	_ = d.doJSON(ctx, token, http.MethodPost, "/channels/"+url.PathEscape(channelID)+"/typing", nil, nil)
}

func (d *discordAPI) RespondInteraction(ctx context.Context, token, interactionID, interactionToken string, payload map[string]any) error {
	path := fmt.Sprintf("/interactions/%s/%s/callback", url.PathEscape(interactionID), url.PathEscape(interactionToken))
	return d.doJSON(ctx, token, http.MethodPost, path, payload, nil)
}

func (d *discordAPI) GetCurrentUserGuilds(ctx context.Context, token string) ([]discordGuild, error) {
	var guilds []discordGuild
	err := d.doJSON(ctx, token, http.MethodGet, "/users/@me/guilds", nil, &guilds)
	return guilds, err
}

func (d *discordAPI) GetGuildChannels(ctx context.Context, token, guildID string) ([]discordChannel, error) {
	var channels []discordChannel
	err := d.doJSON(ctx, token, http.MethodGet, "/guilds/"+url.PathEscape(guildID)+"/channels", nil, &channels)
	return channels, err
}

func (d *discordAPI) CreateGuildChannel(ctx context.Context, token, guildID string, payload map[string]any) (discordChannel, error) {
	var ch discordChannel
	err := d.doJSON(ctx, token, http.MethodPost, "/guilds/"+url.PathEscape(guildID)+"/channels", payload, &ch)
	return ch, err
}

func (e *Engine) discordClient() *discordAPI {
	return newDiscordAPI(e.discordAPIBase)
}

func (e *Engine) resolveDiscordBotToken(ctx context.Context, cfg DiscordConfig) (string, error) {
	if !strings.HasPrefix(strings.TrimSpace(cfg.BotTokenOPRef), "op://") {
		return "", newErr("discord.bot_token_op_ref must be an op:// reference", http.StatusBadRequest)
	}
	reader := e.getOPReader()
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
		// Drain reload signals emitted before reading the snapshot below so a
		// pre-dial UpdateConfig does not tear down the freshly dialed session.
		select {
		case <-e.discordReloadCh:
		default:
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
	var sessWg sync.WaitGroup
	sessWg.Go(func() {
		select {
		case <-sessCtx.Done():
			_ = conn.Close()
		case <-e.discordReloadCh:
			_ = conn.Close()
		}
	})
	defer func() {
		cancelSess()
		_ = conn.Close()
		sessWg.Wait()
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
		e.trackWG(func() {
			runDiscordHeartbeat(hbCtx, conn, interval, seqMu, lastSeq)
		})
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

func (e *Engine) handleDiscordDispatch(ctx context.Context, tok string, _ DiscordConfig, eventType string, raw json.RawMessage) {
	liveCfg := normalizeDiscordConfig(e.Config().Discord)
	switch eventType {
	case "READY":
		var ready struct {
			User discordUser `json:"user"`
		}
		if json.Unmarshal(raw, &ready) == nil {
			e.setDiscordState(true, ready.User.ID)
			e.trackWG(func() {
				e.handleDiscordReady(ctx, liveCfg, tok)
			})
		}
	case "CHANNEL_CREATE":
		var dc discordChannel
		if json.Unmarshal(raw, &dc) == nil {
			e.handleDiscordChannelCreate(ctx, liveCfg, dc)
		}
	case "MESSAGE_CREATE":
		var msg discordMessage
		if json.Unmarshal(raw, &msg) == nil {
			e.handleDiscordMessageCreate(ctx, tok, liveCfg, msg)
		}
	case "INTERACTION_CREATE":
		var inter discordInteraction
		if json.Unmarshal(raw, &inter) == nil {
			e.handleDiscordInteraction(ctx, tok, liveCfg, inter)
		}
	}
}

func (e *Engine) handleDiscordReady(ctx context.Context, cfg DiscordConfig, tok string) {
	if cfg.AutoSyncChannels == nil || *cfg.AutoSyncChannels {
		syncCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		_, _ = e.SyncDiscordChannels(syncCtx)
		cancel()
		cfg = normalizeDiscordConfig(e.Config().Discord)
	}
	e.catchupDiscordChannels(ctx, cfg, tok)
}

func (e *Engine) handleDiscordChannelCreate(ctx context.Context, cfg DiscordConfig, dc discordChannel) {
	if dc.Type != 0 || dc.ParentID == "" || (cfg.AutoSyncChannels != nil && !*cfg.AutoSyncChannels) {
		return
	}
	for _, mappedID := range cfg.Channels {
		if mappedID == dc.ID {
			return
		}
	}
	e.discordMu.RLock()
	catID := e.discordCategoryID
	e.discordMu.RUnlock()
	if catID != "" && dc.ParentID != catID {
		return
	}
	e.spawnBG(ctx, 15*time.Second, func(syncCtx context.Context) {
		_, _ = e.SyncDiscordChannels(syncCtx)
	})
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

func isAllowedDiscordAttachmentURL(rawURL, apiBase string) bool {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Hostname() == "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if u.Scheme == "https" && (host == "cdn.discordapp.com" || host == "media.discordapp.net" || host == "attachments.discordapp.net") {
		return true
	}
	if trimmedBase := strings.TrimSpace(apiBase); trimmedBase != "" {
		if base, bErr := url.Parse(trimmedBase); bErr == nil && base.Scheme == u.Scheme && strings.EqualFold(base.Host, u.Host) {
			return true
		}
	}
	return false
}

func (e *Engine) downloadOneDiscordAttachment(ctx context.Context, client *http.Client, fleetCh, apiBase string, att discordAttachment, maxBytes int64) (string, bool) {
	if !isAllowedDiscordAttachmentURL(att.URL, apiBase) || att.Size > maxBytes {
		return "", false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, att.URL, nil)
	if err != nil {
		return "", false
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || resp.ContentLength > maxBytes {
		return "", false
	}
	name := cmp.Or(strings.TrimSpace(att.Filename), "attachment.bin")
	ct := cmp.Or(strings.TrimSpace(att.ContentType), resp.Header.Get("Content-Type"))
	rec, err := e.SaveUploadedFile(fleetCh, name, ct, io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return "", false
	}
	return rec.ID, true
}

func (e *Engine) downloadDiscordAttachments(ctx context.Context, fleetCh string, atts []discordAttachment) []string {
	if len(atts) == 0 {
		return nil
	}
	apiBase := e.discordAPIBase
	client := &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if !isAllowedDiscordAttachmentURL(req.URL.String(), apiBase) {
				return errors.New("disallowed redirect target")
			}
			return nil
		},
	}
	maxBytes := e.maxSandboxFileBytes()
	var ids []string
	for _, att := range atts {
		if id, ok := e.downloadOneDiscordAttachment(ctx, client, fleetCh, apiBase, att, maxBytes); ok {
			ids = append(ids, id)
		}
	}
	return ids
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
	if content == "" && len(msg.Attachments) == 0 {
		return
	}
	if len(msg.Attachments) == 0 && e.handleDiscordSlashCommand(ctx, tok, fleetCh, msg.ChannelID, content) {
		return
	}
	if msg.ID != "" && e.store.HasExternalMessage("discord", msg.ID) {
		return
	}
	var fileIDs []string
	if len(msg.Attachments) > 0 {
		fileIDs = e.downloadDiscordAttachments(ctx, fleetCh, msg.Attachments)
	}
	if content == "" && len(fileIDs) == 0 {
		return
	}
	if _, _, inserted, err := e.EnqueueExternalMessage(fleetCh, content, "discord", msg.ID, fileIDs); err == nil && inserted {
		e.spawnBG(ctx, 5*time.Second, func(tCtx context.Context) {
			e.discordClient().TriggerTyping(tCtx, tok, msg.ChannelID)
		})
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
	case "mcp_oauth":
		return map[string]any{
			"title":       title,
			"description": truncateText(mcpOAuthEmbedDesc(dataMap), 1500),
			"color":       0x0284c7,
		}, true
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

func mcpOAuthEmbedDesc(dataMap map[string]any) string {
	srvName := fmt.Sprint(cmp.Or(dataMap["server"], "mcp"))
	provider := fmt.Sprint(cmp.Or(dataMap["provider"], "oauth2"))
	status := "Not connected"
	if auth, _ := dataMap["authorized"].(bool); auth {
		status = "Authorized"
	}
	lines := []string{
		fmt.Sprintf("**Server:** `%s` (%s)", srvName, provider),
		fmt.Sprintf("**Status:** %s", status),
	}
	if scopes := stringListFromAny(dataMap["scopes"]); len(scopes) > 0 {
		lines = append(lines, fmt.Sprintf("**Scopes:** %s", strings.Join(scopes, ", ")))
	}
	if cliCmd := fmt.Sprint(cmp.Or(dataMap["cli_command"], "")); cliCmd != "" {
		lines = append(lines, fmt.Sprintf("**CLI:** `%s`", cliCmd))
	}
	return strings.Join(lines, "\n")
}

func stringListFromAny(v any) []string {
	switch raw := v.(type) {
	case []string:
		return raw
	case []any:
		out := make([]string, 0, len(raw))
		for _, it := range raw {
			out = append(out, fmt.Sprint(it))
		}
		return out
	default:
		return nil
	}
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

func (e *Engine) discordUploadLimit() int64 {
	if e.discordMaxUploadBytes > 0 {
		return e.discordMaxUploadBytes
	}
	return defaultDiscordMaxUploadBytes
}

func extractWidgetFileRefs(widgets []WidgetPayload) []discordFileRef {
	seen := make(map[string]struct{})
	var refs []discordFileRef
	for _, w := range widgets {
		if w.Kind != "files" {
			continue
		}
		dataMap, ok := w.Data.(map[string]any)
		if !ok {
			continue
		}
		rawFiles, ok := dataMap["files"].([]any)
		if !ok {
			continue
		}
		for _, item := range rawFiles {
			fm, ok := item.(map[string]any)
			if !ok {
				continue
			}
			id, _ := fm["id"].(string)
			id = strings.TrimSpace(id)
			if id == "" {
				continue
			}
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
			name, _ := fm["name"].(string)
			var size int64
			switch v := fm["size_bytes"].(type) {
			case float64:
				size = int64(v)
			case int64:
				size = v
			case int:
				size = int64(v)
			}
			refs = append(refs, discordFileRef{ID: id, Name: name, Size: size})
		}
	}
	return refs
}

func (e *Engine) readDiscordUploadFile(ref discordFileRef, remainingBudget int64) (discordUploadFile, string, bool) {
	f, fh, err := e.OpenStoredFile(ref.ID)
	if err != nil {
		return discordUploadFile{}, cmp.Or(ref.Name, ref.ID), false
	}
	defer func() { _ = fh.Close() }()
	size := f.SizeBytes
	if size <= 0 {
		if st, sErr := fh.Stat(); sErr == nil {
			size = st.Size()
		}
	}
	label := fmt.Sprintf("%s (%s)", f.Name, formatByteSize(size))
	if remainingBudget <= 0 || size > remainingBudget {
		return discordUploadFile{}, label, false
	}
	data, err := io.ReadAll(io.LimitReader(fh, remainingBudget+1))
	if err != nil || int64(len(data)) > remainingBudget {
		return discordUploadFile{}, label, false
	}
	return discordUploadFile{
		ID:       f.ID,
		Name:     f.Name,
		MimeType: f.MimeType,
		Size:     int64(len(data)),
		Data:     data,
	}, "", true
}

func (e *Engine) collectDiscordFiles(widgets []WidgetPayload) ([]discordUploadFile, []string) {
	refs := extractWidgetFileRefs(widgets)
	if len(refs) == 0 {
		return nil, nil
	}
	limit := e.discordUploadLimit()
	budget := limit
	if limit > discordUploadSafetyMargin {
		budget = limit - discordUploadSafetyMargin
	}
	var totalBytes int64
	var uploads []discordUploadFile
	var skipped []string
	for _, ref := range refs {
		if len(uploads) >= discordMaxFilesPerMessage {
			label := cmp.Or(ref.Name, ref.ID)
			if ref.Size > 0 {
				label = fmt.Sprintf("%s (%s)", label, formatByteSize(ref.Size))
			}
			skipped = append(skipped, label)
			continue
		}
		uf, skipLabel, ok := e.readDiscordUploadFile(ref, budget-totalBytes)
		if !ok {
			skipped = append(skipped, skipLabel)
			continue
		}
		uploads = append(uploads, uf)
		totalBytes += uf.Size
	}
	return uploads, skipped
}

func buildDiscordAttachmentFallbackPayload(payload map[string]any, files []discordUploadFile, deepLink string) map[string]any {
	fb := maps.Clone(payload)
	if fb == nil {
		fb = make(map[string]any)
	}
	delete(fb, "attachments")
	labels := make([]string, 0, len(files))
	for _, f := range files {
		labels = append(labels, fmt.Sprintf("%s (%s)", f.Name, formatByteSize(f.Size)))
	}
	note := fmt.Sprintf("\n\n*Could not attach file(s) to Discord (%s) — [Open in Fleet Console](%s)*", strings.Join(labels, ", "), deepLink)
	base, _ := fb["content"].(string)
	if maxBase := 2000 - len(note) - 3; maxBase <= 0 {
		fb["content"] = truncateText(strings.TrimSpace(note), 1995)
	} else {
		if len(base) > maxBase {
			base = truncateText(base, maxBase)
		}
		fb["content"] = strings.TrimSpace(base + note)
	}
	return fb
}

func buildMirrorDiscordBody(opts turnOptions, content string, widgets []WidgetPayload) (string, []map[string]any, bool) {
	isDiscordOrigin := opts.TriggerSource == "discord"
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
	return out.String(), embeds, hasHTML
}

func (e *Engine) sendDiscordMirrorChunk(ctx context.Context, api *discordAPI, tok, discChannelID, deepLink string, payload map[string]any, attachments []discordUploadFile) (string, error) {
	if len(attachments) == 0 {
		return api.CreateMessage(ctx, tok, discChannelID, payload)
	}
	msgID, err := api.CreateMessageWithFiles(ctx, tok, discChannelID, payload, attachments)
	if err != nil {
		return api.CreateMessage(ctx, tok, discChannelID, buildDiscordAttachmentFallbackPayload(payload, attachments, deepLink))
	}
	return msgID, nil
}

func (e *Engine) mirrorTurnToDiscord(ctx context.Context, cfg Config, ch Channel, opts turnOptions, content string, widgets []WidgetPayload) {
	if !cfg.Discord.Enabled {
		return
	}
	discChannelID := cfg.Discord.Channels[ch.ID]
	if discChannelID == "" {
		return
	}
	if opts.TriggerSource != "discord" && cfg.Discord.MirrorWebTurns != nil && !*cfg.Discord.MirrorWebTurns {
		return
	}
	tok, err := e.resolveDiscordBotToken(ctx, cfg.Discord)
	if err != nil {
		return
	}
	bodyText, embeds, hasHTML := buildMirrorDiscordBody(opts, content, widgets)
	attachments, skippedFiles := e.collectDiscordFiles(widgets)
	var out strings.Builder
	out.WriteString(bodyText)
	deepLink := ""
	if hasHTML || len(skippedFiles) > 0 || len(attachments) > 0 {
		deepLink = buildFleetDeepLink(cfg.Discord.FleetBaseURL, ch.ID)
	}
	if hasHTML {
		fmt.Fprintf(&out, "\n\n*Interactive widget attached — [Open in Fleet Console](%s)*", deepLink)
	}
	if len(skippedFiles) > 0 {
		fmt.Fprintf(&out, "\n\n*File(s) available in Fleet Console (%s) — [Open in Fleet Console](%s)*", strings.Join(skippedFiles, ", "), deepLink)
	}
	chunks := splitDiscordContent(out.String(), 1900)
	if len(chunks) == 0 && (len(embeds) > 0 || len(attachments) > 0) {
		chunks = []string{""}
	}
	api := e.discordClient()
	for i, chunk := range chunks {
		payload := map[string]any{"content": chunk}
		if i == 0 && opts.ReplyToExternalID != "" {
			payload["message_reference"] = map[string]string{"message_id": opts.ReplyToExternalID}
		}
		var chunkAtts []discordUploadFile
		if i == len(chunks)-1 {
			if len(embeds) > 0 {
				payload["embeds"] = embeds
			}
			chunkAtts = attachments
		}
		if msgID, pErr := e.sendDiscordMirrorChunk(ctx, api, tok, discChannelID, deepLink, payload, chunkAtts); pErr == nil && msgID != "" {
			_ = e.store.SetDiscordCursor(ch.ID, discChannelID, msgID)
		}
	}
}

func (e *Engine) sendApprovalToDiscord(ctx context.Context, cfg Config, ch Channel, apr Approval) bool {
	if !cfg.Discord.Enabled {
		return false
	}
	discChannelID := cfg.Discord.Channels[ch.ID]
	if discChannelID == "" || (cfg.Discord.AllowApprovals != nil && !*cfg.Discord.AllowApprovals) {
		return false
	}
	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	tok, err := e.resolveDiscordBotToken(reqCtx, cfg.Discord)
	if err != nil {
		return false
	}
	deepLink := buildFleetDeepLink(cfg.Discord.FleetBaseURL, ch.ID)
	payload := buildApprovalDiscordPayload(apr, deepLink)
	msgID, err := e.discordClient().CreateMessage(reqCtx, tok, discChannelID, payload)
	if err != nil {
		return false
	}
	if msgID != "" {
		_ = e.store.SetDiscordCursor(ch.ID, discChannelID, msgID)
	}
	return true
}

func buildOutboxDiscordPayload(item OutboxItem, deepLink string) map[string]any {
	desc := truncateText(strings.TrimSpace(item.Body), 1500)
	if deepLink != "" {
		if desc != "" {
			desc += "\n\n"
		}
		desc += fmt.Sprintf("[Open in Fleet Console](%s)", deepLink)
	}
	return map[string]any{
		"embeds": []map[string]any{{
			"title":       truncateText(cmp.Or(strings.TrimSpace(item.Title), "Assistant Notification"), 120),
			"description": desc,
			"url":         deepLink,
			"color":       discordEmbedColor(Notification{Category: "outbox", Priority: item.Priority}),
			"fields": []map[string]any{
				{"name": "Channel", "value": "#" + item.ChannelID, "inline": true},
				{"name": "Priority", "value": cmp.Or(item.Priority, "normal"), "inline": true},
			},
			"footer":    map[string]string{"text": "Fleet Assistant · Proactive alert"},
			"timestamp": time.Now().UTC().Format(time.RFC3339),
		}},
	}
}

func (e *Engine) sendOutboxToDiscord(ctx context.Context, cfg Config, item OutboxItem) bool {
	if !cfg.Discord.Enabled {
		return false
	}
	discChannelID := cfg.Discord.Channels[item.ChannelID]
	if discChannelID == "" {
		return false
	}
	tok, err := e.resolveDiscordBotToken(ctx, cfg.Discord)
	if err != nil {
		return false
	}
	deepLink := buildFleetDeepLink(cfg.Discord.FleetBaseURL, item.ChannelID)
	payload := buildOutboxDiscordPayload(item, deepLink)
	msgID, err := e.discordClient().CreateMessage(ctx, tok, discChannelID, payload)
	if err != nil {
		return false
	}
	if msgID != "" {
		_ = e.store.SetDiscordCursor(item.ChannelID, discChannelID, msgID)
	}
	return true
}

func (e *Engine) handleDiscordSlashCommand(ctx context.Context, tok, fleetCh, discChannelID, content string) bool {
	if strings.EqualFold(content, "/new") || strings.EqualFold(content, "/reset") {
		if ch, err := e.resetChannelInternal(ctx, fleetCh, false, "discord"); err == nil {
			if msgID, pErr := e.discordClient().CreateMessage(ctx, tok, discChannelID, buildContextClearedDiscordPayload(ch, "discord", false)); pErr == nil && msgID != "" {
				_ = e.store.SetDiscordCursor(ch.ID, discChannelID, msgID)
			}
		}
		return true
	}
	if action, scope, matched := parseDiscordAutoApproveCmd(content); matched {
		e.handleDiscordAutoApproveCmd(ctx, tok, fleetCh, discChannelID, action, scope)
		return true
	}
	return false
}

func parseDiscordAutoApproveCmd(content string) (action, scope string, matched bool) {
	fields := strings.Fields(strings.TrimSpace(content))
	if len(fields) == 0 || !isAutoApproveDirective(strings.ToLower(strings.TrimPrefix(fields[0], "/"))) || !strings.HasPrefix(fields[0], "/") {
		return "", "", false
	}
	rest := fields[1:]
	switch len(rest) {
	case 0:
		return "status", "all", true
	case 1:
		switch strings.ToLower(rest[0]) {
		case "status":
			return "status", "all", true
		case "on", "true", "enable":
			return "on", "global", true
		case "off", "false", "disable":
			return "off", "all", true
		case "discord":
			return "status", "discord", true
		}
	case 2:
		if strings.ToLower(rest[0]) == "discord" {
			switch strings.ToLower(rest[1]) {
			case "status":
				return "status", "discord", true
			case "on", "true", "enable":
				return "on", "discord", true
			case "off", "false", "disable":
				return "off", "discord", true
			}
		}
	}
	return "", "", false
}

func boolOnOff(v bool) string {
	if v {
		return "ON (HITL bypassed)"
	}
	return "OFF (HITL required)"
}

func buildAutoApproveDiscordPayload(chID, action, scope string, view ConfigView) map[string]any {
	title := "🛡️ Auto-Approve Status"
	color := 0x0284c7
	desc := "Current Human-in-the-Loop (HITL) bypass settings."
	switch action {
	case "on":
		title = "⚡ Auto-Approve Enabled"
		color = 0xf59e0b
		if scope == "discord" {
			desc = "Discord turns now **bypass HITL approvals** automatically (Web Console turns keep existing policy)."
		} else {
			desc = "All assistant turns (Web & Discord) now **bypass HITL tool and frontier escalation approvals** automatically."
		}
	case "off":
		title = "🛡️ Auto-Approve Disabled"
		color = 0x22c55e
		if scope == "discord" {
			desc = "Discord auto-approve disabled; Discord turns follow global policy."
		} else {
			desc = "Global and Discord auto-approve disabled; sensitive tools and frontier escalations require HITL approval."
		}
	}
	desc += "\n\nCommands: `/auto-approve on|off|status`, `/auto-approve discord on|off`, or prefix a message with `/auto-approve <prompt>` (alias `/yolo`) for a single turn."
	return map[string]any{
		"embeds": []map[string]any{{
			"title":       title,
			"description": desc,
			"color":       color,
			"fields": []map[string]any{
				{"name": "Global Auto-Approve", "value": boolOnOff(view.AutoApprove), "inline": true},
				{"name": "Discord Auto-Approve", "value": boolOnOff(view.Discord.AutoApprove), "inline": true},
			},
			"footer":    map[string]string{"text": "Fleet Assistant · #" + chID},
			"timestamp": time.Now().UTC().Format(time.RFC3339),
		}},
	}
}

func (e *Engine) handleDiscordAutoApproveCmd(ctx context.Context, tok, fleetCh, discChannelID, action, scope string) {
	var view ConfigView
	var err error
	switch action {
	case "on":
		val := true
		if scope == "discord" {
			view, err = e.SetAutoApprove(nil, &val)
		} else {
			view, err = e.SetAutoApprove(&val, nil)
		}
	case "off":
		val := false
		if scope == "discord" {
			view, err = e.SetAutoApprove(nil, &val)
		} else {
			view, err = e.SetAutoApprove(&val, &val)
		}
	default:
		view = e.ConfigView()
	}
	if err != nil {
		return
	}
	payload := buildAutoApproveDiscordPayload(fleetCh, action, scope, view)
	if msgID, pErr := e.discordClient().CreateMessage(ctx, tok, discChannelID, payload); pErr == nil && msgID != "" {
		_ = e.store.SetDiscordCursor(fleetCh, discChannelID, msgID)
	}
}

func buildContextClearedDiscordPayload(ch Channel, origin string, clearHistory bool) map[string]any {
	var desc string
	switch {
	case origin == "discord":
		desc = "Conversation context was cleared in **Discord**. Messages above this note are no longer part of active context.\n\n*Discord history and long-term memory are preserved.*"
	case clearHistory:
		desc = "Conversation context and Fleet Console thread were cleared in **Fleet Console**. Messages above this note are no longer part of active context.\n\n*Discord history and long-term memory are preserved.*"
	default:
		desc = "Conversation context was cleared in **Fleet Console**. Messages above this note are no longer part of active context.\n\n*Discord history and long-term memory are preserved.*"
	}
	return map[string]any{
		"embeds": []map[string]any{{
			"title":       "🧹 Context Cleared — New Conversation",
			"description": desc,
			"color":       0x8b5cf6,
			"footer":      map[string]string{"text": fmt.Sprintf("Fleet Assistant · #%s · Context #%d", ch.ID, ch.ContextEpoch)},
			"timestamp":   time.Now().UTC().Format(time.RFC3339),
		}},
	}
}

func (e *Engine) postDiscordContextCleared(ctx context.Context, cfg Config, ch Channel, origin string, clearHistory bool) {
	if !cfg.Discord.Enabled {
		return
	}
	discChannelID := cfg.Discord.Channels[ch.ID]
	if discChannelID == "" {
		return
	}
	postCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	tok, err := e.resolveDiscordBotToken(postCtx, cfg.Discord)
	if err != nil {
		return
	}
	payload := buildContextClearedDiscordPayload(ch, origin, clearHistory)
	if msgID, pErr := e.discordClient().CreateMessage(postCtx, tok, discChannelID, payload); pErr == nil && msgID != "" {
		_ = e.store.SetDiscordCursor(ch.ID, discChannelID, msgID)
	}
}

func sanitizeFleetChannelID(name string) string {
	raw := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(name, "#")))
	var b strings.Builder
	prevDash := false
	for _, r := range raw {
		valid := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_'
		if valid {
			b.WriteRune(r)
			prevDash = false
			continue
		}
		if !prevDash && b.Len() > 0 {
			b.WriteByte('-')
			prevDash = true
		}
	}
	return strings.Trim(b.String(), "-_")
}

func resolveDiscordGuildID(ctx context.Context, api *discordAPI, tok string, cfg DiscordConfig) (string, error) {
	if cfg.GuildID != "" {
		return cfg.GuildID, nil
	}
	guilds, err := api.GetCurrentUserGuilds(ctx, tok)
	if err != nil {
		return "", err
	}
	switch len(guilds) {
	case 0:
		return "", newErr("Discord bot is not in any server yet; invite the bot to your server first", http.StatusBadRequest)
	case 1:
		return guilds[0].ID, nil
	default:
		choices := make([]string, 0, len(guilds))
		for _, g := range guilds {
			choices = append(choices, fmt.Sprintf("%s (%s)", cmp.Or(g.Name, "server"), g.ID))
		}
		return "", newErr("Discord bot is in multiple servers ("+strings.Join(choices, ", ")+"); set discord.guild_id", http.StatusBadRequest)
	}
}

func ensureDiscordCategory(ctx context.Context, api *discordAPI, tok, guildID, categoryName string, guildChannels []discordChannel) (discordChannel, error) {
	for _, ch := range guildChannels {
		if ch.Type == 4 && strings.EqualFold(strings.TrimSpace(ch.Name), categoryName) {
			return ch, nil
		}
	}
	return api.CreateGuildChannel(ctx, tok, guildID, map[string]any{
		"name": categoryName,
		"type": 4,
	})
}

func indexGuildChannels(guildChannels []discordChannel, categoryID string) (map[string]discordChannel, map[string]discordChannel) {
	byID := make(map[string]discordChannel, len(guildChannels))
	catTextBySlug := make(map[string]discordChannel)
	for _, dc := range guildChannels {
		byID[dc.ID] = dc
		if dc.Type == 0 && dc.ParentID == categoryID {
			if slug := sanitizeFleetChannelID(dc.Name); slug != "" {
				if _, exists := catTextBySlug[slug]; !exists {
					catTextBySlug[slug] = dc
				}
			}
		}
	}
	return byID, catTextBySlug
}

// SyncDiscordChannels reconciles Fleet channels and Discord server channels under the configured category.
func (e *Engine) SyncDiscordChannels(ctx context.Context) (DiscordSyncResult, error) {
	e.discordSyncMu.Lock()
	defer e.discordSyncMu.Unlock()

	cfg := normalizeDiscordConfig(e.Config().Discord)
	if !cfg.Enabled {
		return DiscordSyncResult{}, newErr("discord bot is disabled", http.StatusBadRequest)
	}
	if err := ValidateDiscordConfig(cfg); err != nil {
		return DiscordSyncResult{}, err
	}
	tok, err := e.resolveDiscordBotToken(ctx, cfg)
	if err != nil {
		return DiscordSyncResult{}, err
	}
	api := e.discordClient()
	guildID, err := resolveDiscordGuildID(ctx, api, tok, cfg)
	if err != nil {
		return DiscordSyncResult{}, err
	}
	guildChannels, err := api.GetGuildChannels(ctx, tok, guildID)
	if err != nil {
		return DiscordSyncResult{}, err
	}
	cat, err := ensureDiscordCategory(ctx, api, tok, guildID, cfg.CategoryName, guildChannels)
	if err != nil {
		return DiscordSyncResult{}, err
	}
	e.discordMu.Lock()
	e.discordCategoryID = cat.ID
	e.discordMu.Unlock()

	byID, catTextBySlug := indexGuildChannels(guildChannels, cat.ID)
	nextMap := maps.Clone(cfg.Channels)
	if nextMap == nil {
		nextMap = make(map[string]string)
	}
	res := DiscordSyncResult{
		GuildID:      guildID,
		CategoryID:   cat.ID,
		CategoryName: cmp.Or(cat.Name, cfg.CategoryName),
	}

	var sErr error
	guildChannels, sErr = e.syncFleetToDiscord(ctx, api, tok, guildID, cat.ID, byID, catTextBySlug, guildChannels, nextMap, &res)
	if sErr != nil {
		return DiscordSyncResult{}, sErr
	}
	e.syncDiscordToFleet(cat.ID, guildChannels, nextMap, &res)

	res.Channels = maps.Clone(nextMap)
	if guildID != cfg.GuildID || !maps.Equal(nextMap, cfg.Channels) {
		if pErr := e.persistDiscordMapping(guildID, nextMap); pErr != nil {
			return DiscordSyncResult{}, pErr
		}
	}
	if len(res.CreatedInFleet) > 0 {
		e.publishEvent(StreamEvent{Type: "channels_updated"})
	}
	return res, nil
}

func (e *Engine) syncFleetToDiscord(
	ctx context.Context,
	api *discordAPI,
	tok, guildID, categoryID string,
	byID, catTextBySlug map[string]discordChannel,
	guildChannels []discordChannel,
	nextMap map[string]string,
	res *DiscordSyncResult,
) ([]discordChannel, error) {
	fleetChannels, err := e.store.ListChannels()
	if err != nil {
		return nil, err
	}
	for _, fch := range fleetChannels {
		if existingID := nextMap[fch.ID]; existingID != "" {
			if _, ok := byID[existingID]; ok {
				continue
			}
		}
		if adopted, ok := catTextBySlug[fch.ID]; ok {
			nextMap[fch.ID] = adopted.ID
			res.AdoptedInDiscord = append(res.AdoptedInDiscord, fch.ID)
			continue
		}
		payload := map[string]any{
			"name":      fch.ID,
			"type":      0,
			"parent_id": categoryID,
		}
		if desc := strings.TrimSpace(fch.Description); desc != "" {
			payload["topic"] = truncateText(desc, 1024)
		}
		created, cErr := api.CreateGuildChannel(ctx, tok, guildID, payload)
		if cErr != nil {
			return nil, cErr
		}
		nextMap[fch.ID] = created.ID
		byID[created.ID] = created
		catTextBySlug[fch.ID] = created
		guildChannels = append(guildChannels, created)
		res.CreatedInDiscord = append(res.CreatedInDiscord, fch.ID)
	}
	return guildChannels, nil
}

func (e *Engine) syncDiscordToFleet(
	categoryID string,
	guildChannels []discordChannel,
	nextMap map[string]string,
	res *DiscordSyncResult,
) {
	mappedDiscordIDs := make(map[string]bool, len(nextMap))
	for _, id := range nextMap {
		mappedDiscordIDs[id] = true
	}
	for _, dc := range guildChannels {
		if dc.Type != 0 || dc.ParentID != categoryID || mappedDiscordIDs[dc.ID] {
			continue
		}
		slug := sanitizeFleetChannelID(dc.Name)
		if slug == "" || nextMap[slug] != "" {
			continue
		}
		if _, uErr := e.store.UpsertChannel(slug, "#"+slug, dc.Topic, ""); uErr != nil {
			continue
		}
		nextMap[slug] = dc.ID
		mappedDiscordIDs[dc.ID] = true
		res.CreatedInFleet = append(res.CreatedInFleet, slug)
	}
}
