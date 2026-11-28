package system

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/bdatdo0601/fleet/internal/paths"
)

// FileSHA256 computes lowercase SHA-256 hex digest and size of path.
func FileSHA256(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// SmokeTestBinary verifies binPath executes `<binPath> version` and exits 0.
func SmokeTestBinary(binPath string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, binPath, "version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("smoke test `%s version` failed: %w (%s)", binPath, err, strings.TrimSpace(string(out)))
	}
	if !strings.Contains(string(out), "fleet ") {
		return fmt.Errorf("smoke test `%s version` returned unexpected output: %q", binPath, strings.TrimSpace(string(out)))
	}
	return nil
}

// EnsureSymlink atomically ensures linkPath points to target.
func EnsureSymlink(target, linkPath string) error {
	if linkPath == "" {
		return nil
	}
	if cur, err := os.Readlink(linkPath); err == nil && cur == target {
		return nil
	}
	dir := filepath.Dir(linkPath)
	if err := os.MkdirAll(dir, 0o755); err == nil {
		tmp := filepath.Join(dir, fmt.Sprintf(".%s.%s.sym", filepath.Base(linkPath), randomSuffix()))
		_ = os.Remove(tmp)
		if err := os.Symlink(target, tmp); err == nil {
			if err := os.Rename(tmp, linkPath); err == nil {
				return nil
			}
			_ = os.Remove(tmp)
		}
	}
	if _, _, _, err := Sudo(true, "mkdir", "-p", dir); err != nil {
		return err
	}
	_, _, _, err := Sudo(true, "ln", "-sfn", target, linkPath)
	return err
}

func copyFileAtomic(src, dst string, mode os.FileMode) error {
	if mode == 0 {
		mode = 0o644
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	dir := filepath.Dir(dst)
	uid, gid := rootIDs()
	if err := paths.EnsureDir(dir, 0o755, uid, gid); err != nil {
		return err
	}
	out, err := os.CreateTemp(dir, "."+filepath.Base(dst)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := out.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Chmod(mode); err != nil {
		_ = out.Close()
		return err
	}
	if IsRoot() && uid != nil && gid != nil {
		if err := out.Chown(*uid, *gid); err != nil {
			_ = out.Close()
			return err
		}
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return err
	}
	cleanup = false
	return nil
}

// InstallBinaryAtomic smoke-tests srcPath, saves a .prev backup if targetPath exists,
// atomically replaces targetPath, updates symlinkPath, and allowlists the binary in macOS ALF.
func InstallBinaryAtomic(srcPath, targetPath, symlinkPath string) (string, error) {
	if targetPath == "" {
		targetPath = paths.InstalledBinaryPath()
	}
	if err := SmokeTestBinary(srcPath); err != nil {
		return "", err
	}
	prevPath := ""
	if _, err := os.Stat(targetPath); err == nil {
		prevCandidate := targetPath + ".prev"
		if copyFileAtomic(targetPath, prevCandidate, 0o755) == nil {
			prevPath = prevCandidate
		} else if !IsRoot() {
			_, _, rc, _ := Sudo(false, "install", "-m", "755", "-o", "root", "-g", "wheel", targetPath, prevCandidate)
			if rc == 0 {
				prevPath = prevCandidate
			}
		}
	}
	if err := copyFileAtomic(srcPath, targetPath, 0o755); err != nil {
		if IsRoot() {
			return prevPath, err
		}
		if _, _, _, sErr := Sudo(true, "mkdir", "-p", filepath.Dir(targetPath)); sErr != nil {
			return prevPath, sErr
		}
		tmpDst := targetPath + ".new." + randomSuffix()
		if _, _, _, sErr := Sudo(true, "install", "-m", "755", "-o", "root", "-g", "wheel", srcPath, tmpDst); sErr != nil {
			return prevPath, sErr
		}
		if _, _, _, sErr := Sudo(true, "mv", "-f", tmpDst, targetPath); sErr != nil {
			_, _, _, _ = Sudo(false, "rm", "-f", tmpDst)
			return prevPath, sErr
		}
	}
	if err := EnsureSymlink(targetPath, symlinkPath); err != nil {
		return prevPath, err
	}
	FirewallAllow(targetPath)
	return prevPath, nil
}

// RollbackBinary restores prevPath over targetPath and re-applies FirewallAllow.
func RollbackBinary(targetPath, prevPath string) error {
	if prevPath == "" {
		return nil
	}
	if _, err := os.Stat(prevPath); err != nil {
		return err
	}
	if err := os.Rename(prevPath, targetPath); err != nil {
		if !IsRoot() {
			if _, _, _, sErr := Sudo(true, "mv", "-f", prevPath, targetPath); sErr != nil {
				return sErr
			}
		} else {
			return err
		}
	}
	FirewallAllow(targetPath)
	return nil
}
