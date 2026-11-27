package system

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bdatdo0601/fleet/internal/paths"
	"github.com/bdatdo0601/fleet/internal/tlsdns"
)

const fleetResolverHeader = "# managed by fleet\n"

func isFleetManagedResolver(path string) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return strings.HasPrefix(string(raw), fleetResolverHeader)
}

func removeRootFile(path string) error {
	err := os.Remove(path)
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if !IsRoot() && errors.Is(err, os.ErrPermission) {
		_, stderrStr, rc, sErr := Sudo(false, "rm", "-f", path)
		if sErr != nil || rc != 0 {
			return fmt.Errorf("remove %s failed: %w (%s)", path, sErr, strings.TrimSpace(stderrStr))
		}
		return nil
	}
	return err
}

func ensureRootDir(dir string) error {
	err := os.MkdirAll(dir, 0o755)
	if err == nil {
		return nil
	}
	if !IsRoot() && errors.Is(err, os.ErrPermission) {
		_, stderrStr, rc, sErr := Sudo(false, "mkdir", "-p", dir)
		if sErr != nil || rc != 0 {
			return fmt.Errorf("mkdir %s failed: %w (%s)", dir, sErr, strings.TrimSpace(stderrStr))
		}
		return nil
	}
	return err
}

func cleanupStaleResolvers(resolverDir, keepName string) (bool, error) {
	entries, err := os.ReadDir(resolverDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	removed := false
	for _, ent := range entries {
		if ent.IsDir() || ent.Name() == keepName {
			continue
		}
		full := filepath.Join(resolverDir, ent.Name())
		if !isFleetManagedResolver(full) {
			continue
		}
		if err := removeRootFile(full); err != nil {
			return removed, err
		}
		removed = true
	}
	return removed, nil
}

// SyncMacResolver creates or removes the fleet-managed macOS /etc/resolver/<domain> split-DNS file.
// Files in resolverDir that do not begin with "# managed by fleet\n" are never modified or removed.
func SyncMacResolver(resolverDir, domain, nameserver string) (bool, string, error) {
	if resolverDir == "" {
		return false, "", errors.New("resolver directory is required")
	}
	if strings.TrimSpace(domain) == "" {
		removed, err := cleanupStaleResolvers(resolverDir, "")
		if err != nil {
			return false, "", err
		}
		if removed {
			return true, "removed fleet resolver", nil
		}
		return false, "", nil
	}
	norm, err := tlsdns.NormalizeDomain(domain)
	if err != nil {
		return false, "", err
	}
	if strings.TrimSpace(nameserver) == "" {
		return false, "", errors.New("nameserver is required")
	}
	if err := ensureRootDir(resolverDir); err != nil {
		return false, "", err
	}
	target := filepath.Join(resolverDir, norm)
	content := fmt.Sprintf("%snameserver %s\n", fleetResolverHeader, strings.TrimSpace(nameserver))
	var wrote bool
	if IsRoot() || resolverDir == paths.ResolverDir {
		wrote, err = WriteRootFile(target, content, 0o644)
	} else {
		wrote, err = paths.WriteFileAtomic(target, []byte(content), 0o644, nil, nil)
	}
	if err != nil {
		return false, "", err
	}
	removed, err := cleanupStaleResolvers(resolverDir, norm)
	if err != nil {
		return wrote, "", err
	}
	if wrote {
		return true, fmt.Sprintf("resolver(%s)", norm), nil
	}
	if removed {
		return true, "cleaned stale resolver", nil
	}
	return false, "", nil
}
