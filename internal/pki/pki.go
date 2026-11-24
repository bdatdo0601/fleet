// Package pki wraps the nebula-cert binary for Nebula PKI operations.
package pki

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/bdatdo0601/fleet/internal/paths"
)

var pubkeyPEMRE = regexp.MustCompile(
	`-----BEGIN NEBULA X25519 PUBLIC KEY-----\s*([A-Za-z0-9+/=\s]+?)\s*-----END NEBULA X25519 PUBLIC KEY-----`,
)

// CertInfo holds parsed certificate details from `nebula-cert print -json`.
type CertInfo struct {
	Name        string
	Fingerprint string
	PublicKey   string
	Groups      []string
	Networks    []string
	NotAfter    string
	IsCA        bool
}

// SignOpts specifies parameters to sign a host certificate.
type SignOpts struct {
	CAKey      string
	CACrt      string
	Pubkey     string
	Name       string
	IP         string
	Group      string
	Passphrase string
	Hours      int
}

func binPath() (string, error) {
	p, err := exec.LookPath("nebula-cert")
	if err != nil {
		return "", errors.New("nebula-cert not found (brew install nebula)")
	}
	return p, nil
}

// Run executes nebula-cert with the given arguments and optional CA passphrase.
func Run(passphrase *string, args ...string) (string, error) {
	bin, err := binPath()
	if err != nil {
		return "", err
	}
	cmd := exec.Command(bin, args...)
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "NEBULA_CA_PASSPHRASE=") {
			env = append(env, kv)
		}
	}
	if passphrase != nil {
		env = append(env, "NEBULA_CA_PASSPHRASE="+*passphrase)
	}
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		msg = strings.TrimPrefix(msg, "Error: ")
		if msg == "" && len(args) > 0 {
			msg = fmt.Sprintf("nebula-cert %s failed", args[0])
		}
		return "", errors.New(msg)
	}
	return stdout.String(), nil
}

// CreateCA creates an encrypted Nebula CA key and certificate.
func CreateCA(keyPath, crtPath, passphrase, name string) error {
	if name == "" {
		name = "fleet"
	}
	_, err := Run(
		&passphrase,
		"ca", "-name", name,
		"-duration", fmt.Sprintf("%dh", paths.CAHours),
		"-encrypt",
		"-networks", paths.MeshCIDR,
		"-out-key", keyPath,
		"-out-crt", crtPath,
	)
	return err
}

// Keygen generates a new Nebula X25519 keypair at keyPath and pubPath.
func Keygen(keyPath, pubPath string) error {
	_, err := Run(nil, "keygen", "-out-key", keyPath, "-out-pub", pubPath)
	return err
}

// NormalizePubkey extracts and validates a canonical Nebula X25519 public key PEM block.
func NormalizePubkey(text string) (string, error) {
	m := pubkeyPEMRE.FindStringSubmatch(text)
	if len(m) < 2 {
		return "", errors.New("not a Nebula public key (expected a NEBULA X25519 PUBLIC KEY block)")
	}
	body := strings.Join(strings.Fields(m[1]), "")
	raw, err := base64.StdEncoding.DecodeString(body)
	if err != nil {
		return "", fmt.Errorf("invalid Nebula public key base64: %w", err)
	}
	if len(raw) != 32 {
		return "", errors.New("public key has the wrong length")
	}
	return "-----BEGIN NEBULA X25519 PUBLIC KEY-----\n" + body + "\n-----END NEBULA X25519 PUBLIC KEY-----\n", nil
}

// PubkeyHex returns the lowercase hex representation of the 32-byte public key inside pem.
func PubkeyHex(pem string) (string, error) {
	m := pubkeyPEMRE.FindStringSubmatch(pem)
	if len(m) < 2 {
		return "", errors.New("not a Nebula public key")
	}
	body := strings.Join(strings.Fields(m[1]), "")
	raw, err := base64.StdEncoding.DecodeString(body)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

// Sign signs a Nebula host certificate using the CA key, cert, and passphrase.
func Sign(opts SignOpts) (string, error) {
	norm, err := NormalizePubkey(opts.Pubkey)
	if err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp("", "fleet-sign-*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	pubFile := filepath.Join(tmp, "host.pub")
	outFile := filepath.Join(tmp, "host.crt")
	if err := os.WriteFile(pubFile, []byte(norm), 0o600); err != nil {
		return "", err
	}
	args := []string{
		"sign",
		"-ca-key", opts.CAKey,
		"-ca-crt", opts.CACrt,
		"-name", opts.Name,
		"-networks", fmt.Sprintf("%s/%d", opts.IP, paths.MeshPrefix),
		"-groups", opts.Group,
		"-in-pub", pubFile,
		"-out-crt", outFile,
	}
	if opts.Hours > 0 {
		args = append(args, "-duration", fmt.Sprintf("%dh", opts.Hours))
	}
	pass := opts.Passphrase
	if _, err := Run(&pass, args...); err != nil {
		return "", err
	}
	crtBytes, err := os.ReadFile(outFile)
	if err != nil {
		return "", err
	}
	return string(crtBytes), nil
}

type rawCertJSON struct {
	Fingerprint string `json:"fingerprint"`
	PublicKey   string `json:"publicKey"`
	Details     struct {
		Name     string   `json:"name"`
		Groups   []string `json:"groups"`
		Networks []string `json:"networks"`
		NotAfter string   `json:"notAfter"`
		IsCA     bool     `json:"isCa"`
	} `json:"details"`
}

// Info inspects a PEM-encoded Nebula certificate via `nebula-cert print -json`.
func Info(certPEM string) (CertInfo, error) {
	tmp, err := os.MkdirTemp("", "fleet-info-*")
	if err != nil {
		return CertInfo{}, err
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	crtFile := filepath.Join(tmp, "c.crt")
	if err := os.WriteFile(crtFile, []byte(certPEM), 0o600); err != nil {
		return CertInfo{}, err
	}
	out, err := Run(nil, "print", "-json", "-path", crtFile)
	if err != nil {
		return CertInfo{}, err
	}
	trimmed := strings.TrimSpace(out)
	var item rawCertJSON
	if strings.HasPrefix(trimmed, "[") {
		var list []rawCertJSON
		if err := json.Unmarshal([]byte(trimmed), &list); err != nil {
			return CertInfo{}, err
		}
		if len(list) == 0 {
			return CertInfo{}, errors.New("empty certificate list")
		}
		item = list[0]
	} else {
		if err := json.Unmarshal([]byte(trimmed), &item); err != nil {
			return CertInfo{}, err
		}
	}
	groups := item.Details.Groups
	if groups == nil {
		groups = []string{}
	}
	nets := item.Details.Networks
	if nets == nil {
		nets = []string{}
	}
	return CertInfo{
		Name:        item.Details.Name,
		Fingerprint: item.Fingerprint,
		PublicKey:   item.PublicKey,
		Groups:      groups,
		Networks:    nets,
		NotAfter:    item.Details.NotAfter,
		IsCA:        item.Details.IsCA,
	}, nil
}

// Verify verifies certPEM against caPEM using `nebula-cert verify`.
func Verify(caPEM, certPEM string) error {
	tmp, err := os.MkdirTemp("", "fleet-verify-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	caFile := filepath.Join(tmp, "ca.crt")
	crtFile := filepath.Join(tmp, "host.crt")
	if err := os.WriteFile(caFile, []byte(caPEM), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(crtFile, []byte(certPEM), 0o600); err != nil {
		return err
	}
	_, err = Run(nil, "verify", "-ca", caFile, "-crt", crtFile)
	return err
}

// HoursUntil returns the whole hours remaining until the RFC3339 timestamp notAfter.
func HoursUntil(notAfter string) (int, error) {
	t, err := time.Parse(time.RFC3339, notAfter)
	if err != nil {
		return 0, err
	}
	return int(math.Floor(time.Until(t).Seconds() / 3600.0)), nil
}
