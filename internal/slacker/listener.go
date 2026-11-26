package slacker

import (
	"bytes"
	"cmp"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bdatdo0601/fleet/internal/paths"
	"github.com/bdatdo0601/fleet/internal/registry"
)

// ListenerOpts configures the lighthouse-hosted Socket Mode listener.
type ListenerOpts struct {
	StateDir    string
	ConfigPath  string
	Store       *Store
	Slack       *SlackClient
	TLSConfig   *tls.Config
	NotifyURL   string
	APIBaseURL  string
	AgentLoadFn AgentLoadFunc
	Stderr      io.Writer
	BackoffBase time.Duration
}

type socketEnvelope struct {
	EnvelopeID string          `json:"envelope_id"`
	Type       string          `json:"type"`
	Payload    json.RawMessage `json:"payload"`
}

type eventsAPIPayload struct {
	EventID string `json:"event_id"`
	Event   struct {
		Type     string `json:"type"`
		User     string `json:"user"`
		Reaction string `json:"reaction"`
		Item     struct {
			Type    string `json:"type"`
			Channel string `json:"channel"`
			TS      string `json:"ts"`
		} `json:"item"`
	} `json:"event"`
}

func acquirePIDLock(path, role string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	for range 2 {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
			_ = f.Close()
			return nil
		}
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		raw, rErr := os.ReadFile(path)
		if rErr != nil {
			return err
		}
		pid, pErr := strconv.Atoi(strings.TrimSpace(string(raw)))
		if pErr == nil && pid > 0 && pid != os.Getpid() && syscall.Kill(pid, 0) == nil {
			return fmt.Errorf("another %s is already running (pid %d)", role, pid)
		}
		_ = os.Remove(path)
	}
	return fmt.Errorf("could not acquire %s lock %s", role, path)
}

func releaseListenerLock(path string) {
	if path != "" {
		_ = os.Remove(path)
	}
}

func notifyServeWake(ctx context.Context, notifyURL, device string) {
	if notifyURL == "" {
		return
	}
	reqCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	body, _ := json.Marshal(map[string]string{"device": device})
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, notifyURL, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		_ = resp.Body.Close()
	}
}

type accountRunner struct {
	id        string
	appToken  string
	cancel    context.CancelFunc
	done      chan struct{}
	connected bool
}

type listenerHub struct {
	opts       ListenerOpts
	cfgPath    string
	store      *Store
	slack      *SlackClient
	notifyURL  string
	loadFn     AgentLoadFunc
	stderr     io.Writer
	backoff    time.Duration
	dispatchMu sync.Mutex

	mu      sync.Mutex
	runners map[string]*accountRunner
}

func (h *listenerHub) setConnected(acctID string, connected bool, lastEventAt, lastErr string) {
	h.mu.Lock()
	if r, ok := h.runners[acctID]; ok {
		r.connected = connected
	}
	anyConn := false
	for _, r := range h.runners {
		if r.connected {
			anyConn = true
			break
		}
	}
	h.mu.Unlock()
	_ = h.store.UpsertRuntime("listener:"+acctID, os.Getpid(), connected, lastEventAt, lastErr)
	_ = h.store.UpsertRuntime("listener", os.Getpid(), anyConn, lastEventAt, lastErr)
}

func (h *listenerHub) reconcile(parent context.Context) {
	cfg, err := LoadConfig(h.cfgPath)
	if err != nil {
		_ = h.store.UpsertRuntime("listener", os.Getpid(), false, "", err.Error())
		_, _ = fmt.Fprintf(h.stderr, "slacker listener config error: %v\n", err)
		return
	}
	enabled := cfg.EnabledAccounts()
	want := make(map[string]AccountConfig, len(enabled))
	for _, a := range enabled {
		want[a.ID] = a
	}

	h.mu.Lock()
	var toStop []*accountRunner
	for id, r := range h.runners {
		target, ok := want[id]
		if !ok || target.AppToken != r.appToken {
			r.cancel()
			toStop = append(toStop, r)
			delete(h.runners, id)
		}
	}
	h.mu.Unlock()

	for _, r := range toStop {
		<-r.done
	}

	if len(want) == 0 {
		_ = h.store.UpsertRuntime("listener", os.Getpid(), false, "", "slacker disabled or no configured accounts")
		return
	}

	h.mu.Lock()
	for id, acct := range want {
		if _, exists := h.runners[id]; exists {
			continue
		}
		rCtx, cancel := context.WithCancel(parent)
		r := &accountRunner{
			id:       id,
			appToken: acct.AppToken,
			cancel:   cancel,
			done:     make(chan struct{}),
		}
		h.runners[id] = r
		go h.runAccount(rCtx, r, acct)
	}
	h.mu.Unlock()
}

func (h *listenerHub) stopAll() {
	h.mu.Lock()
	list := make([]*accountRunner, 0, len(h.runners))
	for id, r := range h.runners {
		r.cancel()
		list = append(list, r)
		delete(h.runners, id)
	}
	h.mu.Unlock()
	for _, r := range list {
		<-r.done
	}
}

func (h *listenerHub) runAccount(ctx context.Context, r *accountRunner, acct AccountConfig) {
	defer close(r.done)
	defer h.setConnected(acct.ID, false, "", "")

	curBackoff := h.backoff
	for {
		if ctx.Err() != nil {
			return
		}
		wsURL, err := h.slack.OpenSocketURL(ctx, acct.AppToken)
		if err != nil {
			h.setConnected(acct.ID, false, "", err.Error())
			_, _ = fmt.Fprintf(h.stderr, "slacker[%s] apps.connections.open error: %v\n", acct.ID, err)
			if !sleepContext(ctx, curBackoff) {
				return
			}
			curBackoff = min(curBackoff*2, 30*time.Second)
			continue
		}
		conn, err := DialWS(ctx, wsURL, h.opts.TLSConfig)
		if err != nil {
			h.setConnected(acct.ID, false, "", err.Error())
			_, _ = fmt.Fprintf(h.stderr, "slacker[%s] websocket dial error: %v\n", acct.ID, err)
			if !sleepContext(ctx, curBackoff) {
				return
			}
			curBackoff = min(curBackoff*2, 30*time.Second)
			continue
		}

		curBackoff = h.backoff
		h.setConnected(acct.ID, true, "", "")
		_, _ = fmt.Fprintf(h.stderr, "slacker[%s] listener connected\n", acct.ID)

		reason := h.serveAccountSocket(ctx, conn, acct.ID)
		_ = conn.Close()
		h.setConnected(acct.ID, false, "", reason)
		if ctx.Err() != nil {
			return
		}
		_, _ = fmt.Fprintf(h.stderr, "slacker[%s] listener disconnected: %s\n", acct.ID, reason)
	}
}

func (h *listenerHub) serveAccountSocket(ctx context.Context, conn *WSConn, acctID string) string {
	doneCh := make(chan struct{})
	defer close(doneCh)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-doneCh:
		}
	}()

	for {
		raw, err := conn.ReadText(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return "stopped"
			}
			return err.Error()
		}
		var env socketEnvelope
		if err := json.Unmarshal(raw, &env); err != nil {
			_, _ = fmt.Fprintf(h.stderr, "slacker[%s] invalid envelope JSON: %v\n", acctID, err)
			continue
		}
		if env.Type == "disconnect" {
			return "slack requested disconnect"
		}
		if env.Type == "hello" {
			continue
		}

		cfg, err := LoadConfig(h.cfgPath)
		if err != nil {
			h.setConnected(acctID, true, "", err.Error())
			continue
		}
		acct, ok := cfg.Accounts[acctID]
		if !ok || !acct.Enabled {
			return "account disabled"
		}

		shouldACK, targetDev, procErr := handleAccountEnvelope(ctx, acct, cfg, env, h.store, h.slack, h.loadFn, &h.dispatchMu)
		if procErr != nil {
			h.setConnected(acctID, true, "", procErr.Error())
			_, _ = fmt.Fprintf(h.stderr, "slacker[%s] envelope error: %v\n", acctID, procErr)
		} else {
			h.setConnected(acctID, true, registry.NowISO(), "")
		}
		if shouldACK && env.EnvelopeID != "" {
			ackBytes, _ := json.Marshal(map[string]string{"envelope_id": env.EnvelopeID})
			if wErr := conn.WriteText(ackBytes); wErr != nil {
				return wErr.Error()
			}
			if targetDev != "" {
				notifyServeWake(ctx, h.notifyURL, targetDev)
			}
		}
	}
}

// RunListener runs the single lighthouse Socket Mode daemon managing one WebSocket per enabled Slack account.
func RunListener(ctx context.Context, opts ListenerOpts) error {
	stateDir := cmp.Or(opts.StateDir, paths.StateDir())
	cfgPath := cmp.Or(opts.ConfigPath, filepath.Join(stateDir, "slacker.json"))
	stderr := cmp.Or(opts.Stderr, io.Writer(os.Stderr))
	slack := opts.Slack
	if slack == nil {
		slack = &SlackClient{}
	}
	notifyURL := opts.NotifyURL
	if notifyURL == "" {
		notifyURL = fmt.Sprintf("http://127.0.0.1:%d/v1/slacker/notify", paths.APIPort())
	}
	loadFn := opts.AgentLoadFn
	if loadFn == nil {
		apiBase := opts.APIBaseURL
		if apiBase == "" && opts.NotifyURL == "" {
			apiBase = fmt.Sprintf("http://127.0.0.1:%d", paths.APIPort())
		}
		loadFn = DefaultAgentLoad(stateDir, apiBase)
	}

	store := opts.Store
	if store == nil {
		var err error
		store, err = Open(filepath.Join(stateDir, "slacker.db"))
		if err != nil {
			return err
		}
		defer func() { _ = store.Close() }()
	}

	lockPath := filepath.Join(stateDir, "slacker-listener.lock")
	if err := acquirePIDLock(lockPath, "slacker listener"); err != nil {
		return err
	}
	defer releaseListenerLock(lockPath)

	hub := &listenerHub{
		opts:      opts,
		cfgPath:   cfgPath,
		store:     store,
		slack:     slack,
		notifyURL: notifyURL,
		loadFn:    loadFn,
		stderr:    stderr,
		backoff:   cmp.Or(opts.BackoffBase, 2*time.Second),
		runners:   map[string]*accountRunner{},
	}
	defer func() {
		hub.stopAll()
		_ = store.UpsertRuntime("listener", os.Getpid(), false, "", "")
	}()

	hub.reconcile(ctx)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			hub.reconcile(ctx)
		}
	}
}

// HandleAccountEnvelope processes one Socket Mode envelope for acct. It returns shouldACK=true ONLY
// after any new job has been durably persisted in store (or identified as a duplicate/ignored event).
func HandleAccountEnvelope(
	ctx context.Context,
	acct AccountConfig,
	cfg Config,
	env socketEnvelope,
	store *Store,
	slack *SlackClient,
) (shouldACK bool, notifyDevice string, err error) {
	return handleAccountEnvelope(ctx, acct, cfg, env, store, slack, nil, nil)
}

func handleAccountEnvelope(
	ctx context.Context,
	acct AccountConfig,
	cfg Config,
	env socketEnvelope,
	store *Store,
	slack *SlackClient,
	loadFn AgentLoadFunc,
	dispatchMu *sync.Mutex,
) (shouldACK bool, notifyDevice string, err error) {
	if env.Type != "events_api" || len(env.Payload) == 0 {
		return true, "", nil
	}
	var p eventsAPIPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return true, "", fmt.Errorf("decode events_api payload: %w", err)
	}
	if p.Event.Type != "reaction_added" || p.Event.Item.Type != "message" {
		return true, "", nil
	}
	if acct.AuthorizedUserID == "" || p.Event.User != acct.AuthorizedUserID {
		return true, "", nil
	}
	chMap, ok := cfg.FindChannel(acct.ID, p.Event.Item.Channel)
	if !ok {
		return true, "", nil
	}
	emoji := NormalizeEmoji(p.Event.Reaction)
	harness, ok := cfg.Emojis[emoji]
	if !ok {
		return true, "", nil
	}
	acctID := cmp.Or(acct.ID, DefaultAccountID)
	rawEventID := cmp.Or(p.EventID, fmt.Sprintf("%s:%s:%s:%s", p.Event.User, p.Event.Item.Channel, p.Event.Item.TS, emoji))
	eventID := rawEventID
	if acctID != DefaultAccountID {
		eventID = acctID + ":" + rawEventID
	}

	exists, err := store.HasEvent(eventID)
	if err != nil {
		return false, "", err
	}
	if exists {
		return true, "", nil
	}

	msgExists, err := store.HasMessageJob(acctID, p.Event.Item.Channel, p.Event.Item.TS)
	if err != nil {
		return false, "", err
	}
	if msgExists {
		return true, "", nil
	}

	msg, err := slack.FetchMessage(ctx, acct.UserToken, p.Event.Item.Channel, p.Event.Item.TS)
	if err != nil {
		// Transient fetch error: do NOT ACK so Slack retries delivery.
		return false, "", err
	}
	ev := ReactionEvent{
		EventID:   eventID,
		User:      p.Event.User,
		Reaction:  emoji,
		ChannelID: p.Event.Item.Channel,
		MessageTS: p.Event.Item.TS,
	}

	if dispatchMu != nil {
		dispatchMu.Lock()
		defer dispatchMu.Unlock()
	}
	targetDevice, resolvedRepo, scope, selErr := SelectTarget(ctx, cfg, acctID, chMap, store, loadFn, time.Now())
	if selErr != nil {
		// Configuration error: ACK to prevent Slack retry storm, while surfacing the error in runtime status.
		return true, "", selErr
	}
	resolvedCh := chMap
	resolvedCh.Device = targetDevice
	resolvedCh.RepoPath = resolvedRepo

	job := Job{
		Account:      acctID,
		EventID:      eventID,
		ChannelID:    ev.ChannelID,
		ChannelName:  cmp.Or(msg.ChannelName, chMap.ChannelName),
		MessageTS:    ev.MessageTS,
		ThreadTS:     msg.Message.ThreadTS,
		Reaction:     emoji,
		UserID:       ev.User,
		TargetDevice: targetDevice,
		RepoPath:     resolvedRepo,
		UseTmux:      chMap.UseTmux,
		Policy:       cfg.EffectiveChannelPolicy(chMap),
		Harness:      harness,
		SessionName:  DeterministicSessionNameForAccount(acctID, ev.ChannelID, ev.MessageTS),
		Prompt:       FormatTaskPrompt(acctID, ev, msg, resolvedCh),
		Permalink:    msg.Permalink,
	}

	inserted, _, err := store.EnqueueJob(job)
	if err != nil {
		// Persist failed: do NOT ACK!
		return false, "", err
	}
	if inserted {
		_ = store.SetLastDevice(scope, targetDevice)
		return true, job.TargetDevice, nil
	}
	return true, "", nil
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
