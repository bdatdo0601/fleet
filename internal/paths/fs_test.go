package paths

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestEnsureDirAndWriteFileAtomic(t *testing.T) {
	tmp := t.TempDir()
	target := filepath.Join(tmp, "nested", "dir", "config.txt")

	wrote, err := WriteFileAtomic(target, []byte("hello"), 0o600, nil, nil)
	if err != nil || !wrote {
		t.Fatalf("initial WriteFileAtomic failed: wrote=%v err=%v", wrote, err)
	}
	fi, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("expected mode 0600, got %o", fi.Mode().Perm())
	}

	// Corrupt mode while keeping identical content; WriteFileAtomic should repair mode and return (false, nil).
	if err := os.Chmod(target, 0o644); err != nil {
		t.Fatal(err)
	}
	wrote, err = WriteFileAtomic(target, []byte("hello"), 0o600, nil, nil)
	if err != nil || wrote {
		t.Fatalf("mode-repair WriteFileAtomic unexpected: wrote=%v err=%v", wrote, err)
	}
	fi, err = os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("expected repaired mode 0600, got %o", fi.Mode().Perm())
	}

	// EnsureDir applies mode to newly created directories.
	subDir := filepath.Join(tmp, "a", "b")
	if err := EnsureDir(subDir, 0o700, nil, nil); err != nil {
		t.Fatal(err)
	}
	dfi, err := os.Stat(subDir)
	if err != nil || !dfi.IsDir() {
		t.Fatalf("expected directory at %s, err=%v", subDir, err)
	}
	if dfi.Mode().Perm() != 0o700 {
		t.Fatalf("expected dir mode 0700, got %o", dfi.Mode().Perm())
	}
}

func TestWriteFileAtomicConcurrent(t *testing.T) {
	tmp := t.TempDir()
	target := filepath.Join(tmp, "concurrent.txt")

	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := range 16 {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			payload := fmt.Appendf(nil, "worker-%02d-payload\n", id)
			if _, err := WriteFileAtomic(target, payload, 0o640, nil, nil); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent WriteFileAtomic failed: %v", err)
	}

	raw, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(raw, []byte("worker-")) || !bytes.HasSuffix(raw, []byte("-payload\n")) {
		t.Fatalf("corrupted final file content: %q", string(raw))
	}
}

func TestEnvPortBounds(t *testing.T) {
	t.Setenv("FLEET_API_PORT", "8443")
	if got := APIPort(); got != 8443 {
		t.Fatalf("APIPort() = %d, want 8443", got)
	}
	t.Setenv("FLEET_API_PORT", "0")
	if got := APIPort(); got != 7443 {
		t.Fatalf("APIPort(0) = %d, want 7443", got)
	}
	t.Setenv("FLEET_API_PORT", "65536")
	if got := APIPort(); got != 7443 {
		t.Fatalf("APIPort(65536) = %d, want 7443", got)
	}
	t.Setenv("FLEET_HTTPS_PORT", "65535")
	if got := HTTPSPort(); got != 65535 {
		t.Fatalf("HTTPSPort(65535) = %d, want 65535", got)
	}
	t.Setenv("FLEET_DNS_PORT", "-1")
	if got := DNSPort(); got != 53 {
		t.Fatalf("DNSPort(-1) = %d, want 53", got)
	}
}
