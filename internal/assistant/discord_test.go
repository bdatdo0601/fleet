package assistant

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type recordedDiscordPost struct {
	ChannelID string
	Payload   map[string]any
}

type recordedDiscordInteractionResp struct {
	ID      string
	Token   string
	Payload map[string]any
}

type mockDiscordServer struct {
	t            *testing.T
	httpSrv      *httptest.Server
	mu           sync.Mutex
	conn         net.Conn
	bw           *bufio.Writer
	identified   chan string
	posts        []recordedDiscordPost
	postCh       chan recordedDiscordPost
	interactions []recordedDiscordInteractionResp
	interCh      chan recordedDiscordInteractionResp
	meStatus     int
	meBody       string
	botUserID    string
	msgSeq       atomic.Int64
	guilds       []discordGuild
	channels     []discordChannel
	nextChID     atomic.Int64
}

func newMockDiscordServer(t *testing.T) *mockDiscordServer {
	t.Helper()
	m := &mockDiscordServer{
		t:          t,
		identified: make(chan string, 4),
		postCh:     make(chan recordedDiscordPost, 32),
		interCh:    make(chan recordedDiscordInteractionResp, 16),
		meStatus:   http.StatusOK,
		meBody:     `{"id":"999999999999999999","username":"FleetBot","bot":true}`,
		botUserID:  "999999999999999999",
	}
	m.httpSrv = httptest.NewServer(http.HandlerFunc(m.handleHTTP))
	return m
}

func (m *mockDiscordServer) Close() {
	m.mu.Lock()
	if m.conn != nil {
		_ = m.conn.Close()
		m.conn = nil
	}
	m.mu.Unlock()
	m.httpSrv.Close()
}

func (m *mockDiscordServer) WSURL() string {
	return "ws://" + m.httpSrv.Listener.Addr().String() + "/gateway?v=10&encoding=json"
}

func (m *mockDiscordServer) APIURL() string {
	return m.httpSrv.URL
}

func (m *mockDiscordServer) handleHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if path == "/gateway" {
		m.handleGatewayWS(w, r)
		return
	}
	switch {
	case path == "/users/@me" && r.Method == http.MethodGet:
		m.mu.Lock()
		st, body := m.meStatus, m.meBody
		m.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(st)
		_, _ = io.WriteString(w, body)
	case path == "/users/@me/guilds" && r.Method == http.MethodGet:
		m.mu.Lock()
		guilds := append([]discordGuild(nil), m.guilds...)
		m.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(guilds)
	case strings.HasPrefix(path, "/guilds/") && strings.HasSuffix(path, "/channels") && r.Method == http.MethodGet:
		guildID := strings.TrimSuffix(strings.TrimPrefix(path, "/guilds/"), "/channels")
		m.mu.Lock()
		var out []discordChannel
		for _, ch := range m.channels {
			if ch.GuildID == "" || ch.GuildID == guildID {
				out = append(out, ch)
			}
		}
		m.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	case strings.HasPrefix(path, "/guilds/") && strings.HasSuffix(path, "/channels") && r.Method == http.MethodPost:
		guildID := strings.TrimSuffix(strings.TrimPrefix(path, "/guilds/"), "/channels")
		var req struct {
			Name     string `json:"name"`
			Type     int    `json:"type"`
			ParentID string `json:"parent_id"`
			Topic    string `json:"topic"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		created := discordChannel{
			ID:       fmt.Sprintf("7700000000000000%02d", m.nextChID.Add(1)),
			GuildID:  guildID,
			Type:     req.Type,
			Name:     req.Name,
			ParentID: req.ParentID,
			Topic:    req.Topic,
		}
		m.mu.Lock()
		m.channels = append(m.channels, created)
		m.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(created)
	case path == "/gateway/bot" && r.Method == http.MethodGet:
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"url":%q,"shards":1}`, m.WSURL())
	case strings.HasPrefix(path, "/channels/") && strings.HasSuffix(path, "/messages") && r.Method == http.MethodGet:
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[]`)
	case strings.HasPrefix(path, "/channels/") && strings.HasSuffix(path, "/messages") && r.Method == http.MethodPost:
		chID := strings.TrimSuffix(strings.TrimPrefix(path, "/channels/"), "/messages")
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		rec := recordedDiscordPost{ChannelID: chID, Payload: payload}
		m.mu.Lock()
		m.posts = append(m.posts, rec)
		m.mu.Unlock()
		select {
		case m.postCh <- rec:
		default:
		}
		id := fmt.Sprintf("8800000000000000%02d", m.msgSeq.Add(1))
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":%q,"channel_id":%q}`, id, chID)
	case strings.HasPrefix(path, "/channels/") && strings.HasSuffix(path, "/typing") && r.Method == http.MethodPost:
		w.WriteHeader(http.StatusNoContent)
	case strings.HasPrefix(path, "/interactions/") && strings.HasSuffix(path, "/callback") && r.Method == http.MethodPost:
		trimmed := strings.TrimSuffix(strings.TrimPrefix(path, "/interactions/"), "/callback")
		id, tok, _ := strings.Cut(trimmed, "/")
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		rec := recordedDiscordInteractionResp{ID: id, Token: tok, Payload: payload}
		m.mu.Lock()
		m.interactions = append(m.interactions, rec)
		m.mu.Unlock()
		select {
		case m.interCh <- rec:
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

func (m *mockDiscordServer) handleGatewayWS(w http.ResponseWriter, r *http.Request) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	h := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	accept := base64.StdEncoding.EncodeToString(h[:])

	conn, rw, err := hj.Hijack()
	if err != nil {
		return
	}
	_, _ = io.WriteString(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: "+accept+"\r\n\r\n")
	_ = rw.Flush()

	m.mu.Lock()
	if m.conn != nil {
		_ = m.conn.Close()
	}
	m.conn = conn
	m.bw = rw.Writer
	m.mu.Unlock()

	// Send Hello immediately.
	_ = m.SendGatewayJSON(map[string]any{
		"op": 10,
		"d":  map[string]any{"heartbeat_interval": 30000},
	})

	go func(br *bufio.Reader, c net.Conn) {
		defer func() { _ = c.Close() }()
		for {
			op, payload, rErr := readClientWSFrame(br)
			if rErr != nil {
				return
			}
			if op == 8 {
				return
			}
			if op != 1 {
				continue
			}
			var pkt struct {
				Op int             `json:"op"`
				D  json.RawMessage `json:"d"`
			}
			if json.Unmarshal(payload, &pkt) != nil {
				continue
			}
			switch pkt.Op {
			case 2: // Identify
				var ident struct {
					Token string `json:"token"`
				}
				_ = json.Unmarshal(pkt.D, &ident)
				select {
				case m.identified <- ident.Token:
				default:
				}
				_ = m.SendGatewayJSON(map[string]any{
					"op": 0,
					"t":  "READY",
					"s":  1,
					"d": map[string]any{
						"user": map[string]any{"id": m.botUserID, "username": "FleetBot", "bot": true},
					},
				})
			case 1: // Heartbeat
				_ = m.SendGatewayJSON(map[string]any{"op": 11})
			}
		}
	}(rw.Reader, conn)
}

func (m *mockDiscordServer) SendGatewayJSON(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.bw == nil {
		return fmt.Errorf("no connected gateway socket")
	}
	var hdr [10]byte
	hdr[0] = 0x81 // FIN + Text
	n := len(b)
	hdrLen := 2
	switch {
	case n < 126:
		hdr[1] = byte(n)
	case n <= 65535:
		hdr[1] = 126
		binary.BigEndian.PutUint16(hdr[2:4], uint16(n))
		hdrLen = 4
	default:
		hdr[1] = 127
		binary.BigEndian.PutUint64(hdr[2:10], uint64(n))
		hdrLen = 10
	}
	if _, err := m.bw.Write(hdr[:hdrLen]); err != nil {
		return err
	}
	if _, err := m.bw.Write(b); err != nil {
		return err
	}
	return m.bw.Flush()
}

func readClientWSFrame(br *bufio.Reader) (byte, []byte, error) {
	b1, err := br.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	b2, err := br.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	op := b1 & 0x0f
	masked := (b2 & 0x80) != 0
	n := uint64(b2 & 0x7f)
	switch n {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			return 0, nil, err
		}
		n = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			return 0, nil, err
		}
		n = binary.BigEndian.Uint64(ext[:])
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(br, mask[:]); err != nil {
			return 0, nil, err
		}
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(br, payload); err != nil {
		return 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return op, payload, nil
}

func waitForPost(t *testing.T, ch <-chan recordedDiscordPost, timeout time.Duration, pred func(recordedDiscordPost) bool) recordedDiscordPost {
	t.Helper()
	dl := time.After(timeout)
	for {
		select {
		case p := <-ch:
			if pred == nil || pred(p) {
				return p
			}
		case <-dl:
			t.Fatalf("timed out waiting for Discord REST post")
		}
	}
}

func setupDiscordTestEngine(t *testing.T, llmHandler http.HandlerFunc, mockDisc *mockDiscordServer) (*Engine, Config) {
	t.Helper()
	llmSrv := httptest.NewServer(llmHandler)
	t.Cleanup(llmSrv.Close)

	tmp := t.TempDir()
	eng, err := Open(filepath.Join(tmp, "assistant.db"), filepath.Join(tmp, "assistant.json"), filepath.Join(tmp, "vault"), &fakeDeps{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = eng.Close() })

	eng.discordAPIBase = mockDisc.APIURL()
	eng.discordGatewayURL = mockDisc.WSURL()
	eng.SetOPReader(func(_ context.Context, ref string) (string, error) {
		if ref == "op://Fleet-Agent/DiscordBot/token" {
			return "disc-secret-bot-tok-xyz", nil
		}
		return "", fmt.Errorf("missing ref %s", ref)
	})

	cfg := DefaultConfig()
	cfg.Enabled = true
	cfg.BaseURL = llmSrv.URL
	cfg.APIKey = "sk-or-test"
	noAutoSync := false
	cfg.Discord = DiscordConfig{
		Enabled:          true,
		BotTokenOPRef:    "op://Fleet-Agent/DiscordBot/token",
		AuthorizedUserID: "222222222222222222",
		AutoSyncChannels: &noAutoSync,
		FleetBaseURL:     "http://127.0.0.1:4771",
		Channels: map[string]string{
			"general":   "555555555555555555",
			"fleet-ops": "666666666666666666",
		},
	}
	if _, err := eng.UpdateConfig(cfg, false); err != nil {
		t.Fatal(err)
	}
	return eng, cfg
}

func TestDiscordGatewaySyncAndTwoWayTurn(t *testing.T) {
	disc := newMockDiscordServer(t)
	defer disc.Close()

	var llmCalls atomic.Int32
	eng, _ := setupDiscordTestEngine(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var req struct {
			Stream bool `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if !req.Stream {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":""}}]}`)
			return
		}
		llmCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hello back on Discord!\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"finish_reason\":\"stop\",\"delta\":{}}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}, disc)

	ctx := t.Context()
	eng.Start(ctx)

	select {
	case tok := <-disc.identified:
		if tok != "disc-secret-bot-tok-xyz" {
			t.Fatalf("unexpected identify token %q", tok)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Discord Gateway Identify")
	}

	// 1. Unauthorized user and bot messages must be ignored.
	_ = disc.SendGatewayJSON(map[string]any{
		"op": 0, "t": "MESSAGE_CREATE", "s": 2,
		"d": map[string]any{
			"id": "100000000000000001", "channel_id": "555555555555555555",
			"content": "bot echo", "author": map[string]any{"id": "999999999999999999", "bot": true},
		},
	})
	_ = disc.SendGatewayJSON(map[string]any{
		"op": 0, "t": "MESSAGE_CREATE", "s": 3,
		"d": map[string]any{
			"id": "100000000000000002", "channel_id": "555555555555555555",
			"content": "intruder", "author": map[string]any{"id": "777777777777777777", "bot": false},
		},
	})

	// 2. Authorized user message in mapped channel -> runs turn and replies with message_reference.
	_ = disc.SendGatewayJSON(map[string]any{
		"op": 0, "t": "MESSAGE_CREATE", "s": 4,
		"d": map[string]any{
			"id": "100000000000000003", "channel_id": "555555555555555555",
			"content": "hello from discord", "author": map[string]any{"id": "222222222222222222", "bot": false},
		},
	})

	reply := waitForPost(t, disc.postCh, 5*time.Second, func(p recordedDiscordPost) bool {
		c, _ := p.Payload["content"].(string)
		return strings.Contains(c, "Hello back on Discord!")
	})
	if reply.ChannelID != "555555555555555555" {
		t.Fatalf("unexpected reply channel %s", reply.ChannelID)
	}
	if strings.Contains(fmt.Sprint(reply.Payload["content"]), "You (Fleet Web)") {
		t.Fatalf("discord-origin turn should not prepend Web echo blockquote: %v", reply.Payload["content"])
	}
	ref, _ := reply.Payload["message_reference"].(map[string]any)
	if ref["message_id"] != "100000000000000003" {
		t.Fatalf("expected message_reference to original discord msg, got %+v", ref)
	}

	// 3. Duplicate external_id dispatch must be ignored.
	_ = disc.SendGatewayJSON(map[string]any{
		"op": 0, "t": "MESSAGE_CREATE", "s": 5,
		"d": map[string]any{
			"id": "100000000000000003", "channel_id": "555555555555555555",
			"content": "hello from discord", "author": map[string]any{"id": "222222222222222222", "bot": false},
		},
	})
	time.Sleep(150 * time.Millisecond)
	if llmCalls.Load() != 1 {
		t.Fatalf("expected exactly 1 LLM call (ignored bot/intruder/duplicate), got %d", llmCalls.Load())
	}

	// Verify DB attribution.
	msgs, err := eng.store.ListMessages("general", 20)
	if err != nil {
		t.Fatal(err)
	}
	var sawDiscordUser bool
	for _, msg := range msgs {
		if msg.Role == "user" && msg.Source == "discord" && msg.ExternalID == "100000000000000003" {
			sawDiscordUser = true
		}
		if msg.Content == "intruder" || msg.Content == "bot echo" {
			t.Fatalf("unauthorized/bot message leaked into assistant_messages: %+v", msg)
		}
	}
	if !sawDiscordUser {
		t.Fatalf("expected user message with source=discord and external_id recorded in DB, got %+v", msgs)
	}

	// 4. `/new` resets context_epoch without deleting Fleet UI history and posts divider to Discord.
	_ = disc.SendGatewayJSON(map[string]any{
		"op": 0, "t": "MESSAGE_CREATE", "s": 6,
		"d": map[string]any{
			"id": "100000000000000004", "channel_id": "555555555555555555",
			"content": "/new", "author": map[string]any{"id": "222222222222222222", "bot": false},
		},
	})
	div := waitForPost(t, disc.postCh, 5*time.Second, func(p recordedDiscordPost) bool {
		embeds, _ := p.Payload["embeds"].([]any)
		if len(embeds) == 0 {
			return false
		}
		emb, _ := embeds[0].(map[string]any)
		title, _ := emb["title"].(string)
		return strings.Contains(title, "Context Cleared")
	})
	if div.ChannelID != "555555555555555555" {
		t.Fatalf("unexpected divider channel %s", div.ChannelID)
	}
	divEmb := div.Payload["embeds"].([]any)[0].(map[string]any)
	divDesc, _ := divEmb["description"].(string)
	if !strings.Contains(divDesc, "Discord") || !strings.Contains(divDesc, "long-term memory") {
		t.Fatalf("unexpected /new embed description: %q", divDesc)
	}
	ch, err := eng.store.GetChannel("general")
	if err != nil || ch.ContextEpoch < 1 {
		t.Fatalf("expected context_epoch incremented after /new, got %+v (%v)", ch, err)
	}
	afterResetMsgs, err := eng.store.ListMessages("general", 20)
	if err != nil || len(afterResetMsgs) < 2 {
		t.Fatalf("expected Fleet UI history preserved across /new, got %d (%v)", len(afterResetMsgs), err)
	}
}

func TestDiscordWebTurnMirrorAndWidgets(t *testing.T) {
	disc := newMockDiscordServer(t)
	defer disc.Close()

	var step atomic.Int32
	eng, _ := setupDiscordTestEngine(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var req struct {
			Stream bool `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if !req.Stream {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":""}}]}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		switch step.Add(1) {
		case 1:
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"tc_card\",\"type\":\"function\",\"function\":{\"name\":\"render_card\",\"arguments\":\"{\\\"title\\\":\\\"Fleet Snapshot\\\",\\\"kind\\\":\\\"key_value\\\",\\\"data\\\":{\\\"Online\\\":3}}\"}}]}}]}\n\n")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":1,\"id\":\"tc_html\",\"type\":\"function\",\"function\":{\"name\":\"render_widget\",\"arguments\":\"{\\\"title\\\":\\\"Usage Chart\\\",\\\"html\\\":\\\"<div>chart</div>\\\"}\"}}]}}]}\n\n")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"finish_reason\":\"tool_calls\",\"delta\":{}}]}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		default:
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Status rendered.\"}}]}\n\n")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"finish_reason\":\"stop\",\"delta\":{}}]}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		}
	}, disc)

	ctx := t.Context()
	eng.Start(ctx)

	if _, _, err := eng.EnqueueUserMessage("general", "show status", ""); err != nil {
		t.Fatal(err)
	}

	mirrored := waitForPost(t, disc.postCh, 5*time.Second, func(p recordedDiscordPost) bool {
		c, _ := p.Payload["content"].(string)
		return strings.Contains(c, "Status rendered.")
	})
	content, _ := mirrored.Payload["content"].(string)
	if !strings.Contains(content, "> **You (Fleet Web):** show status") {
		t.Fatalf("expected non-spoofed Web origin quote header, got %q", content)
	}
	if !strings.Contains(content, "Interactive widget attached") || !strings.Contains(content, "http://127.0.0.1:4771/#chat/general") {
		t.Fatalf("expected HTML widget deep-link fallback note, got %q", content)
	}
	embeds, _ := mirrored.Payload["embeds"].([]any)
	if len(embeds) != 1 {
		t.Fatalf("expected 1 Discord embed for render_card, got %+v", mirrored.Payload["embeds"])
	}
}

func TestDiscordInteractiveApprovals(t *testing.T) {
	disc := newMockDiscordServer(t)
	defer disc.Close()

	var step atomic.Int32
	eng, _ := setupDiscordTestEngine(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var req struct {
			Stream bool `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if !req.Stream {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":""}}]}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if step.Add(1) == 1 {
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"tc_w\",\"type\":\"function\",\"function\":{\"name\":\"vault_write\",\"arguments\":\"{\\\"path\\\":\\\"Projects/Secret.md\\\",\\\"content\\\":\\\"Approved via Discord!\\\"}\"}}]}}]}\n\n")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"finish_reason\":\"tool_calls\",\"delta\":{}}]}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Secret.md saved after Discord approval.\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"finish_reason\":\"stop\",\"delta\":{}}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}, disc)

	ctx := t.Context()
	eng.Start(ctx)

	select {
	case <-disc.identified:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Discord Gateway Identify")
	}

	if _, _, err := eng.EnqueueUserMessage("fleet-ops", "write Projects/Secret.md", ""); err != nil {
		t.Fatal(err)
	}

	aprPost := waitForPost(t, disc.postCh, 5*time.Second, func(p recordedDiscordPost) bool {
		comps, _ := p.Payload["components"].([]any)
		return len(comps) > 0
	})
	if aprPost.ChannelID != "666666666666666666" {
		t.Fatalf("expected approval on fleet-ops Discord channel, got %s", aprPost.ChannelID)
	}
	// Extract aprID from button custom_id "fleet_apr:approve:<id>".
	row := aprPost.Payload["components"].([]any)[0].(map[string]any)
	btn := row["components"].([]any)[0].(map[string]any)
	customID, _ := btn["custom_id"].(string)
	aprID := strings.TrimPrefix(customID, "fleet_apr:approve:")
	if aprID == "" || aprID == customID {
		t.Fatalf("unexpected approve button custom_id: %q", customID)
	}

	// Unauthorized user clicking button gets ephemeral rejection.
	_ = disc.SendGatewayJSON(map[string]any{
		"op": 0, "t": "INTERACTION_CREATE", "s": 2,
		"d": map[string]any{
			"id": "901", "type": 3, "token": "tok-unauth",
			"user": map[string]any{"id": "888888888888888888"},
			"data": map[string]any{"custom_id": customID},
		},
	})
	var unauthResp recordedDiscordInteractionResp
	select {
	case unauthResp = <-disc.interCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for unauthorized interaction callback")
	}
	if int(unauthResp.Payload["type"].(float64)) != 4 {
		t.Fatalf("expected ephemeral type 4 response for unauthorized click, got %+v", unauthResp.Payload)
	}

	// Authorized user clicking Approve updates embed (type 7), executes vault_write, and mirrors resume reply.
	_ = disc.SendGatewayJSON(map[string]any{
		"op": 0, "t": "INTERACTION_CREATE", "s": 3,
		"d": map[string]any{
			"id": "902", "type": 3, "token": "tok-auth",
			"user": map[string]any{"id": "222222222222222222"},
			"data": map[string]any{"custom_id": customID},
		},
	})
	var authResp recordedDiscordInteractionResp
	select {
	case authResp = <-disc.interCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for authorized interaction callback")
	}
	if int(authResp.Payload["type"].(float64)) != 7 {
		t.Fatalf("expected type 7 component update response, got %+v", authResp.Payload)
	}
	dataMap, _ := authResp.Payload["data"].(map[string]any)
	if comps, _ := dataMap["components"].([]any); len(comps) != 0 {
		t.Fatalf("expected decided approval to clear buttons, got %+v", comps)
	}

	resumed := waitForPost(t, disc.postCh, 5*time.Second, func(p recordedDiscordPost) bool {
		c, _ := p.Payload["content"].(string)
		return strings.Contains(c, "Secret.md saved after Discord approval.")
	})
	if resumed.ChannelID != "666666666666666666" {
		t.Fatalf("unexpected resumed mirror channel %s", resumed.ChannelID)
	}
	v, vErr := eng.vault()
	if vErr != nil {
		t.Fatal(vErr)
	}
	body, err := v.ReadFile("Projects/Secret.md")
	if err != nil || body != "Approved via Discord!" {
		t.Fatalf("expected vault write executed after Discord approve, got %q (%v)", body, err)
	}
}

func TestDiscordDoctorAndTokenScrubbing(t *testing.T) {
	disc := newMockDiscordServer(t)
	defer disc.Close()

	eng, _ := setupDiscordTestEngine(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}, disc)

	rep := eng.Doctor(context.Background())
	var discCheck *DoctorCheck
	for i := range rep.Checks {
		if rep.Checks[i].Name == "discord_bot" {
			discCheck = &rep.Checks[i]
			break
		}
	}
	if discCheck == nil || discCheck.Status != "ok" || !strings.Contains(discCheck.Detail, "FleetBot") {
		t.Fatalf("expected passing discord_bot doctor check mentioning FleetBot, got %+v", discCheck)
	}

	// Simulate auth failure echoing the bot secret token -> must fail and scrub token.
	disc.mu.Lock()
	disc.meStatus = http.StatusUnauthorized
	disc.meBody = `{"message":"401: Unauthorized for token disc-secret-bot-tok-xyz"}`
	disc.mu.Unlock()

	rep2 := eng.Doctor(context.Background())
	var discCheck2 *DoctorCheck
	for i := range rep2.Checks {
		if rep2.Checks[i].Name == "discord_bot" {
			discCheck2 = &rep2.Checks[i]
			break
		}
	}
	if discCheck2 == nil || discCheck2.Status != "fail" {
		t.Fatalf("expected failing discord_bot doctor check, got %+v", discCheck2)
	}
	if strings.Contains(discCheck2.Detail, "disc-secret-bot-tok-xyz") {
		t.Fatalf("expected bot token scrubbed from Doctor detail, got %q", discCheck2.Detail)
	}
}

func TestDiscordAutoSyncChannelsZeroConfig(t *testing.T) {
	disc := newMockDiscordServer(t)
	defer disc.Close()

	// Single private guild with pre-existing category and #general channel.
	disc.guilds = []discordGuild{{ID: "333333333333333333", Name: "My Private Fleet"}}
	disc.channels = []discordChannel{
		{ID: "440000000000000001", GuildID: "333333333333333333", Type: 4, Name: "Fleet Assistant"},
		{ID: "440000000000000002", GuildID: "333333333333333333", Type: 0, Name: "general", ParentID: "440000000000000001"},
	}

	llmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var req struct {
			Stream bool `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if !req.Stream {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":""}}]}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Synced reply from fleet-ops!\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"finish_reason\":\"stop\",\"delta\":{}}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer llmSrv.Close()

	tmp := t.TempDir()
	eng, err := Open(filepath.Join(tmp, "assistant.db"), filepath.Join(tmp, "assistant.json"), filepath.Join(tmp, "vault"), &fakeDeps{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = eng.Close() }()
	eng.discordAPIBase = disc.APIURL()
	eng.discordGatewayURL = disc.WSURL()
	eng.SetOPReader(func(_ context.Context, _ string) (string, error) {
		return "disc-secret-bot-tok-xyz", nil
	})

	// Configure ONLY bot_token_op_ref + authorized_user_id (no guild_id, no channels).
	cfg := DefaultConfig()
	cfg.Enabled = true
	cfg.BaseURL = llmSrv.URL
	cfg.APIKey = "sk-or-test"
	cfg.Discord = DiscordConfig{
		Enabled:          true,
		BotTokenOPRef:    "op://Fleet-Agent/DiscordBot/token",
		AuthorizedUserID: "222222222222222222",
	}
	if _, err := eng.UpdateConfig(cfg, false); err != nil {
		t.Fatal(err)
	}

	eng.Start(t.Context())
	select {
	case <-disc.identified:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Discord Identify")
	}

	// Poll until READY auto-sync persists guild_id and all 3 default channels.
	deadline := time.Now().Add(5 * time.Second)
	var syncedCfg DiscordConfig
	for time.Now().Before(deadline) {
		syncedCfg = eng.Config().Discord
		if syncedCfg.GuildID == "333333333333333333" && len(syncedCfg.Channels) >= 3 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if syncedCfg.Channels["general"] != "440000000000000002" {
		t.Fatalf("expected existing #general adopted cleanly, got %+v", syncedCfg.Channels)
	}
	opsDiscID := syncedCfg.Channels["fleet-ops"]
	if opsDiscID == "" || syncedCfg.Channels["briefing"] == "" {
		t.Fatalf("expected #fleet-ops and #briefing auto-created in Discord, got %+v", syncedCfg.Channels)
	}

	// Verify persistence across reload from disk.
	reloaded, err := LoadConfig(filepath.Join(tmp, "assistant.json"))
	if err != nil || reloaded.Discord.Channels["fleet-ops"] != opsDiscID {
		t.Fatalf("expected synced channels saved to assistant.json, got %+v (%v)", reloaded.Discord, err)
	}

	// Immediately message the newly auto-created #fleet-ops Discord channel without Gateway reconnect.
	_ = disc.SendGatewayJSON(map[string]any{
		"op": 0, "t": "MESSAGE_CREATE", "s": 2,
		"d": map[string]any{
			"id": "100000000000000099", "channel_id": opsDiscID,
			"content": "ping auto synced channel", "author": map[string]any{"id": "222222222222222222", "bot": false},
		},
	})
	reply := waitForPost(t, disc.postCh, 5*time.Second, func(p recordedDiscordPost) bool {
		c, _ := p.Payload["content"].(string)
		return strings.Contains(c, "Synced reply from fleet-ops!")
	})
	if reply.ChannelID != opsDiscID {
		t.Fatalf("expected reply on %s, got %s", opsDiscID, reply.ChannelID)
	}
}

func TestDiscordTwoWayChannelCreation(t *testing.T) {
	disc := newMockDiscordServer(t)
	defer disc.Close()

	disc.guilds = []discordGuild{{ID: "333333333333333333", Name: "My Private Fleet"}}

	tmp := t.TempDir()
	eng, err := Open(filepath.Join(tmp, "assistant.db"), filepath.Join(tmp, "assistant.json"), filepath.Join(tmp, "vault"), &fakeDeps{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = eng.Close() }()
	eng.discordAPIBase = disc.APIURL()
	eng.discordGatewayURL = disc.WSURL()
	eng.SetOPReader(func(_ context.Context, _ string) (string, error) {
		return "disc-secret-bot-tok-xyz", nil
	})

	cfg := DefaultConfig()
	cfg.Enabled = true
	cfg.APIKey = "sk-or-test"
	cfg.Discord = DiscordConfig{
		Enabled:          true,
		BotTokenOPRef:    "op://Fleet-Agent/DiscordBot/token",
		AuthorizedUserID: "222222222222222222",
	}
	if _, err := eng.UpdateConfig(cfg, false); err != nil {
		t.Fatal(err)
	}

	ctx := t.Context()
	eng.Start(ctx)
	select {
	case <-disc.identified:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Discord Identify")
	}

	// Wait for initial READY category & default channels creation.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(eng.Config().Discord.Channels) < 3 {
		time.Sleep(25 * time.Millisecond)
	}
	disc.mu.Lock()
	var catID string
	for _, c := range disc.channels {
		if c.Type == 4 && c.Name == DefaultDiscordCategoryName {
			catID = c.ID
			break
		}
	}
	disc.mu.Unlock()
	if catID == "" {
		t.Fatalf("expected %q category created in Discord", DefaultDiscordCategoryName)
	}

	sub := eng.Subscribe()
	defer eng.Unsubscribe(sub)

	// 1. Fleet -> Discord: creating #research in Fleet auto-creates #research in Discord.
	if _, err := eng.UpsertChannel(ctx, "research", "#research", "Deep dives", ""); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && eng.Config().Discord.Channels["research"] == "" {
		time.Sleep(25 * time.Millisecond)
	}
	if eng.Config().Discord.Channels["research"] == "" {
		t.Fatalf("expected #research mapped after UpsertChannel, got %+v", eng.Config().Discord.Channels)
	}

	// 2. Discord -> Fleet: adding a channel inside category adopts it into Fleet + ignores outside channel.
	outsideCh := discordChannel{ID: "990000000000000001", GuildID: "333333333333333333", Type: 0, Name: "random-outside", ParentID: ""}
	financeCh := discordChannel{ID: "990000000000000002", GuildID: "333333333333333333", Type: 0, Name: "Finance & Tax", ParentID: catID, Topic: "Tax docs"}
	disc.mu.Lock()
	disc.channels = append(disc.channels, outsideCh, financeCh)
	disc.mu.Unlock()

	_ = disc.SendGatewayJSON(map[string]any{"op": 0, "t": "CHANNEL_CREATE", "s": 2, "d": outsideCh})
	_ = disc.SendGatewayJSON(map[string]any{"op": 0, "t": "CHANNEL_CREATE", "s": 3, "d": financeCh})

	var sawSSE bool
	dl := time.After(5 * time.Second)
	for !sawSSE || eng.Config().Discord.Channels["finance-tax"] != financeCh.ID {
		select {
		case ev := <-sub:
			if ev.Type == "channels_updated" {
				sawSSE = true
			}
		case <-time.After(25 * time.Millisecond):
		case <-dl:
			t.Fatalf("timed out waiting for finance-tax sync/SSE (sawSSE=%v, channels=%+v)", sawSSE, eng.Config().Discord.Channels)
		}
	}
	if _, err := eng.store.GetChannel("finance-tax"); err != nil {
		t.Fatalf("expected finance-tax created in assistant_channels: %v", err)
	}
	if eng.Config().Discord.Channels["random-outside"] != "" {
		t.Fatalf("outside channel should not be synced into Fleet")
	}
}

func TestDiscordAutoSyncMultipleGuildsRequiresID(t *testing.T) {
	disc := newMockDiscordServer(t)
	defer disc.Close()

	disc.guilds = []discordGuild{
		{ID: "111111111111111111", Name: "Server A"},
		{ID: "222222222222222222", Name: "Server B"},
	}

	eng, _ := setupDiscordTestEngine(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}, disc)

	if _, err := eng.SyncDiscordChannels(context.Background()); err == nil || !strings.Contains(err.Error(), "multiple servers") {
		t.Fatalf("expected multiple servers error when guild_id is omitted, got %v", err)
	}
}

func TestDiscordContextClearedNoteFromWebReset(t *testing.T) {
	disc := newMockDiscordServer(t)
	defer disc.Close()

	eng, cfg := setupDiscordTestEngine(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}, disc)

	noMirror := false
	cfg.Discord.MirrorWebTurns = &noMirror
	if _, err := eng.UpdateConfig(cfg, false); err != nil {
		t.Fatal(err)
	}

	if _, err := eng.ResetChannel("general", true); err != nil {
		t.Fatal(err)
	}

	note := waitForPost(t, disc.postCh, 5*time.Second, func(p recordedDiscordPost) bool {
		return p.ChannelID == "555555555555555555"
	})
	embeds, _ := note.Payload["embeds"].([]any)
	if len(embeds) != 1 {
		t.Fatalf("expected 1 embed in context-cleared post, got %+v", note.Payload)
	}
	emb, _ := embeds[0].(map[string]any)
	title, _ := emb["title"].(string)
	desc, _ := emb["description"].(string)
	footer, _ := emb["footer"].(map[string]any)
	footerText, _ := footer["text"].(string)
	if !strings.Contains(title, "Context Cleared") {
		t.Fatalf("expected Context Cleared title, got %q", title)
	}
	if !strings.Contains(desc, "Fleet Console") || !strings.Contains(desc, "long-term memory") {
		t.Fatalf("unexpected embed description: %q", desc)
	}
	if !strings.Contains(footerText, "#general") || !strings.Contains(footerText, "Context #2") {
		t.Fatalf("unexpected embed footer: %q", footerText)
	}

	deadline := time.Now().Add(2 * time.Second)
	var cursor string
	for time.Now().Before(deadline) {
		var err error
		cursor, err = eng.store.GetDiscordCursor("general")
		if err == nil && cursor != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if cursor != "880000000000000001" {
		t.Fatalf("expected Discord cursor updated to 880000000000000001, got %q", cursor)
	}
}
