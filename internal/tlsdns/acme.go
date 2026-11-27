package tlsdns

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/acme"

	"github.com/bdatdo0601/fleet/internal/ddns"
	"github.com/bdatdo0601/fleet/internal/paths"
)

const (
	// LetsEncryptProdURL is the Let's Encrypt v2 production directory URL.
	LetsEncryptProdURL = acme.LetsEncryptURL
	// LetsEncryptStagingURL is the Let's Encrypt v2 staging directory URL.
	LetsEncryptStagingURL = "https://acme-staging-v02.api.letsencrypt.org/directory"
	// RenewBefore triggers automatic renewal when fewer than 30 days remain.
	RenewBefore = 30 * 24 * time.Hour

	renewCheckInterval = 12 * time.Hour
	startupCheckDelay  = 2 * time.Second
)

// CertStatus describes the on-disk TLS certificate state for the configured domain.
type CertStatus struct {
	Exists        bool     `json:"exists"`
	Domain        string   `json:"domain,omitempty"`
	SANs          []string `json:"sans,omitempty"`
	Issuer        string   `json:"issuer,omitempty"`
	NotBefore     string   `json:"not_before,omitempty"`
	NotAfter      string   `json:"not_after,omitempty"`
	DaysRemaining int      `json:"days_remaining"`
	NeedsRenewal  bool     `json:"needs_renewal"`
	Error         string   `json:"error,omitempty"`
}

// ACMEDeps injects dependencies for testing ACME and Route 53 flows.
type ACMEDeps struct {
	DirectoryURL string
	HTTPClient   *http.Client
	AWSBin       string
	RunCmd       ddns.RunCmdFn
	Dig          ddns.DigFn
	Now          func() time.Time
	PollInterval time.Duration
	Logf         func(string, ...any)
}

func (d ACMEDeps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now().UTC()
}

func (d ACMEDeps) directoryURL(staging bool) string {
	if d.DirectoryURL != "" {
		return d.DirectoryURL
	}
	if staging {
		return LetsEncryptStagingURL
	}
	return LetsEncryptProdURL
}

func (d ACMEDeps) logf(format string, args ...any) {
	if d.Logf != nil {
		d.Logf(format, args...)
	}
}

// InspectCert reads and validates fullchain.pem + privkey.pem against expectedDomain.
func InspectCert(tlsDir, expectedDomain string, now time.Time) CertStatus {
	st := CertStatus{NeedsRenewal: true}
	p := Paths{
		Dir:       tlsDir,
		Fullchain: tlsDir + "/fullchain.pem",
		Privkey:   tlsDir + "/privkey.pem",
	}
	certPEM, err := os.ReadFile(p.Fullchain)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			st.Error = err.Error()
		}
		return st
	}
	keyPEM, err := os.ReadFile(p.Privkey)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			st.Error = err.Error()
		}
		return st
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		st.Error = fmt.Sprintf("invalid certificate keypair: %v", err)
		return st
	}
	if len(pair.Certificate) == 0 {
		st.Error = "certificate chain is empty"
		return st
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		st.Error = fmt.Sprintf("parse leaf certificate: %v", err)
		return st
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	rem := leaf.NotAfter.Sub(now)
	days := int(rem / (24 * time.Hour))
	if rem < 0 {
		days = 0
	}
	st.Exists = true
	st.SANs = slices.Clone(leaf.DNSNames)
	st.Issuer = leaf.Issuer.CommonName
	if st.Issuer == "" && len(leaf.Issuer.Organization) > 0 {
		st.Issuer = leaf.Issuer.Organization[0]
	}
	st.NotBefore = leaf.NotBefore.UTC().Format(time.RFC3339)
	st.NotAfter = leaf.NotAfter.UTC().Format(time.RFC3339)
	st.DaysRemaining = days
	if len(leaf.DNSNames) > 0 {
		st.Domain = leaf.DNSNames[0]
	}
	if expectedDomain != "" {
		if err := leaf.VerifyHostname(expectedDomain); err != nil {
			st.Error = fmt.Sprintf("certificate does not match %s: %v", expectedDomain, err)
			return st
		}
		st.Domain = expectedDomain
	}
	st.NeedsRenewal = rem < RenewBefore || now.Before(leaf.NotBefore)
	return st
}

func loadOrCreateAccountKey(path string) (*ecdsa.PrivateKey, error) {
	if raw, err := os.ReadFile(path); err == nil {
		block, _ := pem.Decode(raw)
		if block == nil {
			return nil, fmt.Errorf("invalid PEM in account key %s", path)
		}
		key, err := x509.ParseECPrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse account key %s: %w", path, err)
		}
		return key, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
	if err := writeAtomicFile(path, pemBytes, 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

func encodeCertChainPEM(certs [][]byte) []byte {
	var buf bytes.Buffer
	for _, der := range certs {
		_ = pem.Encode(&buf, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	}
	return buf.Bytes()
}

func encodeECKeyPEM(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), nil
}

func registerACMEAccount(ctx context.Context, cl *acme.Client, email string) error {
	acc := &acme.Account{}
	if email = strings.TrimSpace(email); email != "" {
		acc.Contact = []string{"mailto:" + email}
	}
	_, err := cl.Register(ctx, acc, acme.AcceptTOS)
	if errors.Is(err, acme.ErrAccountAlreadyExists) {
		return nil
	}
	return err
}

func solveDNS01Authz(
	ctx context.Context,
	cl *acme.Client,
	cfg Config,
	authzURL string,
	deps ACMEDeps,
) error {
	authz, err := cl.GetAuthorization(ctx, authzURL)
	if err != nil {
		return fmt.Errorf("get authorization: %w", err)
	}
	if authz.Status == acme.StatusValid {
		return nil
	}
	var chal *acme.Challenge
	for _, c := range authz.Challenges {
		if c.Type == "dns-01" {
			chal = c
			break
		}
	}
	if chal == nil {
		return errors.New("ACME server offered no dns-01 challenge")
	}
	val, err := cl.DNS01ChallengeRecord(chal.Token)
	if err != nil {
		return fmt.Errorf("compute dns-01 record: %w", err)
	}
	recordFQDN := "_acme-challenge." + cfg.Domain
	changeID, err := ChangeTXT(cfg, recordFQDN, "UPSERT", val, deps.AWSBin, deps.RunCmd)
	if err != nil {
		return err
	}
	defer func() {
		if _, dErr := ChangeTXT(cfg, recordFQDN, "DELETE", val, deps.AWSBin, deps.RunCmd); dErr != nil {
			deps.logf("tls: warning cleaning up %s TXT: %v", recordFQDN, dErr)
		}
	}()
	if err := WaitChangeInSync(ctx, cfg.Profile, changeID, deps.AWSBin, deps.RunCmd, deps.PollInterval); err != nil {
		return err
	}
	if err := WaitTXTPropagated(ctx, recordFQDN, val, cfg.Nameservers, deps.Dig, deps.PollInterval); err != nil {
		return err
	}
	if _, err := cl.Accept(ctx, chal); err != nil {
		return fmt.Errorf("accept dns-01 challenge: %w", err)
	}
	if _, err := cl.WaitAuthorization(ctx, authz.URI); err != nil {
		return fmt.Errorf("wait authorization: %w", err)
	}
	return nil
}

// EnsureCert issues or renews the TLS certificate via ACME DNS-01 if missing, invalid, expiring, or forced.
func EnsureCert(ctx context.Context, tlsDir string, cfg Config, force bool, deps ACMEDeps) (bool, CertStatus, error) {
	domain, err := NormalizeDomain(cfg.Domain)
	if err != nil {
		return false, CertStatus{}, err
	}
	cfg.Domain = domain

	cur := InspectCert(tlsDir, domain, deps.now())
	if !force && cur.Exists && !cur.NeedsRenewal {
		return false, cur, nil
	}
	if err := os.MkdirAll(tlsDir, 0o700); err != nil {
		return false, cur, err
	}
	accountKeyPath := (Paths{
		Dir:               tlsDir,
		ProdAccountKey:    tlsDir + "/account.key",
		StagingAccountKey: tlsDir + "/account-staging.key",
	}).AccountKey(cfg.Staging)
	acctKey, err := loadOrCreateAccountKey(accountKeyPath)
	if err != nil {
		return false, cur, err
	}
	cl := &acme.Client{
		Key:          acctKey,
		HTTPClient:   deps.HTTPClient,
		DirectoryURL: deps.directoryURL(cfg.Staging),
		UserAgent:    "fleet-acme/1.0",
	}
	if err := registerACMEAccount(ctx, cl, cfg.Email); err != nil {
		return false, cur, fmt.Errorf("register ACME account: %w", err)
	}
	order, err := cl.AuthorizeOrder(ctx, acme.DomainIDs(domain))
	if err != nil {
		return false, cur, fmt.Errorf("create ACME order: %w", err)
	}
	for _, authzURL := range order.AuthzURLs {
		if err := solveDNS01Authz(ctx, cl, cfg, authzURL, deps); err != nil {
			return false, cur, err
		}
	}
	order, err = cl.WaitOrder(ctx, order.URI)
	if err != nil {
		return false, cur, fmt.Errorf("wait ACME order: %w", err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return false, cur, err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		DNSNames: []string{domain},
	}, leafKey)
	if err != nil {
		return false, cur, err
	}
	derChain, _, err := cl.CreateOrderCert(ctx, order.FinalizeURL, csr, true)
	if err != nil {
		return false, cur, fmt.Errorf("finalize ACME certificate: %w", err)
	}
	certPEM := encodeCertChainPEM(derChain)
	keyPEM, err := encodeECKeyPEM(leafKey)
	if err != nil {
		return false, cur, err
	}
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		return false, cur, fmt.Errorf("verify issued certificate: %w", err)
	}
	if err := writeAtomicFile(tlsDir+"/privkey.pem", keyPEM, 0o600); err != nil {
		return false, cur, err
	}
	if err := writeAtomicFile(tlsDir+"/fullchain.pem", certPEM, 0o644); err != nil {
		return false, cur, err
	}
	return true, InspectCert(tlsDir, domain, deps.now()), nil
}

// ManagerState reports background ACME renewal state.
type ManagerState struct {
	Renewing  bool   `json:"renewing"`
	LastCheck string `json:"last_check,omitempty"`
	LastRenew string `json:"last_renew,omitempty"`
	LastError string `json:"last_error,omitempty"`
}

// RenewResult reports the outcome of a manual or scheduled TLS renewal attempt.
type RenewResult struct {
	Renewed  bool       `json:"renewed"`
	PublicA  string     `json:"public_a,omitempty"`
	Cert     CertStatus `json:"cert"`
	Checked  string     `json:"checked_at"`
	ErrorMsg string     `json:"error,omitempty"`
}

// Manager runs periodic background TLS certificate renewals on the lighthouse.
type Manager struct {
	stateDir string
	loadCfg  func() (*Config, error)
	deps     ACMEDeps

	opMu    sync.Mutex
	stateMu sync.Mutex
	state   ManagerState
	cancel  context.CancelFunc
	done    chan struct{}
}

// NewManager creates a Manager that renews certificates under stateDir/tls.
func NewManager(stateDir string, loadCfg func() (*Config, error), deps ACMEDeps) *Manager {
	return &Manager{
		stateDir: stateDir,
		loadCfg:  loadCfg,
		deps:     deps,
	}
}

// Start launches the background renewal loop.
func (m *Manager) Start(ctx context.Context) {
	if m == nil {
		return
	}
	m.stateMu.Lock()
	if m.cancel != nil {
		m.stateMu.Unlock()
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	m.cancel = cancel
	m.done = done
	m.stateMu.Unlock()

	go m.loop(runCtx, done)
}

// Stop stops the background renewal loop and waits for exit.
func (m *Manager) Stop() {
	if m == nil {
		return
	}
	m.stateMu.Lock()
	cancel := m.cancel
	done := m.done
	m.cancel = nil
	m.done = nil
	m.stateMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

func (m *Manager) loop(ctx context.Context, done chan struct{}) {
	defer close(done)
	select {
	case <-ctx.Done():
		return
	case <-time.After(startupCheckDelay):
	}
	_, _ = m.RenewNow(ctx, false)

	ticker := time.NewTicker(renewCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, _ = m.RenewNow(ctx, false)
		}
	}
}

// State returns a snapshot of the background renewal state.
func (m *Manager) State() ManagerState {
	if m == nil {
		return ManagerState{}
	}
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	return m.state
}

// RenewNow checks and renews the certificate (and ensures PublicA if configured).
func (m *Manager) RenewNow(ctx context.Context, force bool) (RenewResult, error) {
	m.opMu.Lock()
	defer m.opMu.Unlock()

	cfg, err := m.loadCfg()
	if err != nil {
		return RenewResult{}, err
	}
	if cfg == nil || cfg.Domain == "" {
		return RenewResult{}, errors.New("TLS is not configured (run `fleet tls setup`)")
	}

	nowStr := m.deps.now().UTC().Format(time.RFC3339)
	m.stateMu.Lock()
	m.state.Renewing = true
	m.state.LastCheck = nowStr
	m.stateMu.Unlock()
	defer func() {
		m.stateMu.Lock()
		m.state.Renewing = false
		m.stateMu.Unlock()
	}()

	var publicAStr string
	if cfg.PublicA {
		rec, rErr := CheckStaticA(*cfg, m.deps.Dig)
		if rErr != nil || rec == nil || *rec != paths.LighthouseIP {
			if _, uErr := UpsertStaticA(*cfg, paths.LighthouseIP, m.deps.AWSBin, m.deps.RunCmd); uErr != nil {
				m.deps.logf("tls: warning ensuring Route 53 A record for %s: %v", cfg.Domain, uErr)
			} else {
				publicAStr = paths.LighthouseIP
			}
		} else {
			publicAStr = *rec
		}
	}

	renewed, status, err := EnsureCert(ctx, paths.TLSDir(m.stateDir), *cfg, force, m.deps)
	m.stateMu.Lock()
	if err != nil {
		m.state.LastError = err.Error()
		m.stateMu.Unlock()
		return RenewResult{Cert: status, Checked: nowStr, ErrorMsg: err.Error()}, err
	}
	m.state.LastError = ""
	if renewed {
		m.state.LastRenew = m.deps.now().UTC().Format(time.RFC3339)
	}
	m.stateMu.Unlock()
	return RenewResult{
		Renewed: renewed,
		PublicA: publicAStr,
		Cert:    status,
		Checked: nowStr,
	}, nil
}
