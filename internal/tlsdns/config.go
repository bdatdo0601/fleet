// Package tlsdns implements Route 53 ACME DNS-01 certificates, private mesh DNS, and the HTTPS edge proxy.
package tlsdns

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bdatdo0601/fleet/internal/paths"
)

const (
	// HeaderEdgeToken authenticates loopback proxy requests from `fleet edge` to `fleet serve`.
	HeaderEdgeToken = "X-Fleet-Edge-Token"
	// HeaderClientIP carries the verified Nebula client IP from `fleet edge` to `fleet serve`.
	HeaderClientIP = "X-Fleet-Client-IP"

	edgeTokenBytes = 32
)

// Config configures public TLS certificates and private mesh DNS.
type Config struct {
	Domain      string   `json:"domain"`
	ZoneID      string   `json:"zone_id"`
	Profile     string   `json:"profile"`
	Email       string   `json:"email,omitempty"`
	Nameservers []string `json:"nameservers,omitempty"`
	PublicA     bool     `json:"public_a"`
	PrivateDNS  bool     `json:"private_dns"`
	Staging     bool     `json:"staging,omitempty"`
}

// Paths holds on-disk TLS certificate and ACME account file locations.
type Paths struct {
	Dir               string
	Fullchain         string
	Privkey           string
	ProdAccountKey    string
	StagingAccountKey string
}

// CertPaths computes TLS file locations under stateDir/tls.
func CertPaths(stateDir string) Paths {
	dir := paths.TLSDir(stateDir)
	return Paths{
		Dir:               dir,
		Fullchain:         filepath.Join(dir, "fullchain.pem"),
		Privkey:           filepath.Join(dir, "privkey.pem"),
		ProdAccountKey:    filepath.Join(dir, "account.key"),
		StagingAccountKey: filepath.Join(dir, "account-staging.key"),
	}
}

// AccountKey returns the ACME account key path for staging or production.
func (p Paths) AccountKey(staging bool) string {
	if staging {
		return p.StagingAccountKey
	}
	return p.ProdAccountKey
}

// NormalizeDomain lowercases, trims trailing dots, and validates a DNS FQDN.
func NormalizeDomain(raw string) (string, error) {
	d := strings.ToLower(strings.TrimSpace(raw))
	d = strings.TrimRight(d, ".")
	if d == "" || len(d) > 253 {
		return "", fmt.Errorf("invalid domain %q", raw)
	}
	if strings.ContainsAny(d, "/\\ \t\r\n:*?") {
		return "", fmt.Errorf("invalid domain %q", raw)
	}
	labels := strings.Split(d, ".")
	if len(labels) < 2 {
		return "", fmt.Errorf("domain %q must be a fully qualified domain name (e.g. fleet.example.com)", raw)
	}
	for _, lbl := range labels {
		if !validDNSLabel(lbl) {
			return "", fmt.Errorf("invalid DNS label %q in domain %q", lbl, raw)
		}
	}
	return d, nil
}

func validDNSLabel(lbl string) bool {
	if len(lbl) == 0 || len(lbl) > 63 {
		return false
	}
	if lbl[0] == '-' || lbl[len(lbl)-1] == '-' {
		return false
	}
	for i := 0; i < len(lbl); i++ {
		c := lbl[i]
		isLower := c >= 'a' && c <= 'z'
		isDigit := c >= '0' && c <= '9'
		if !isLower && !isDigit && c != '-' {
			return false
		}
	}
	return true
}

func isValidHexToken(s string) bool {
	if len(s) != edgeTokenBytes*2 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// ReadEdgeToken reads the loopback edge authentication token from stateDir/edge.token.
func ReadEdgeToken(stateDir string) (string, error) {
	raw, err := os.ReadFile(paths.EdgeTokenPath(stateDir))
	if err != nil {
		return "", err
	}
	tok := strings.TrimSpace(string(raw))
	if !isValidHexToken(tok) {
		return "", fmt.Errorf("invalid edge token in %s", paths.EdgeTokenPath(stateDir))
	}
	return tok, nil
}

// EnsureEdgeToken returns the existing stateDir/edge.token or generates a fresh 0600 secret.
func EnsureEdgeToken(stateDir string) (string, error) {
	if tok, err := ReadEdgeToken(stateDir); err == nil {
		return tok, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		if _, statErr := os.Stat(paths.EdgeTokenPath(stateDir)); statErr != nil {
			return "", err
		}
		// Overwrite corrupt token atomically below.
	}
	buf := make([]byte, edgeTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(buf)
	tokPath := paths.EdgeTokenPath(stateDir)
	if err := os.MkdirAll(filepath.Dir(tokPath), 0o700); err != nil {
		return "", err
	}
	if err := writeAtomicFile(tokPath, []byte(tok+"\n"), 0o600); err != nil {
		return "", err
	}
	return tok, nil
}

func writeAtomicFile(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}
