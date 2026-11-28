package tlsdns

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

func TestNormalizeDomain(t *testing.T) {
	got, err := NormalizeDomain("  Fleet.Example.COM. ")
	if err != nil || got != "fleet.example.com" {
		t.Fatalf("got (%q, %v)", got, err)
	}
	for _, bad := range []string{"", "localhost", "-bad.example.com", "bad-.example.com", "a/b.com", "*.example.com", "bad label.com"} {
		if _, err := NormalizeDomain(bad); err == nil {
			t.Fatalf("expected error for %q", bad)
		}
	}
}

func TestPolicyAndRoute53Helpers(t *testing.T) {
	pol, err := Policy(PolicyOpts{
		ZoneID:         "/hostedzone/Z0123ABC",
		Domain:         "fleet.example.com",
		DDNSName:       "lighthouse.example.com",
		IncludePublicA: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(pol)
	s := string(raw)
	for _, want := range []string{
		"_acme-challenge.fleet.example.com",
		`"TXT"`,
		`"DELETE"`,
		"fleet.example.com",
		"lighthouse.example.com",
		"arn:aws:route53:::change/*",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("expected %s in policy %s", want, s)
		}
	}

	var cmds [][]string
	runFn := func(name string, args ...string) (string, string, error) {
		cmds = append(cmds, append([]string{name}, args...))
		if len(args) >= 2 && args[1] == "get-change" {
			return `{"ChangeInfo":{"Id":"/change/C1","Status":"INSYNC"}}`, "", nil
		}
		return `{"ChangeInfo":{"Id":"/change/C1","Status":"PENDING"}}`, "", nil
	}
	cfg := Config{Domain: "fleet.example.com", ZoneID: "Z0123ABC", Profile: "fleet-ddns", Nameservers: []string{"ns-1.awsdns.com"}}
	id, err := UpsertStaticA(cfg, "10.213.0.1", "/opt/homebrew/bin/aws", runFn)
	if err != nil || id != "/change/C1" {
		t.Fatalf("UpsertStaticA got (%q, %v)", id, err)
	}
	if !strings.Contains(strings.Join(cmds[0], " "), "10.213.0.1") {
		t.Fatalf("expected 10.213.0.1 in batch: %v", cmds[0])
	}

	if err := WaitChangeInSync(context.Background(), cfg.Profile, "/change/C1", "/opt/homebrew/bin/aws", runFn, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	digCalls := 0
	err = WaitTXTPropagated(context.Background(), "_acme-challenge.fleet.example.com", "tok-val", cfg.Nameservers, func(...string) ([]string, error) {
		digCalls++
		if digCalls == 1 {
			return []string{`"stale"`}, nil
		}
		return []string{`"tok-val"`}, nil
	}, time.Millisecond)
	if err != nil || digCalls < 2 {
		t.Fatalf("WaitTXTPropagated got err=%v calls=%d", err, digCalls)
	}
}

func generateTestCAAndLeaf(t *testing.T, domain string, notAfter time.Time) (caDER, certDER, keyPEM []byte) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Test ACME CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err = x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: domain},
		DNSNames:     []string{domain},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	certDER, err = x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return caDER, certDER, keyPEM
}

func startFakeACMEServer(t *testing.T, domain string) *httptest.Server {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(10),
		Subject:               pkix.Name{CommonName: "Fake Let's Encrypt"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, _ := x509.ParseCertificate(caDER)

	var mu sync.Mutex
	accepted := false
	var issuedPEM []byte
	var srv *httptest.Server

	decodeJWSPayload := func(r *http.Request) ([]byte, error) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		var env struct {
			Payload string `json:"payload"`
		}
		if err := json.Unmarshal(raw, &env); err != nil {
			return nil, err
		}
		if env.Payload == "" {
			return nil, nil
		}
		return base64.RawURLEncoding.DecodeString(env.Payload)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/directory", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"newNonce":   srv.URL + "/new-nonce",
			"newAccount": srv.URL + "/new-account",
			"newOrder":   srv.URL + "/new-order",
		})
	})
	mux.HandleFunc("/new-nonce", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Replay-Nonce", "nonce-1")
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/new-account", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Replay-Nonce", "nonce-2")
		w.Header().Set("Location", srv.URL+"/acct/1")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"status":"valid"}`))
	})
	mux.HandleFunc("/new-order", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Replay-Nonce", "nonce-3")
		w.Header().Set("Location", srv.URL+"/order/1")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":         "pending",
			"identifiers":    []map[string]string{{"type": "dns", "value": domain}},
			"authorizations": []string{srv.URL + "/authz/1"},
			"finalize":       srv.URL + "/order/1/finalize",
		})
	})
	mux.HandleFunc("/authz/1", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		st := "pending"
		if accepted {
			st = "valid"
		}
		mu.Unlock()
		w.Header().Set("Replay-Nonce", "nonce-4")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":     st,
			"identifier": map[string]string{"type": "dns", "value": domain},
			"challenges": []map[string]any{
				{"type": "dns-01", "url": srv.URL + "/chal/1", "status": st, "token": "dns-token-123"},
			},
		})
	})
	mux.HandleFunc("/chal/1", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		accepted = true
		mu.Unlock()
		w.Header().Set("Replay-Nonce", "nonce-5")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"dns-01","status":"valid","token":"dns-token-123"}`))
	})
	mux.HandleFunc("/order/1", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		st := "ready"
		var certURL string
		if len(issuedPEM) > 0 {
			st = "valid"
			certURL = srv.URL + "/cert/1"
		}
		mu.Unlock()
		w.Header().Set("Replay-Nonce", "nonce-6")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":         st,
			"identifiers":    []map[string]string{{"type": "dns", "value": domain}},
			"authorizations": []string{srv.URL + "/authz/1"},
			"finalize":       srv.URL + "/order/1/finalize",
			"certificate":    certURL,
		})
	})
	mux.HandleFunc("/order/1/finalize", func(w http.ResponseWriter, r *http.Request) {
		payload, err := decodeJWSPayload(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var req struct {
			CSR string `json:"csr"`
		}
		if err := json.Unmarshal(payload, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		csrDER, err := base64.RawURLEncoding.DecodeString(req.CSR)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		csr, err := x509.ParseCertificateRequest(csrDER)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		leafTmpl := &x509.Certificate{
			SerialNumber: big.NewInt(99),
			Subject:      pkix.Name{CommonName: domain},
			DNSNames:     csr.DNSNames,
			NotBefore:    time.Now().Add(-time.Hour),
			NotAfter:     time.Now().Add(90 * 24 * time.Hour),
			KeyUsage:     x509.KeyUsageDigitalSignature,
			ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}
		leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, csr.PublicKey, caKey)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		var pemBuf bytes.Buffer
		_ = pem.Encode(&pemBuf, &pem.Block{Type: "CERTIFICATE", Bytes: leafDER})
		_ = pem.Encode(&pemBuf, &pem.Block{Type: "CERTIFICATE", Bytes: caDER})
		mu.Lock()
		issuedPEM = pemBuf.Bytes()
		mu.Unlock()

		w.Header().Set("Replay-Nonce", "nonce-7")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":      "valid",
			"certificate": srv.URL + "/cert/1",
		})
	})
	mux.HandleFunc("/cert/1", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		out := issuedPEM
		mu.Unlock()
		w.Header().Set("Replay-Nonce", "nonce-8")
		w.Header().Set("Content-Type", "application/pem-certificate-chain")
		_, _ = w.Write(out)
	})

	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestEnsureCertACMEDNS01Lifecycle(t *testing.T) {
	domain := "fleet.example.com"
	srv := startFakeACMEServer(t, domain)
	stateDir := t.TempDir()
	tlsDir := filepath.Join(stateDir, "tls")

	var mu sync.Mutex
	var txtRecord string
	var actions []string

	deps := ACMEDeps{
		DirectoryURL: srv.URL + "/directory",
		HTTPClient:   srv.Client(),
		AWSBin:       "/opt/homebrew/bin/aws",
		PollInterval: time.Millisecond,
		RunCmd: func(_ string, args ...string) (string, string, error) {
			mu.Lock()
			defer mu.Unlock()
			if len(args) >= 2 && args[1] == "get-change" {
				return `{"ChangeInfo":{"Id":"/change/C1","Status":"INSYNC"}}`, "", nil
			}
			for i := 0; i < len(args)-1; i++ {
				if args[i] == "--change-batch" {
					var batch struct {
						Changes []struct {
							Action            string `json:"Action"`
							ResourceRecordSet struct {
								ResourceRecords []struct {
									Value string `json:"Value"`
								} `json:"ResourceRecords"`
							} `json:"ResourceRecordSet"`
						} `json:"Changes"`
					}
					if err := json.Unmarshal([]byte(args[i+1]), &batch); err == nil && len(batch.Changes) > 0 {
						act := batch.Changes[0].Action
						actions = append(actions, act)
						if act == "UPSERT" && len(batch.Changes[0].ResourceRecordSet.ResourceRecords) > 0 {
							txtRecord = batch.Changes[0].ResourceRecordSet.ResourceRecords[0].Value
						}
						if act == "DELETE" {
							txtRecord = ""
						}
					}
				}
			}
			return `{"ChangeInfo":{"Id":"/change/C1","Status":"PENDING"}}`, "", nil
		},
		Dig: func(...string) ([]string, error) {
			mu.Lock()
			defer mu.Unlock()
			if txtRecord == "" {
				return nil, nil
			}
			return []string{txtRecord}, nil
		},
	}

	cfg := Config{
		Domain:      domain,
		ZoneID:      "Z0123ABC",
		Profile:     "fleet-ddns",
		Email:       "ops@example.com",
		Nameservers: []string{"ns-1.awsdns.com"},
		PublicA:     true,
		PrivateDNS:  true,
	}

	renewed, status, err := EnsureCert(context.Background(), tlsDir, cfg, false, deps)
	if err != nil {
		t.Fatalf("EnsureCert failed: %v", err)
	}
	if !renewed || !status.Exists || status.NeedsRenewal || status.Domain != domain {
		t.Fatalf("unexpected issued status: renewed=%v status=%+v", renewed, status)
	}
	if len(actions) != 2 || actions[0] != "UPSERT" || actions[1] != "DELETE" {
		t.Fatalf("expected UPSERT then DELETE TXT cleanup, got %v", actions)
	}
	if txtRecord != "" {
		t.Fatalf("expected TXT record cleaned up, still %q", txtRecord)
	}

	// Second call skips renewal when cert is fresh.
	renewed2, _, err := EnsureCert(context.Background(), tlsDir, cfg, false, deps)
	if err != nil || renewed2 {
		t.Fatalf("expected second EnsureCert to skip, renewed=%v err=%v", renewed2, err)
	}

	// CertLoader hot-reloads updated fullchain + privkey.
	loader := NewCertLoader(stateDir)
	c1, err := loader.GetCertificate(nil)
	if err != nil || c1 == nil {
		t.Fatalf("GetCertificate initial failed: %v", err)
	}
	caDER, certDER, keyPEM := generateTestCAAndLeaf(t, domain, time.Now().Add(60*24*time.Hour))
	var chainBuf bytes.Buffer
	_ = pem.Encode(&chainBuf, &pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	_ = pem.Encode(&chainBuf, &pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	if err := os.WriteFile(filepath.Join(tlsDir, "fullchain.pem"), chainBuf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tlsDir, "privkey.pem"), keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(10 * time.Second)
	_ = os.Chtimes(filepath.Join(tlsDir, "fullchain.pem"), future, future)
	loader.mu.Lock()
	loader.lastChecked = time.Time{}
	loader.mu.Unlock()
	c2, err := loader.GetCertificate(nil)
	if err != nil || bytes.Equal(c1.Certificate[0], c2.Certificate[0]) {
		t.Fatalf("expected CertLoader to reload updated certificate, err=%v", err)
	}
}

func buildDNSQueryPacket(t *testing.T, qname string, qtype dnsmessage.Type) []byte {
	t.Helper()
	n, err := dnsmessage.NewName(qname)
	if err != nil {
		t.Fatal(err)
	}
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 0x1234, RecursionDesired: true})
	if err := b.StartQuestions(); err != nil {
		t.Fatal(err)
	}
	if err := b.Question(dnsmessage.Question{Name: n, Type: qtype, Class: dnsmessage.ClassINET}); err != nil {
		t.Fatal(err)
	}
	out, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestDNSServerQueriesAndTCPUDP(t *testing.T) {
	snap := DNSSnapshot{
		Domain:       "fleet.example.com",
		Enabled:      true,
		LighthouseIP: "10.213.0.1",
		Records: map[string]string{
			"fleet.example.com":        "10.213.0.1",
			"laptop.fleet.example.com": "10.213.0.2",
		},
	}

	// A query for device.
	rep, err := HandleQuery(buildDNSQueryPacket(t, "LAPTOP.fleet.example.com.", dnsmessage.TypeA), snap)
	if err != nil {
		t.Fatal(err)
	}
	var p dnsmessage.Parser
	hdr, err := p.Start(rep)
	if err != nil || hdr.RCode != dnsmessage.RCodeSuccess || !hdr.Authoritative {
		t.Fatalf("unexpected A reply header: %+v err=%v", hdr, err)
	}
	_ = p.SkipAllQuestions()
	ah, err := p.AnswerHeader()
	if err != nil || ah.Type != dnsmessage.TypeA {
		t.Fatalf("expected A answer, got %+v err=%v", ah, err)
	}
	aRes, err := p.AResource()
	if err != nil || net.IP(aRes.A[:]).String() != "10.213.0.2" {
		t.Fatalf("unexpected A IP: %v err=%v", aRes, err)
	}

	// AAAA on existing name returns NOERROR (NODATA) + SOA.
	repAAAA, err := HandleQuery(buildDNSQueryPacket(t, "fleet.example.com.", dnsmessage.TypeAAAA), snap)
	if err != nil {
		t.Fatal(err)
	}
	hdrAAAA, _ := p.Start(repAAAA)
	if hdrAAAA.RCode != dnsmessage.RCodeSuccess {
		t.Fatalf("expected NOERROR NODATA for AAAA, got %v", hdrAAAA.RCode)
	}

	// Unknown name in zone returns NXDOMAIN.
	repNX, err := HandleQuery(buildDNSQueryPacket(t, "ghost.fleet.example.com.", dnsmessage.TypeA), snap)
	if err != nil {
		t.Fatal(err)
	}
	hdrNX, _ := p.Start(repNX)
	if hdrNX.RCode != dnsmessage.RCodeNameError {
		t.Fatalf("expected NXDOMAIN, got %v", hdrNX.RCode)
	}

	// Out-of-zone returns REFUSED.
	repRef, err := HandleQuery(buildDNSQueryPacket(t, "example.org.", dnsmessage.TypeA), snap)
	if err != nil {
		t.Fatal(err)
	}
	hdrRef, _ := p.Start(repRef)
	if hdrRef.RCode != dnsmessage.RCodeRefused {
		t.Fatalf("expected REFUSED, got %v", hdrRef.RCode)
	}

	// Peer IP filter rejects public IP and accepts loopback + 10.213.x.
	if IsAllowedDNSPeer(&net.UDPAddr{IP: net.ParseIP("8.8.8.8"), Port: 5353}) {
		t.Fatal("expected public IP to be rejected")
	}
	if !IsAllowedDNSPeer(&net.UDPAddr{IP: net.ParseIP("10.213.0.4"), Port: 5353}) {
		t.Fatal("expected Nebula mesh IP to be allowed")
	}

	// Live UDP + TCP round-trip on loopback.
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pc.Close() }()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	ctx := t.Context()
	prov := func(context.Context) (DNSSnapshot, error) { return snap, nil }
	go serveDNSPacketConn(ctx, pc, prov)
	go func() {
		for {
			c, aErr := ln.Accept()
			if aErr != nil {
				return
			}
			go handleDNSTCPConn(ctx, c, prov)
		}
	}()

	q := buildDNSQueryPacket(t, "fleet.example.com.", dnsmessage.TypeA)
	uconn, err := net.Dial("udp4", pc.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = uconn.Close() }()
	if _, err := uconn.Write(q); err != nil {
		t.Fatal(err)
	}
	ubuf := make([]byte, 512)
	n, err := uconn.Read(ubuf)
	if err != nil {
		t.Fatal(err)
	}
	if hdrU, _ := p.Start(ubuf[:n]); hdrU.RCode != dnsmessage.RCodeSuccess {
		t.Fatalf("UDP query failed: %+v", hdrU)
	}

	tconn, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tconn.Close() }()
	var lenPrefix [2]byte
	binary.BigEndian.PutUint16(lenPrefix[:], uint16(len(q)))
	if _, err := tconn.Write(lenPrefix[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := tconn.Write(q); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(tconn, lenPrefix[:]); err != nil {
		t.Fatal(err)
	}
	tbuf := make([]byte, binary.BigEndian.Uint16(lenPrefix[:]))
	if _, err := io.ReadFull(tconn, tbuf); err != nil {
		t.Fatal(err)
	}
	if hdrT, _ := p.Start(tbuf); hdrT.RCode != dnsmessage.RCodeSuccess {
		t.Fatalf("TCP query failed: %+v", hdrT)
	}
}

func TestEdgeHTTPSProxyStripsSpoofedHeaders(t *testing.T) {
	stateDir := t.TempDir()
	tok, err := EnsureEdgeToken(stateDir)
	if err != nil {
		t.Fatal(err)
	}

	var gotHost, gotTok, gotClientIP, gotProto string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		gotTok = r.Header.Get(HeaderEdgeToken)
		gotClientIP = r.Header.Get(HeaderClientIP)
		gotProto = r.Header.Get("X-Forwarded-Proto")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	proxyHandler, err := NewHTTPSProxy(upstream.URL, stateDir)
	if err != nil {
		t.Fatal(err)
	}

	// Spoofed incoming headers must be replaced by verified RemoteAddr & edge.token.
	req := httptest.NewRequest(http.MethodGet, "https://fleet.example.com/v1/overview", nil)
	req.Host = "fleet.example.com"
	req.RemoteAddr = "10.213.0.4:54321"
	req.Header.Set(HeaderClientIP, "10.213.0.1")
	req.Header.Set(HeaderEdgeToken, "spoofed")
	rec := httptest.NewRecorder()
	proxyHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if gotHost != "fleet.example.com" || gotTok != tok || gotClientIP != "10.213.0.4" || gotProto != "https" {
		t.Fatalf("unexpected forwarded headers: host=%q tok=%q ip=%q proto=%q", gotHost, gotTok, gotClientIP, gotProto)
	}

	// Non-mesh remote address is rejected at the proxy.
	badReq := httptest.NewRequest(http.MethodGet, "https://fleet.example.com/v1/overview", nil)
	badReq.RemoteAddr = "203.0.113.10:1234"
	badRec := httptest.NewRecorder()
	proxyHandler.ServeHTTP(badRec, badReq)
	if badRec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for public client IP, got %d", badRec.Code)
	}
}

func TestServeDNSBindsLoopbackNotWildcard(t *testing.T) {
	pc, ln, err := bindDNSAddr("127.0.0.1", 0)
	if err != nil {
		t.Fatalf("bindDNSAddr(127.0.0.1, 0) failed: %v", err)
	}
	udpAddr := pc.LocalAddr().String()
	tcpAddr := ln.Addr().String()
	_ = pc.Close()
	_ = ln.Close()

	if !strings.HasPrefix(udpAddr, "127.0.0.1:") || !strings.HasPrefix(tcpAddr, "127.0.0.1:") {
		t.Fatalf("expected loopback bind addresses, got udp=%q tcp=%q", udpAddr, tcpAddr)
	}

	// Find a free port on 127.0.0.1 where both UDP and TCP are available.
	probeLN, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probeLN.Addr().(*net.TCPAddr).Port
	_ = probeLN.Close()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	snap := DNSSnapshot{
		Domain:       "fleet.example.com",
		Enabled:      true,
		LighthouseIP: "10.213.0.1",
		Records:      map[string]string{"fleet.example.com": "10.213.0.1"},
	}
	prov := func(context.Context) (DNSSnapshot, error) { return snap, nil }

	var (
		logMu sync.Mutex
		logs  []string
	)
	done := make(chan error, 1)
	go func() {
		done <- ServeDNS(ctx, port, prov, func(format string, _ ...any) {
			logMu.Lock()
			defer logMu.Unlock()
			logs = append(logs, format)
		})
	}()

	// Wait briefly for 127.0.0.1:port to answer UDP queries.
	addr := probeLN.Addr().(*net.TCPAddr)
	target := net.JoinHostPort("127.0.0.1", strings.Split(addr.String(), ":")[1])
	q := buildDNSQueryPacket(t, "fleet.example.com.", dnsmessage.TypeA)
	var gotIP string
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		uconn, dErr := net.DialTimeout("udp4", target, 200*time.Millisecond)
		if dErr == nil {
			_ = uconn.SetDeadline(time.Now().Add(200 * time.Millisecond))
			_, _ = uconn.Write(q)
			buf := make([]byte, 512)
			n, rErr := uconn.Read(buf)
			_ = uconn.Close()
			if rErr == nil {
				var p dnsmessage.Parser
				if hdr, pErr := p.Start(buf[:n]); pErr == nil && hdr.RCode == dnsmessage.RCodeSuccess {
					_ = p.SkipAllQuestions()
					if ah, aErr := p.AnswerHeader(); aErr == nil && ah.Type == dnsmessage.TypeA {
						if res, resErr := p.AResource(); resErr == nil {
							gotIP = net.IP(res.A[:]).String()
							break
						}
					}
				}
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	if gotIP != "10.213.0.1" {
		t.Fatalf("expected 10.213.0.1 from ServeDNS loopback listener on %s, got %q", target, gotIP)
	}

	cancel()
	select {
	case sErr := <-done:
		if sErr != nil {
			t.Fatalf("ServeDNS returned error on cancel: %v", sErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ServeDNS did not shut down promptly after cancel")
	}
}

func TestEdgeHTTPSProxyFailClosedAndUnmap(t *testing.T) {
	// Missing edge.token -> 503 Service Unavailable, upstream never hit.
	emptyDir := t.TempDir()
	upstreamHits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits++
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	proxyNoTok, err := NewHTTPSProxy(upstream.URL, emptyDir)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "https://fleet.example.com/v1/overview", nil)
	req.RemoteAddr = "10.213.0.4:54321"
	rec := httptest.NewRecorder()
	proxyNoTok.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable || upstreamHits != 0 {
		t.Fatalf("expected 503 without upstream hit when edge.token missing, got code=%d hits=%d", rec.Code, upstreamHits)
	}

	// Valid edge.token + IPv4-mapped IPv6 mesh RemoteAddr -> unmapped and forwarded; mapped public IP -> 403.
	tokDir := t.TempDir()
	tok, err := EnsureEdgeToken(tokDir)
	if err != nil {
		t.Fatal(err)
	}
	var gotClientIP, gotTok string
	upstream2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotClientIP = r.Header.Get(HeaderClientIP)
		gotTok = r.Header.Get(HeaderEdgeToken)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream2.Close()

	proxyWithTok, err := NewHTTPSProxy(upstream2.URL, tokDir)
	if err != nil {
		t.Fatal(err)
	}
	req6 := httptest.NewRequest(http.MethodGet, "https://fleet.example.com/v1/overview", nil)
	req6.RemoteAddr = "[::ffff:10.213.0.4]:54321"
	rec6 := httptest.NewRecorder()
	proxyWithTok.ServeHTTP(rec6, req6)
	if rec6.Code != http.StatusOK || gotClientIP != "10.213.0.4" || gotTok != tok {
		t.Fatalf("expected 200 with unmapped 10.213.0.4, got code=%d ip=%q tok=%q", rec6.Code, gotClientIP, gotTok)
	}

	badReq6 := httptest.NewRequest(http.MethodGet, "https://fleet.example.com/v1/overview", nil)
	badReq6.RemoteAddr = "[::ffff:203.0.113.10]:1234"
	badRec6 := httptest.NewRecorder()
	proxyWithTok.ServeHTTP(badRec6, badReq6)
	if badRec6.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for mapped public IP, got %d", badRec6.Code)
	}
}

func TestWaitChangeInSyncFailFastAndRenewNowTryLockAndProviderBackoff(t *testing.T) {
	// WaitChangeInSync fails fast after 5 consecutive errors.
	calls := 0
	err := WaitChangeInSync(
		t.Context(),
		"prof",
		"/change/C1",
		"/usr/bin/aws",
		func(string, ...string) (string, string, error) {
			calls++
			return "", "AccessDenied", errors.New("exit 255")
		},
		time.Millisecond,
	)
	if err == nil || !strings.Contains(err.Error(), "AccessDenied") || calls != maxRoute53PollErrors {
		t.Fatalf("expected fail-fast after %d calls with AccessDenied, got calls=%d err=%v", maxRoute53PollErrors, calls, err)
	}

	// RenewNow uses TryLock and returns ErrRenewalInProgress if already running.
	started := make(chan struct{})
	release := make(chan struct{})
	stateDir := t.TempDir()
	mgr := NewManager(stateDir, func() (*Config, error) {
		close(started)
		<-release
		return nil, nil
	}, ACMEDeps{})
	done1 := make(chan error, 1)
	go func() {
		_, rErr := mgr.RenewNow(t.Context(), false)
		done1 <- rErr
	}()
	<-started
	_, err2 := mgr.RenewNow(t.Context(), false)
	if !errors.Is(err2, ErrRenewalInProgress) {
		t.Fatalf("expected ErrRenewalInProgress, got %v", err2)
	}
	close(release)
	<-done1

	// NewCachedRecordProvider applies failure backoff instead of hammering upstream.
	hits := 0
	failSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer failSrv.Close()
	prov := NewCachedRecordProvider(failSrv.URL, t.TempDir(), failSrv.Client())
	if _, err := prov(t.Context()); err == nil {
		t.Fatal("expected initial error")
	}
	if _, err := prov(t.Context()); err == nil {
		t.Fatal("expected cached backoff error")
	}
	if hits != 1 {
		t.Fatalf("expected 1 upstream hit during failure backoff, got %d", hits)
	}

	// CertPathsForDir constructs paths directly inside tlsDir.
	cp := CertPathsForDir("/tmp/custom-tls")
	if cp.Fullchain != "/tmp/custom-tls/fullchain.pem" || cp.Privkey != "/tmp/custom-tls/privkey.pem" {
		t.Fatalf("unexpected CertPathsForDir: %+v", cp)
	}
}
