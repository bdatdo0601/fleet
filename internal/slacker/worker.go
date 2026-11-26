package slacker

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bdatdo0601/fleet/internal/client"
	"github.com/bdatdo0601/fleet/internal/paths"
	"github.com/bdatdo0601/fleet/internal/registry"
	"github.com/bdatdo0601/fleet/internal/session"
	"github.com/bdatdo0601/fleet/internal/usage"
)

// LocalReceipt persists worker-side launch state for crash/restart reconciliation.
type LocalReceipt struct {
	JobID       string `json:"job_id"`
	ReceiptID   string `json:"receipt_id"`
	SessionName string `json:"session_name"`
	Harness     string `json:"harness"`
	Backend     string `json:"backend"`
	Status      string `json:"status"`
	Error       string `json:"error,omitempty"`
	Acked       bool   `json:"acked"`
	CreatedAt   string `json:"created_at"`
}

// WorkerOpts configures RunWorker.
type WorkerOpts struct {
	APIURL           string
	LocalStateDir    string
	PollTimeout      float64
	UseTmux          bool
	Policy           session.Policy
	Once             bool
	RefreshShellPath bool
	Stderr           io.Writer
	Stdout           io.Writer
	SessionExists    func(harness, name string) (bool, string)
	LaunchFn         func(job Job, useTmux bool, stdout, stderr io.Writer) (string, error)
	KillFn           func(job Job, stdout, stderr io.Writer) (string, error)
}

func expandHomePath(p string) (string, error) {
	if p == "~" {
		return os.UserHomeDir()
	}
	if strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, p[2:]), nil
	}
	return p, nil
}

func defaultLaunchJob(job Job, useTmux bool, stdout, stderr io.Writer) (string, error) {
	dir, err := expandHomePath(job.RepoPath)
	if err != nil {
		return "", err
	}
	pol, err := session.ParsePolicy(job.Policy, session.PolicyAuto)
	if err != nil {
		pol = session.PolicyAuto
	}
	return session.Start(session.StartOpts{
		Harness:   job.Harness,
		Name:      job.SessionName,
		Directory: dir,
		UseTmux:   useTmux || job.UseTmux,
		Model:     job.Model,
		Policy:    pol,
		Prompt:    job.Prompt,
		Stdout:    stdout,
		Stderr:    stderr,
	})
}

func defaultKillJob(job Job, stdout, stderr io.Writer) (string, error) {
	switch job.Reaction {
	case "kill":
		var target session.KillTarget
		if strings.TrimSpace(job.Prompt) == "" || json.Unmarshal([]byte(job.Prompt), &target) != nil {
			target = session.KillTarget{Harness: job.Harness, Name: job.SessionName}
		}
		if target.Harness == "" {
			target.Harness = job.Harness
		}
		return "kill", session.KillAgent(target, stdout, stderr)
	case "kill-all":
		var targets []session.KillTarget
		if strings.TrimSpace(job.Prompt) != "" {
			_ = json.Unmarshal([]byte(job.Prompt), &targets)
		}
		return "kill-all", session.KillAllAgents(job.Harness, targets, stdout, stderr)
	default:
		return "", fmt.Errorf("unsupported kill reaction %q", job.Reaction)
	}
}

func receiptsDir(localStateDir string) string {
	return filepath.Join(localStateDir, "receipts")
}

func receiptPath(localStateDir, jobID string) string {
	return filepath.Join(receiptsDir(localStateDir), jobID+".json")
}

func loadLocalReceipt(path string) (LocalReceipt, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return LocalReceipt{}, false
	}
	var r LocalReceipt
	if json.Unmarshal(raw, &r) != nil {
		return LocalReceipt{}, false
	}
	return r, r.JobID != ""
}

func saveLocalReceipt(path string, r LocalReceipt) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	_, err = paths.WriteFileAtomic(path, append(b, '\n'), 0o600, nil, nil)
	return err
}

func postReceipt(ctx context.Context, api string, r LocalReceipt) error {
	return client.CallContext(ctx, api, http.MethodPost, "/v1/slacker/worker/receipt", Receipt{
		JobID:       r.JobID,
		ReceiptID:   r.ReceiptID,
		SessionName: r.SessionName,
		Backend:     r.Backend,
		Status:      r.Status,
		Error:       r.Error,
	}, 15*time.Second, nil, nil)
}

func replayPendingReceipts(ctx context.Context, api, localStateDir string, stderr io.Writer) {
	dir := receiptsDir(localStateDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".json") {
			continue
		}
		p := filepath.Join(dir, ent.Name())
		r, ok := loadLocalReceipt(p)
		if !ok || r.Acked {
			continue
		}
		if err := postReceipt(ctx, api, r); err != nil {
			_, _ = fmt.Fprintf(stderr, "slacker worker receipt replay failed for %s: %v\n", r.JobID, err)
			continue
		}
		r.Acked = true
		_ = saveLocalReceipt(p, r)
	}
}

func processJob(ctx context.Context, api, localStateDir string, job Job, opts WorkerOpts) error {
	rPath := receiptPath(localStateDir, job.ID)
	if existing, ok := loadLocalReceipt(rPath); ok {
		if !existing.Acked {
			if err := postReceipt(ctx, api, existing); err != nil {
				return err
			}
			existing.Acked = true
			_ = saveLocalReceipt(rPath, existing)
		}
		return nil
	}

	existsFn := opts.SessionExists
	if existsFn == nil {
		existsFn = session.SessionExists
	}
	launchFn := opts.LaunchFn
	if launchFn == nil {
		launchFn = defaultLaunchJob
	}

	rec := LocalReceipt{
		JobID:       job.ID,
		ReceiptID:   fmt.Sprintf("rcpt-%s-%d", job.ID, time.Now().UnixNano()),
		SessionName: job.SessionName,
		Harness:     job.Harness,
		CreatedAt:   registry.NowISO(),
	}

	if job.Reaction == "kill" || job.Reaction == "kill-all" {
		killFn := opts.KillFn
		if killFn == nil {
			killFn = defaultKillJob
		}
		backend, err := killFn(job, opts.Stdout, opts.Stderr)
		if err != nil {
			rec.Status = "failed"
			rec.Error = err.Error()
		} else {
			rec.Status = "launched"
			rec.Backend = cmp.Or(backend, job.Reaction)
		}
		if err := saveLocalReceipt(rPath, rec); err != nil {
			return err
		}
		if err := postReceipt(ctx, api, rec); err != nil {
			return err
		}
		rec.Acked = true
		return saveLocalReceipt(rPath, rec)
	}

	if exists, backend := existsFn(job.Harness, job.SessionName); exists {
		rec.Status = "launched"
		rec.Backend = cmp.Or(backend, "existing")
	} else {
		if job.Policy == "" && opts.Policy != "" {
			job.Policy = string(opts.Policy)
		}
		if opts.RefreshShellPath {
			paths.RefreshProcessPathFromLoginShell(ctx)
		}
		backend, err := launchFn(job, opts.UseTmux, opts.Stdout, opts.Stderr)
		if err != nil {
			rec.Status = "failed"
			rec.Error = err.Error()
		} else {
			rec.Status = "launched"
			rec.Backend = backend
		}
	}

	if err := saveLocalReceipt(rPath, rec); err != nil {
		return err
	}
	if err := postReceipt(ctx, api, rec); err != nil {
		return err
	}
	rec.Acked = true
	return saveLocalReceipt(rPath, rec)
}

// RunWorker runs the target Mac pull worker over Fleet's Nebula API as the logged-in user.
func RunWorker(ctx context.Context, opts WorkerOpts) error {
	api, err := client.ResolveAPI(opts.APIURL)
	if err != nil {
		return err
	}
	localDir := cmp.Or(opts.LocalStateDir, paths.SlackerLocalStateDir())
	stdout := cmp.Or(opts.Stdout, io.Writer(os.Stdout))
	stderr := cmp.Or(opts.Stderr, io.Writer(os.Stderr))
	opts.Stdout = stdout
	opts.Stderr = stderr

	lockPath := filepath.Join(localDir, "worker.lock")
	if err := acquirePIDLock(lockPath, "slacker worker"); err != nil {
		return err
	}
	defer func() { _ = os.Remove(lockPath) }()

	if opts.RefreshShellPath {
		paths.RefreshProcessPathFromLoginShell(ctx)
	}

	pollTimeout := opts.PollTimeout
	if pollTimeout <= 0 {
		pollTimeout = 30.0
	}
	if pollTimeout > 55.0 {
		pollTimeout = 55.0
	}
	if opts.Once && opts.PollTimeout == 0 {
		pollTimeout = 0.2
	}

	if !opts.Once && opts.LaunchFn == nil && os.Getenv("FLEET_DISABLE_USAGE_REPORTER") != "1" {
		usage.StartPeriodicReporter(ctx, 10*time.Minute, api)
	}

	backoff := 2 * time.Second
	for {
		if ctx.Err() != nil {
			return nil
		}
		replayPendingReceipts(ctx, api, localDir, stderr)

		var pollResp struct {
			Job *Job `json:"job"`
		}
		callTimeout := time.Duration((pollTimeout+15.0)*float64(time.Second)) + 5*time.Second
		if err := client.CallContext(ctx, api, http.MethodPost, "/v1/slacker/worker/poll", map[string]any{
			"timeout": pollTimeout,
		}, callTimeout, nil, &pollResp); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			_, _ = fmt.Fprintf(stderr, "slacker worker poll error: %v\n", err)
			if opts.Once {
				return err
			}
			if !sleepContext(ctx, backoff) {
				return nil
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		backoff = 2 * time.Second
		if pollResp.Job != nil {
			if err := processJob(ctx, api, localDir, *pollResp.Job, opts); err != nil {
				_, _ = fmt.Fprintf(stderr, "slacker worker job %s error: %v\n", pollResp.Job.ID, err)
			}
		}
		if opts.Once {
			return nil
		}
	}
}
