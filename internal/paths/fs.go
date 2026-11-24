package paths

import (
	"bytes"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
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

// WriteFileAtomic atomically writes data to path with mode and optional uid/gid ownership.
// Returns (false, nil) if path already has identical content.
func WriteFileAtomic(path string, data []byte, mode os.FileMode, uid, gid *int) (bool, error) {
	if mode == 0 {
		mode = 0o644
	}
	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, data) {
		return false, nil
	}
	dir := filepath.Dir(path)
	if err := EnsureDir(dir, 0o755, uid, gid); err != nil {
		return false, err
	}
	tmp := filepath.Join(dir, fmt.Sprintf(".%s.%d.tmp", filepath.Base(path), os.Getpid()))
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return false, err
	}
	_ = os.Chmod(tmp, mode)
	if os.Geteuid() == 0 && uid != nil && gid != nil {
		_ = os.Chown(tmp, *uid, *gid)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return false, err
	}
	return true, nil
}
