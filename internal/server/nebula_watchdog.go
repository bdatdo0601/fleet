package server

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bdatdo0601/fleet/internal/core"
	"github.com/bdatdo0601/fleet/internal/registry"
)

const (
	defaultNebulaLogPath     = "/opt/homebrew/var/log/nebula.log"
	defaultWatchdogInterval  = 15 * time.Second
	defaultBurstWindow       = 60 * time.Second
	defaultBurstThreshold    = 5
	defaultWatchdogCooldown  = 10 * time.Minute
	maxWatchdogReadBytes     = 256 * 1024
	relayStaleErrorSubstring = "unable to find host with relay"
)

// NebulaWatchdog tails the lighthouse Nebula log for bursts of stale-relay errors
// and requests a Nebula restart when a burst exceeds the configured threshold.
type NebulaWatchdog struct {
	StateDir       string
	LogPath        string
	PollInterval   time.Duration
	BurstWindow    time.Duration
	BurstThreshold int
	Cooldown       time.Duration

	mu          sync.Mutex
	initialized bool
	inode       uint64
	offset      int64
	recent      []time.Time
	lastTrigger time.Time
	lastError   string
}

// NewNebulaWatchdog returns a NebulaWatchdog with production defaults for stateDir.
func NewNebulaWatchdog(stateDir string) *NebulaWatchdog {
	return &NebulaWatchdog{
		StateDir:       stateDir,
		LogPath:        defaultNebulaLogPath,
		PollInterval:   defaultWatchdogInterval,
		BurstWindow:    defaultBurstWindow,
		BurstThreshold: defaultBurstThreshold,
		Cooldown:       defaultWatchdogCooldown,
	}
}

func (w *NebulaWatchdog) logPath() string {
	return cmp.Or(w.LogPath, defaultNebulaLogPath)
}

func (w *NebulaWatchdog) pollInterval() time.Duration {
	if w.PollInterval > 0 {
		return w.PollInterval
	}
	return defaultWatchdogInterval
}

func (w *NebulaWatchdog) burstWindow() time.Duration {
	if w.BurstWindow > 0 {
		return w.BurstWindow
	}
	return defaultBurstWindow
}

func (w *NebulaWatchdog) burstThreshold() int {
	if w.BurstThreshold > 0 {
		return w.BurstThreshold
	}
	return defaultBurstThreshold
}

func (w *NebulaWatchdog) cooldown() time.Duration {
	if w.Cooldown > 0 {
		return w.Cooldown
	}
	return defaultWatchdogCooldown
}

func fileInode(info os.FileInfo) uint64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return st.Ino
	}
	return 0
}

// Init positions the log cursor at the current EOF so pre-existing log lines do not trigger on startup.
func (w *NebulaWatchdog) Init() {
	w.mu.Lock()
	defer w.mu.Unlock()
	info, err := os.Stat(w.logPath())
	if err != nil {
		w.initialized = false
		w.offset = 0
		w.inode = 0
		return
	}
	w.initialized = true
	w.offset = info.Size()
	w.inode = fileInode(info)
}

// Start initializes the log cursor and polls the Nebula log until ctx is canceled.
func (w *NebulaWatchdog) Start(ctx context.Context) {
	w.Init()
	go func() {
		ticker := time.NewTicker(w.pollInterval())
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				w.CheckOnce(now)
			}
		}
	}()
}

// State returns a snapshot of the watchdog's configuration and runtime counters.
func (w *NebulaWatchdog) State() core.NebulaWatchdogState {
	w.mu.Lock()
	defer w.mu.Unlock()
	st := core.NebulaWatchdogState{
		Enabled:        true,
		LogPath:        w.logPath(),
		BurstThreshold: w.burstThreshold(),
		BurstWindowSec: int(w.burstWindow().Seconds()),
		CooldownSec:    int(w.cooldown().Seconds()),
		RecentErrors:   len(w.recent),
		LastError:      w.lastError,
	}
	if !w.lastTrigger.IsZero() {
		st.LastTriggerAt = w.lastTrigger.UTC().Truncate(time.Second).Format("2006-01-02T15:04:05+00:00")
	}
	return st
}

func extractRelayField(line, key string) string {
	idx := strings.Index(line, key+"=")
	if idx < 0 {
		return ""
	}
	rest := line[idx+len(key)+1:]
	if end := strings.IndexByte(rest, ' '); end >= 0 {
		rest = rest[:end]
	}
	return strings.Trim(rest, `"`)
}

func formatWatchdogReason(line string) string {
	from := extractRelayField(line, "relayFrom")
	to := extractRelayField(line, "relayTo")
	if from != "" && to != "" {
		return fmt.Sprintf("relay watchdog: %s (%s -> %s)", relayStaleErrorSubstring, from, to)
	}
	return "relay watchdog: " + relayStaleErrorSubstring
}

func (w *NebulaWatchdog) readNewLines() ([]string, error) {
	path := w.logPath()
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			w.initialized = false
			w.offset = 0
			w.inode = 0
			return nil, nil
		}
		return nil, err
	}
	ino := fileInode(info)
	if !w.initialized || info.Size() < w.offset || (w.inode != 0 && ino != 0 && ino != w.inode) {
		w.offset = 0
		w.inode = ino
		w.initialized = true
	}
	if info.Size() == w.offset {
		return nil, nil
	}

	start := w.offset
	skipPartialFirst := false
	if info.Size()-start > maxWatchdogReadBytes {
		start = info.Size() - maxWatchdogReadBytes
		skipPartialFirst = true
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	if start > 0 {
		if _, err := f.Seek(start, io.SeekStart); err != nil {
			return nil, err
		}
	}
	buf, err := io.ReadAll(io.LimitReader(f, maxWatchdogReadBytes))
	if err != nil {
		return nil, err
	}
	text := strings.ReplaceAll(string(buf), "\r\n", "\n")
	lastNL := strings.LastIndexByte(text, '\n')
	if lastNL < 0 {
		return nil, nil
	}
	w.offset = start + int64(lastNL+1)
	text = text[:lastNL]
	if skipPartialFirst {
		if _, after, ok := strings.Cut(text, "\n"); ok {
			text = after
		} else {
			return nil, nil
		}
	}
	if text == "" {
		return nil, nil
	}
	return strings.Split(text, "\n"), nil
}

// CheckOnce inspects newly appended Nebula log lines and queues a restart when a burst is detected.
func (w *NebulaWatchdog) CheckOnce(now time.Time) bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	lines, err := w.readNewLines()
	if err != nil {
		w.lastError = err.Error()
		return false
	}
	w.lastError = ""

	var sampleLine string
	for _, line := range lines {
		if strings.Contains(line, relayStaleErrorSubstring) {
			w.recent = append(w.recent, now)
			sampleLine = line
		}
	}

	cutoff := now.Add(-w.burstWindow())
	kept := w.recent[:0]
	for _, ts := range w.recent {
		if !ts.Before(cutoff) {
			kept = append(kept, ts)
		}
	}
	w.recent = kept

	if len(w.recent) < w.burstThreshold() {
		return false
	}
	if !w.lastTrigger.IsZero() && now.Sub(w.lastTrigger) < w.cooldown() {
		return false
	}
	pending, pErr := core.PendingNebulaRestart(w.StateDir)
	if pErr != nil || pending != nil {
		return false
	}

	reason := formatWatchdogReason(sampleLine)
	if _, err := core.RequestNebulaRestart(w.StateDir, "watchdog", reason); err != nil {
		w.lastError = err.Error()
		return false
	}
	w.lastTrigger = now
	w.recent = nil
	_, _ = fmt.Fprintf(os.Stderr, "%s %s\n", registry.NowISO(), reason)
	return true
}
