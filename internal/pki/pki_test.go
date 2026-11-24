package pki

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestCreateCAKeygenSignInfoVerify(t *testing.T) {
	if _, err := exec.LookPath("nebula-cert"); err != nil {
		t.Skip("nebula-cert not installed")
	}
	dir := t.TempDir()
	caKey := filepath.Join(dir, "ca.key")
	caCrt := filepath.Join(dir, "ca.crt")
	if err := CreateCA(caKey, caCrt, "correct horse battery staple", "fleet"); err != nil {
		t.Fatal(err)
	}
	hostKey := filepath.Join(dir, "host.key")
	hostPub := filepath.Join(dir, "host.pub")
	if err := Keygen(hostKey, hostPub); err != nil {
		t.Fatal(err)
	}
	pubPEMBytes, err := os.ReadFile(hostPub)
	if err != nil {
		t.Fatal(err)
	}
	certPEM, err := Sign(SignOpts{
		CAKey:      caKey,
		CACrt:      caCrt,
		Pubkey:     string(pubPEMBytes),
		Name:       "laptop",
		IP:         "10.213.0.2",
		Group:      "admin",
		Passphrase: "correct horse battery staple",
	})
	if err != nil {
		t.Fatal(err)
	}
	caPEMBytes, err := os.ReadFile(caCrt)
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(string(caPEMBytes), certPEM); err != nil {
		t.Fatalf("verify failed: %v", err)
	}
	info, err := Info(certPEM)
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "laptop" || len(info.Groups) != 1 || info.Groups[0] != "admin" {
		t.Fatalf("unexpected info: %+v", info)
	}
	pubHex, err := PubkeyHex(string(pubPEMBytes))
	if err != nil || pubHex != info.PublicKey {
		t.Fatalf("pubkey mismatch: %q vs %q (err=%v)", pubHex, info.PublicKey, err)
	}
	hrs, err := HoursUntil(info.NotAfter)
	if err != nil || hrs <= 0 {
		t.Fatalf("unexpected HoursUntil: %d (err=%v)", hrs, err)
	}
}
