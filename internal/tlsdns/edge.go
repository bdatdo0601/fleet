package tlsdns

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/bdatdo0601/fleet/internal/paths"
)

const (
	certCheckInterval = 2 * time.Second
	dnsCacheTTL       = 5 * time.Second
)

type fileStamp struct {
	modTime time.Time
	size    int64
}

// CertLoader loads and hot-reloads fullchain.pem + privkey.pem whenever files change.
type CertLoader struct {
	fullchainPath string
	privkeyPath   string

	mu          sync.Mutex
	lastChecked time.Time
	certStamp   fileStamp
	keyStamp    fileStamp
	cached      *tls.Certificate
}

// NewCertLoader creates a CertLoader watching stateDir/tls/{fullchain.pem,privkey.pem}.
func NewCertLoader(stateDir string) *CertLoader {
	p := CertPaths(stateDir)
	return &CertLoader{
		fullchainPath: p.Fullchain,
		privkeyPath:   p.Privkey,
	}
}

func statStamp(path string) (fileStamp, error) {
	info, err := os.Stat(path)
	if err != nil {
		return fileStamp{}, err
	}
	return fileStamp{modTime: info.ModTime(), size: info.Size()}, nil
}

// GetCertificate satisfies tls.Config.GetCertificate with hot-reload on file change.
func (c *CertLoader) GetCertificate(_ *tls.ClientHelloInfo) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	if c.cached != nil && now.Sub(c.lastChecked) < certCheckInterval {
		return c.cached, nil
	}
	c.lastChecked = now

	cs, err1 := statStamp(c.fullchainPath)
	ks, err2 := statStamp(c.privkeyPath)
	if err1 != nil || err2 != nil {
		if c.cached != nil {
			return c.cached, nil
		}
		return nil, fmt.Errorf("TLS certificate unavailable: %w", errors.Join(err1, err2))
	}
	if c.cached != nil && cs == c.certStamp && ks == c.keyStamp {
		return c.cached, nil
	}
	pair, err := tls.LoadX509KeyPair(c.fullchainPath, c.privkeyPath)
	if err != nil {
		if c.cached != nil {
			return c.cached, nil
		}
		return nil, fmt.Errorf("load TLS keypair: %w", err)
	}
	c.certStamp = cs
	c.keyStamp = ks
	c.cached = &pair
	return c.cached, nil
}

func clientIPFromAddr(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err == nil && host != "" {
		return strings.Trim(host, "[]")
	}
	return strings.Trim(remoteAddr, "[]")
}

// NewHTTPSProxy returns a reverse proxy to upstreamURL that authenticates via stateDir/edge.token
// and forwards the verified mesh peer IP in X-Fleet-Client-IP.
func NewHTTPSProxy(upstreamURL, stateDir string) (http.Handler, error) {
	target, err := url.Parse(upstreamURL)
	if err != nil {
		return nil, fmt.Errorf("invalid upstream URL %q: %w", upstreamURL, err)
	}

	var tokenMu sync.Mutex
	var cachedTok string
	var tokenChecked time.Time
	loadToken := func() string {
		tokenMu.Lock()
		defer tokenMu.Unlock()
		if cachedTok != "" && time.Since(tokenChecked) < certCheckInterval {
			return cachedTok
		}
		tokenChecked = time.Now()
		if tok, rErr := ReadEdgeToken(stateDir); rErr == nil {
			cachedTok = tok
		}
		return cachedTok
	}

	proxy := &httputil.ReverseProxy{
		FlushInterval: -1, // Preserve SSE streaming (/v1/assistant/stream).
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			// Preserve browser Host header so server.checkBrowserOrigin matches Origin vs Host.
			pr.Out.Host = pr.In.Host
			pr.Out.Header.Del(HeaderClientIP)
			pr.Out.Header.Del(HeaderEdgeToken)
			if peerIP := clientIPFromAddr(pr.In.RemoteAddr); peerIP != "" {
				pr.Out.Header.Set(HeaderClientIP, peerIP)
			}
			if tok := loadToken(); tok != "" {
				pr.Out.Header.Set(HeaderEdgeToken, tok)
			}
			pr.Out.Header.Set("X-Forwarded-Proto", "https")
		},
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peerIP := clientIPFromAddr(r.RemoteAddr)
		if addr, pErr := netip.ParseAddr(peerIP); pErr != nil || (!addr.IsLoopback() && !meshPrefix.Contains(addr)) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		proxy.ServeHTTP(w, r)
	}), nil
}

// NewCachedRecordProvider polls upstreamURL/v1/dns/records with TTL caching and stale fallback.
func NewCachedRecordProvider(upstreamURL, stateDir string, httpClient *http.Client) RecordProvider {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 3 * time.Second}
	}
	endpoint := strings.TrimRight(upstreamURL, "/") + "/v1/dns/records"

	var mu sync.Mutex
	var cached DNSSnapshot
	var fetchedAt time.Time
	var hasCached bool

	return func(ctx context.Context) (DNSSnapshot, error) {
		mu.Lock()
		if hasCached && time.Since(fetchedAt) < dnsCacheTTL {
			snap := cached
			mu.Unlock()
			return snap, nil
		}
		mu.Unlock()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return DNSSnapshot{}, err
		}
		if tok, tErr := ReadEdgeToken(stateDir); tErr == nil && tok != "" {
			req.Header.Set(HeaderEdgeToken, tok)
			req.Header.Set(HeaderClientIP, "127.0.0.1")
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			mu.Lock()
			defer mu.Unlock()
			if hasCached {
				return cached, nil
			}
			return DNSSnapshot{}, err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			mu.Lock()
			defer mu.Unlock()
			if hasCached {
				return cached, nil
			}
			return DNSSnapshot{}, fmt.Errorf("upstream DNS records HTTP %d", resp.StatusCode)
		}
		var snap DNSSnapshot
		if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
			mu.Lock()
			defer mu.Unlock()
			if hasCached {
				return cached, nil
			}
			return DNSSnapshot{}, err
		}

		mu.Lock()
		cached = snap
		fetchedAt = time.Now()
		hasCached = true
		mu.Unlock()
		return snap, nil
	}
}

// EdgeOpts configures the root-owned `fleet edge` HTTPS + DNS daemon.
type EdgeOpts struct {
	StateDir    string
	UpstreamURL string
	HTTPSAddr   string
	DNSPort     int
	Logf        func(string, ...any)
}

// RunEdge runs the HTTPS reverse proxy and private mesh DNS server until ctx is canceled.
func RunEdge(ctx context.Context, opts EdgeOpts) error {
	if opts.UpstreamURL == "" {
		opts.UpstreamURL = fmt.Sprintf("http://127.0.0.1:%d", paths.APIPort())
	}
	if opts.HTTPSAddr == "" {
		opts.HTTPSAddr = fmt.Sprintf("0.0.0.0:%d", paths.HTTPSPort())
	}
	if opts.DNSPort <= 0 {
		opts.DNSPort = paths.DNSPort()
	}
	logf := opts.Logf
	if logf == nil {
		logf = func(format string, args ...any) {
			_, _ = fmt.Fprintf(os.Stderr, format+"\n", args...)
		}
	}

	proxyHandler, err := NewHTTPSProxy(opts.UpstreamURL, opts.StateDir)
	if err != nil {
		return err
	}
	loader := NewCertLoader(opts.StateDir)
	ln, err := net.Listen("tcp", opts.HTTPSAddr)
	if err != nil {
		return fmt.Errorf("listen HTTPS %s: %w", opts.HTTPSAddr, err)
	}
	srv := &http.Server{
		Addr:              opts.HTTPSAddr,
		Handler:           proxyHandler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    64 * 1024,
		TLSConfig: &tls.Config{
			MinVersion:     tls.VersionTLS12,
			GetCertificate: loader.GetCertificate,
		},
	}
	logf("fleet edge HTTPS listening on %s -> %s", opts.HTTPSAddr, opts.UpstreamURL)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	errCh := make(chan error, 2)
	go func() {
		err := srv.ServeTLS(ln, "", "")
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("HTTPS server: %w", err)
			return
		}
		errCh <- nil
	}()
	go func() {
		provider := NewCachedRecordProvider(opts.UpstreamURL, opts.StateDir, nil)
		errCh <- ServeDNS(runCtx, opts.DNSPort, provider, logf)
	}()

	var firstErr error
	select {
	case <-ctx.Done():
	case err := <-errCh:
		if err != nil {
			firstErr = err
		}
		cancel()
	}
	shutCtx, shutCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer shutCancel()
	_ = srv.Shutdown(shutCtx)
	if second := <-errCh; second != nil && firstErr == nil && ctx.Err() == nil {
		firstErr = second
	}
	return firstErr
}
