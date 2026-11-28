package system

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/bdatdo0601/fleet/internal/client"
	"github.com/bdatdo0601/fleet/internal/core"
	"github.com/bdatdo0601/fleet/internal/paths"
	"github.com/bdatdo0601/fleet/internal/version"
)

// LighthouseUpdateOpts configures SelfUpdateLighthouse.
type LighthouseUpdateOpts struct {
	RepoDir            string
	StateDir           string
	Owner              string
	TargetBin          string
	SymlinkPath        string
	LocalAPI           string
	Rebuild            bool
	HealthcheckTimeout time.Duration
	SkipDaemonControl  bool // for unit tests without launchd
	HealthcheckFn      func(apiURL string) bool
}

func waitHealthy(apiURL string, timeout time.Duration, checkFn func(string) bool) bool {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	if checkFn == nil {
		hc := &http.Client{Timeout: 2 * time.Second}
		checkFn = func(u string) bool {
			resp, err := hc.Get(strings.TrimRight(u, "/") + "/v1/sync")
			if err != nil {
				return false
			}
			_ = resp.Body.Close()
			return resp.StatusCode == http.StatusOK
		}
	}
	deadline := time.Now().Add(timeout)
	for {
		if checkFn(apiURL) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func writeOwnerFile(path string, data []byte, mode os.FileMode, owner string) error {
	uid, gid := OwnerIDs(owner)
	_, err := paths.WriteFileAtomic(path, data, mode, uid, gid)
	return err
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func resolveLighthousePullTarget(repoDir, owner string, gitEnv map[string]string) (string, string) {
	branchOut, _, err := RunAsOwner(owner, repoDir, gitEnv, 15*time.Second, "git", "-C", repoDir, "symbolic-ref", "--short", "HEAD")
	branch := strings.TrimSpace(branchOut)
	if err != nil || branch == "" {
		return "", ""
	}
	remote := "origin"
	if remoteOut, _, err := RunAsOwner(owner, repoDir, gitEnv, 15*time.Second, "git", "-C", repoDir, "config", "--get", "branch."+branch+".remote"); err == nil {
		if r := strings.TrimSpace(remoteOut); r != "" {
			remote = r
		}
	}
	mergeRef := branch
	if mergeOut, _, err := RunAsOwner(owner, repoDir, gitEnv, 15*time.Second, "git", "-C", repoDir, "config", "--get-all", "branch."+branch+".merge"); err == nil {
		var unique []string
		seen := make(map[string]struct{})
		for line := range strings.SplitSeq(mergeOut, "\n") {
			ref := strings.TrimSpace(line)
			if ref == "" {
				continue
			}
			if _, ok := seen[ref]; !ok {
				seen[ref] = struct{}{}
				unique = append(unique, ref)
			}
		}
		if len(unique) == 1 {
			mergeRef = unique[0]
		}
	}
	return remote, mergeRef
}

func pullLighthouseRepo(repoDir, owner string) (string, error) {
	gitEnv := map[string]string{"GIT_TERMINAL_PROMPT": "0", "GIT_SSH_COMMAND": "ssh -o BatchMode=yes"}
	if !fileExists(filepath.Join(repoDir, ".git")) {
		return "", nil
	}
	args := []string{"-C", repoDir, "-c", "pull.rebase=false", "pull", "--no-rebase", "--ff-only", "-q"}
	if remote, mergeRef := resolveLighthousePullTarget(repoDir, owner, gitEnv); remote != "" && mergeRef != "" {
		args = append(args, remote, mergeRef)
	}
	var pullErr error
	if _, pullErrOut, err := RunAsOwner(owner, repoDir, gitEnv, 60*time.Second, "git", args...); err != nil {
		detail := cmp.Or(strings.TrimSpace(pullErrOut), err.Error())
		pullErr = fmt.Errorf("git pull failed: %s", detail)
	}
	out, revErrOut, err := RunAsOwner(owner, repoDir, gitEnv, 15*time.Second, "git", "-C", repoDir, "rev-parse", "HEAD")
	if err != nil {
		if pullErr != nil {
			return "", pullErr
		}
		detail := cmp.Or(strings.TrimSpace(revErrOut), err.Error())
		return "", fmt.Errorf("git rev-parse HEAD failed: %s", detail)
	}
	return strings.TrimSpace(out), pullErr
}

func buildDarwinBinaries(repoDir, binDir, owner, headSHA, builtAt string) (map[string]string, error) {
	goBin := FindGoBinary()
	if goBin == "" {
		return nil, fmt.Errorf("go not found on lighthouse (install with: brew install go)")
	}
	uid, gid := OwnerIDs(owner)
	if err := paths.EnsureDir(binDir, 0o755, uid, gid); err != nil {
		return nil, err
	}
	ldflags := fmt.Sprintf(
		"-s -w -X github.com/bdatdo0601/fleet/internal/version.Commit=%s -X github.com/bdatdo0601/fleet/internal/version.BuiltAt=%s",
		cmp.Or(headSHA, "dev"), builtAt,
	)
	tmpBins := map[string]string{}
	for _, arch := range []string{"arm64", "amd64"} {
		tmpOut := filepath.Join(binDir, fmt.Sprintf(".fleet-darwin-%s.%s.tmp", arch, randomSuffix()))
		tmpBins[arch] = tmpOut
		buildEnv := map[string]string{"CGO_ENABLED": "0", "GOOS": "darwin", "GOARCH": arch, "GOTOOLCHAIN": "auto"}
		if _, stderrStr, err := RunAsOwner(owner, repoDir, buildEnv, 240*time.Second, goBin, "build", "-ldflags", ldflags, "-o", tmpOut, "./cmd/fleet"); err != nil {
			for _, p := range tmpBins {
				_ = os.Remove(p)
			}
			return nil, fmt.Errorf("go build darwin/%s failed: %w (%s)", arch, err, strings.TrimSpace(stderrStr))
		}
	}
	return tmpBins, nil
}

func restartAndVerifyLighthouse(opts LighthouseUpdateOpts, targetBin, prevBin, owner, stateDir string) error {
	ensureServe := func() error {
		if opts.SkipDaemonControl {
			return nil
		}
		if LaunchdLoaded(paths.ServeLabel) && RestartService(paths.ServeLabel) == nil {
			return nil
		}
		return InstallServeDaemon(targetBin, owner, stateDir, stateDir)
	}
	rollbackServe := func(cause error) error {
		errs := []error{cause}
		if rbErr := RollbackBinary(targetBin, prevBin); rbErr != nil {
			errs = append(errs, fmt.Errorf("rollback binary: %w", rbErr))
		}
		if srvErr := ensureServe(); srvErr != nil {
			errs = append(errs, fmt.Errorf("restore serve daemon: %w", srvErr))
		}
		return errors.Join(errs...)
	}

	var daemonErrs []error
	if !opts.SkipDaemonControl {
		if err := InstallServeDaemon(targetBin, owner, stateDir, stateDir); err != nil {
			return rollbackServe(err)
		}
		if LaunchdLoaded(paths.DDNSLabel) {
			if err := InstallDDNSDaemon(targetBin, owner, stateDir, stateDir); err != nil {
				daemonErrs = append(daemonErrs, fmt.Errorf("ddns daemon: %w", err))
			}
		}
		if LaunchdLoaded(paths.SlackerLabel) {
			if err := InstallSlackerDaemon(targetBin, owner, stateDir, stateDir); err != nil {
				daemonErrs = append(daemonErrs, fmt.Errorf("slacker daemon: %w", err))
			}
		}
		if LaunchdLoaded(paths.EdgeLabel) {
			if err := InstallEdgeDaemon(targetBin, stateDir, stateDir); err != nil {
				daemonErrs = append(daemonErrs, fmt.Errorf("edge daemon: %w", err))
			}
		}
	}
	if !opts.SkipDaemonControl || opts.HealthcheckFn != nil {
		localAPI := cmp.Or(opts.LocalAPI, fmt.Sprintf("http://127.0.0.1:%d", paths.APIPort()))
		if !waitHealthy(localAPI, opts.HealthcheckTimeout, opts.HealthcheckFn) {
			errs := append([]error{fmt.Errorf("new fleet serve failed health check; rolled back")}, daemonErrs...)
			return rollbackServe(errors.Join(errs...))
		}
	}
	if len(daemonErrs) > 0 {
		return errors.Join(daemonErrs...)
	}
	return nil
}

func publishManifest(binDir, owner, headSHA, builtAt string, tmpBins map[string]string) error {
	uid, gid := OwnerIDs(owner)
	manifest := core.BinaryManifest{Version: version.Version, Commit: headSHA, BuiltAt: builtAt, Binaries: map[string]core.BinaryEntry{}}
	for _, arch := range []string{"arm64", "amd64"} {
		finalName := "fleet-darwin-" + arch
		finalPath := filepath.Join(binDir, finalName)
		if err := os.Rename(tmpBins[arch], finalPath); err != nil {
			return err
		}
		_ = os.Chmod(finalPath, 0o755)
		if uid != nil && gid != nil {
			_ = os.Chown(finalPath, *uid, *gid)
		}
		sha, sz, err := FileSHA256(finalPath)
		if err != nil {
			return err
		}
		manifest.Binaries["darwin/"+arch] = core.BinaryEntry{File: finalName, SHA256: sha, Size: sz}
	}
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return writeOwnerFile(filepath.Join(binDir, "manifest.json"), append(manifestBytes, '\n'), 0o644, owner)
}

// SelfUpdateLighthouse pulls git, builds darwin/arm64 & darwin/amd64 when needed,
// installs and healthchecks `fleet serve` (rolling back on failure), and publishes manifest.json only when healthy.
func SelfUpdateLighthouse(opts LighthouseUpdateOpts) (bool, error) {
	if opts.RepoDir == "" || !fileExists(filepath.Join(opts.RepoDir, "go.mod")) {
		return false, nil
	}
	owner := opts.Owner
	if owner == "" {
		owner, _ = PathOwner(opts.RepoDir)
	}
	stateDir := cmp.Or(opts.StateDir, paths.StateDirForOwner(owner))
	targetBin := cmp.Or(opts.TargetBin, paths.InstalledBinaryPath())
	symlinkPath := cmp.Or(opts.SymlinkPath, paths.SymlinkPath())

	headSHA, pullErr := pullLighthouseRepo(opts.RepoDir, owner)
	if pullErr != nil {
		return false, pullErr
	}
	binDir := filepath.Join(stateDir, "bin")
	var existing core.BinaryManifest
	manifestOK := false
	if raw, err := os.ReadFile(filepath.Join(binDir, "manifest.json")); err == nil {
		manifestOK = json.Unmarshal(raw, &existing) == nil && len(existing.Binaries) > 0
	}
	if !opts.Rebuild && fileExists(targetBin) && manifestOK && (headSHA == "" || existing.Commit == headSHA) {
		return false, nil
	}

	builtAt := time.Now().UTC().Format(time.RFC3339)
	tmpBins, err := buildDarwinBinaries(opts.RepoDir, binDir, owner, headSHA, builtAt)
	if err != nil {
		return false, err
	}
	cleanupTmp := func() {
		for _, p := range tmpBins {
			_ = os.Remove(p)
		}
	}
	hostTmp, ok := tmpBins[runtime.GOARCH]
	if !ok {
		cleanupTmp()
		return false, fmt.Errorf("unsupported host arch %s", runtime.GOARCH)
	}
	prevBin, err := InstallBinaryAtomic(hostTmp, targetBin, symlinkPath)
	if err != nil {
		cleanupTmp()
		return false, err
	}
	if err := restartAndVerifyLighthouse(opts, targetBin, prevBin, owner, stateDir); err != nil {
		cleanupTmp()
		return false, err
	}
	if err := publishManifest(binDir, owner, headSHA, builtAt, tmpBins); err != nil {
		cleanupTmp()
		return false, err
	}
	return true, nil
}

// ClientUpdateOpts configures SelfUpdateClient.
type ClientUpdateOpts struct {
	APIURL      string
	ExpectedSHA string
	TargetBin   string
	SymlinkPath string
}

// SelfUpdateClient downloads and atomically installs a new binary from the lighthouse when ExpectedSHA differs.
func SelfUpdateClient(opts ClientUpdateOpts) (bool, error) {
	if opts.ExpectedSHA == "" {
		return false, nil
	}
	targetBin := opts.TargetBin
	if targetBin == "" {
		targetBin = paths.InstalledBinaryPath()
	}
	symlinkPath := opts.SymlinkPath
	if symlinkPath == "" {
		symlinkPath = paths.SymlinkPath()
	}
	if curSHA, _, err := FileSHA256(targetBin); err == nil && strings.EqualFold(curSHA, opts.ExpectedSHA) {
		_ = EnsureSymlink(targetBin, symlinkPath)
		return false, nil
	}

	tmpDir, err := os.MkdirTemp("", "fleet-dl-*")
	if err != nil {
		return false, err
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()
	dlPath := filepath.Join(tmpDir, "fleet.new")
	dlQuery := fmt.Sprintf("/v1/binary?os=%s&arch=%s", runtime.GOOS, runtime.GOARCH)
	gotSHA, _, err := client.Download(opts.APIURL, dlQuery, dlPath, 60*time.Second)
	if err != nil {
		return false, err
	}
	if !strings.EqualFold(gotSHA, opts.ExpectedSHA) {
		return false, fmt.Errorf("downloaded binary SHA mismatch: expected %s, got %s", opts.ExpectedSHA, gotSHA)
	}
	if err := os.Chmod(dlPath, 0o755); err != nil {
		return false, err
	}
	if _, err := InstallBinaryAtomic(dlPath, targetBin, symlinkPath); err != nil {
		return false, err
	}
	return true, nil
}
