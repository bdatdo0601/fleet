package paths

import (
	"bytes"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
)

// LookupUIDGID returns numeric uid/gid for username.
func LookupUIDGID(username string) (int, int, error) {
	u, err := user.Lookup(username)
	if err != nil {
		return 0, 0, err
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return 0, 0, err
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return 0, 0, err
	}
	return uid, gid, nil
}

// EnsureDir creates dir and any missing parents, setting mode and optional ownership on newly created directories.
func EnsureDir(dir string, mode os.FileMode, uid, gid *int) error {
	if mode == 0 {
		mode = 0o755
	}
	var missing []string
	for cur := dir; ; cur = filepath.Dir(cur) {
		if _, err := os.Stat(cur); err == nil {
			break
		}
		missing = append(missing, cur)
		if filepath.Dir(cur) == cur {
			break
		}
	}
	if err := os.MkdirAll(dir, mode); err != nil {
		return err
	}
	isRoot := os.Geteuid() == 0
	for _, d := range slices.Backward(missing) {
		_ = os.Chmod(d, mode)
		if isRoot && uid != nil && gid != nil {
			_ = os.Chown(d, *uid, *gid)
		}
	}
	return nil
}

func ensureFilePerms(path string, mode os.FileMode, uid, gid *int) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if fi.Mode().Perm() != mode.Perm() {
		if err := os.Chmod(path, mode); err != nil {
			return err
		}
	}
	if os.Geteuid() == 0 && uid != nil && gid != nil {
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok || int(st.Uid) != *uid || int(st.Gid) != *gid {
			if err := os.Chown(path, *uid, *gid); err != nil {
				return err
			}
		}
	}
	return nil
}

// WriteFileAtomic atomically writes data to path with mode and optional uid/gid ownership.
// Returns (false, nil) if path already has identical content (still verifying/repairing mode and ownership).
func WriteFileAtomic(path string, data []byte, mode os.FileMode, uid, gid *int) (bool, error) {
	if mode == 0 {
		mode = 0o644
	}
	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, data) {
		if err := ensureFilePerms(path, mode, uid, gid); err != nil {
			return false, err
		}
		return false, nil
	}
	dir := filepath.Dir(path)
	if err := EnsureDir(dir, 0o755, uid, gid); err != nil {
		return false, err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return false, err
	}
	tmp := f.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return false, err
	}
	if err := f.Chmod(mode); err != nil {
		_ = f.Close()
		return false, err
	}
	if os.Geteuid() == 0 && uid != nil && gid != nil {
		if err := f.Chown(*uid, *gid); err != nil {
			_ = f.Close()
			return false, err
		}
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return false, err
	}
	if err := f.Close(); err != nil {
		return false, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return false, err
	}
	cleanup = false
	return true, nil
}
