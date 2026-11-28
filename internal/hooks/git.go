package hooks

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/bdatdo0601/fleet/internal/agents"
)

// GitContext returns (repo, branch, shortStatus) for cwd without blocking longer than 1.5s.
func GitContext(cwd string) (string, string, string) {
	if cwd == "" {
		return "", "", ""
	}
	info, err := os.Stat(cwd)
	if err != nil || !info.IsDir() {
		return filepath.Base(cwd), "", ""
	}
	repo := filepath.Base(cwd)
	var branch, gitStatus string

	ctx1, cancel1 := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel1()
	topOut, err := exec.CommandContext(ctx1, "git", "-C", cwd, "rev-parse", "--show-toplevel", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return repo, "", ""
	}
	var lines []string
	for ln := range strings.SplitSeq(string(topOut), "\n") {
		if t := strings.TrimSpace(ln); t != "" {
			lines = append(lines, t)
		}
	}
	if len(lines) > 0 {
		repo = filepath.Base(lines[0])
	}
	if len(lines) > 1 {
		branch = lines[1]
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel2()
	if stOut, err := exec.CommandContext(ctx2, "git", "-C", cwd, "status", "--short").Output(); err == nil && strings.TrimSpace(string(stOut)) != "" {
		stLines := strings.Split(string(stOut), "\n")
		if len(stLines) > 25 {
			stLines = stLines[:25]
		}
		gitStatus = agents.TruncateRunes(strings.Join(stLines, "\n"), 1500)
	}
	return repo, branch, gitStatus
}
