package mcpinstall

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bdatdo0601/fleet/internal/paths"
)

// StaleMCPServer describes a running fleet MCP server process using legacy fleetctl or an older binary.
type StaleMCPServer struct {
	PID       int
	Harness   string
	StartedAt time.Time
	Legacy    bool
	Reason    string
	Command   string
}

func parseMCPServeCommand(cmdTokens []string) (harness string, legacy, ok bool) {
	if len(cmdTokens) < 3 {
		return "", false, false
	}
	mcpIdx := -1
	for i := range len(cmdTokens) - 1 {
		if cmdTokens[i] == "mcp" && cmdTokens[i+1] == "serve" {
			mcpIdx = i
			break
		}
	}
	if mcpIdx <= 0 {
		return "", false, false
	}
	prefix := cmdTokens[:mcpIdx]
	for _, tok := range prefix {
		if filepath.Base(tok) == "fleetctl" {
			legacy = true
			break
		}
	}
	if !legacy {
		base := filepath.Base(prefix[0])
		if base != "fleet" && base != "fleet.prev" && !strings.HasPrefix(base, "fleet-") {
			return "", false, false
		}
	}
	rest := cmdTokens[mcpIdx+2:]
	for i := 0; i < len(rest); i++ {
		switch {
		case rest[i] == "--harness" && i+1 < len(rest):
			harness = rest[i+1]
			i++
		case strings.HasPrefix(rest[i], "--harness="):
			harness = strings.TrimPrefix(rest[i], "--harness=")
		}
	}
	if !paths.ValidHarness(harness) {
		return "", false, false
	}
	return harness, legacy, true
}

func staleReason(legacy bool, startedAt, binModTime time.Time) (string, bool) {
	if legacy {
		if !startedAt.IsZero() {
			return "running legacy Python fleetctl, started " + startedAt.Format("2006-01-02 15:04"), true
		}
		return "running legacy Python fleetctl", true
	}
	if !binModTime.IsZero() && !startedAt.IsZero() && startedAt.Before(binModTime) {
		return fmt.Sprintf(
			"started %s, older than installed binary (%s)",
			startedAt.Format("2006-01-02 15:04"),
			binModTime.Format("2006-01-02 15:04"),
		), true
	}
	return "", false
}

func parsePSMCPServers(psOutput string, binModTime time.Time, selfPID int, loc *time.Location) []StaleMCPServer {
	if loc == nil {
		loc = time.Local
	}
	var stale []StaleMCPServer
	for line := range strings.SplitSeq(psOutput, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 8 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid <= 0 || pid == selfPID {
			continue
		}
		var startedAt time.Time
		if ts, parseErr := time.ParseInLocation("Mon Jan 2 15:04:05 2006", strings.Join(fields[1:6], " "), loc); parseErr == nil {
			startedAt = ts
		}
		cmdTokens := fields[6:]
		harness, legacy, ok := parseMCPServeCommand(cmdTokens)
		if !ok {
			continue
		}
		reason, isStale := staleReason(legacy, startedAt, binModTime)
		if !isStale {
			continue
		}
		stale = append(stale, StaleMCPServer{
			PID:       pid,
			Harness:   harness,
			StartedAt: startedAt,
			Legacy:    legacy,
			Reason:    reason,
			Command:   strings.Join(cmdTokens, " "),
		})
	}
	slices.SortFunc(stale, func(a, b StaleMCPServer) int {
		if c := cmp.Compare(a.Harness, b.Harness); c != 0 {
			return c
		}
		return cmp.Compare(a.PID, b.PID)
	})
	return stale
}

// FindStaleMCPServers inspects live `fleet mcp serve` processes and returns any running legacy fleetctl or an older binary.
func FindStaleMCPServers(binPath string) []StaleMCPServer {
	binPath = cmp.Or(binPath, paths.LauncherPath())
	var binModTime time.Time
	if fi, err := os.Stat(binPath); err == nil {
		binModTime = fi.ModTime()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ps", "-axo", "pid=,lstart=,command=")
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	return parsePSMCPServers(string(out), binModTime, os.Getpid(), time.Local)
}

// FormatStaleWarning formats a human-readable warning for a stale MCP server process.
func FormatStaleWarning(s StaleMCPServer) string {
	return fmt.Sprintf("stale MCP server (pid %d, %s) — reload MCP servers or restart %s", s.PID, s.Reason, s.Harness)
}
