package assistant

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bdatdo0601/fleet/internal/paths"
)

var (
	cgnatPrefix = netip.MustParsePrefix("100.64.0.0/10")
	meshPrefix  = netip.MustParsePrefix(paths.MeshCIDR)
)

// IsBlockedEgressAddr reports whether addr is loopback, private, link-local, CGNAT, Nebula mesh, or otherwise non-public.
func IsBlockedEgressAddr(addr netip.Addr) bool {
	if !addr.IsValid() {
		return true
	}
	addr = addr.Unmap()
	if addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() ||
		addr.IsLinkLocalMulticast() || addr.IsMulticast() || addr.IsUnspecified() ||
		addr.IsInterfaceLocalMulticast() {
		return true
	}
	return cgnatPrefix.Contains(addr) || meshPrefix.Contains(addr)
}

// HostMatchesAllowlist reports whether host matches the optional egress allowlist (empty allowlist permits any public host).
func HostMatchesAllowlist(host string, allowlist []string) bool {
	cleanHost := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if cleanHost == "" {
		return false
	}
	if len(allowlist) == 0 {
		return true
	}
	for _, raw := range allowlist {
		pat := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(raw), "."))
		if pat == "" {
			continue
		}
		if pat == "*" || pat == cleanHost {
			return true
		}
		if suffix, ok := strings.CutPrefix(pat, "*."); ok && suffix != "" {
			if cleanHost == suffix || strings.HasSuffix(cleanHost, "."+suffix) {
				return true
			}
		}
	}
	return false
}

// EgressProxy is an ephemeral authenticating HTTP/CONNECT proxy blocking private/Nebula IPs and non-80/443 ports.
type EgressProxy struct {
	listener     net.Listener
	srv          *http.Server
	transport    *http.Transport
	token        string
	allowedHosts []string
	lookupIP     func(context.Context, string) ([]netip.Addr, error)
	dialTCP      func(context.Context, string, string) (net.Conn, error)

	mu        sync.Mutex
	closed    bool
	contacted map[string]struct{}
	conns     map[net.Conn]struct{}
	tunnelWG  sync.WaitGroup
}

// StartEgressProxy binds an authenticating HTTP/CONNECT proxy on listenAddr.
func StartEgressProxy(listenAddr string, allowedHosts []string, lookupFn func(context.Context, string) ([]netip.Addr, error)) (*EgressProxy, error) {
	var rawTok [16]byte
	if _, err := rand.Read(rawTok[:]); err != nil {
		return nil, err
	}
	if lookupFn == nil {
		lookupFn = func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		}
	}
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return nil, err
	}
	p := &EgressProxy{
		listener:     ln,
		token:        hex.EncodeToString(rawTok[:]),
		allowedHosts: append([]string(nil), allowedHosts...),
		lookupIP:     lookupFn,
		contacted:    make(map[string]struct{}),
		conns:        make(map[net.Conn]struct{}),
	}
	p.transport = &http.Transport{
		DialContext:           p.dialVerified,
		ForceAttemptHTTP2:     false,
		ResponseHeaderTimeout: 30 * time.Second,
		IdleConnTimeout:       15 * time.Second,
	}
	p.srv = &http.Server{
		Handler:           p,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		_ = p.srv.Serve(ln)
	}()
	return p, nil
}

// ProxyURL returns the authenticated http://fleet:<token>@host:port URL for sandbox env injection.
func (p *EgressProxy) ProxyURL() string {
	return fmt.Sprintf("http://fleet:%s@%s", p.token, p.listener.Addr().String())
}

// ContactedHosts returns the sorted list of remote hostnames dialed through this proxy.
func (p *EgressProxy) ContactedHosts() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.contacted))
	for h := range p.contacted {
		out = append(out, h)
	}
	slices.Sort(out)
	return out
}

// Close shuts down the ephemeral egress proxy and terminates active hijacked tunnels.
func (p *EgressProxy) Close() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	p.closed = true
	active := make([]net.Conn, 0, len(p.conns))
	for c := range p.conns {
		active = append(active, c)
	}
	clear(p.conns)
	p.mu.Unlock()

	for _, c := range active {
		_ = c.Close()
	}
	p.transport.CloseIdleConnections()
	err := p.srv.Close()
	p.tunnelWG.Wait()
	return err
}

func (p *EgressProxy) trackTunnelConns(a, b net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	p.conns[a] = struct{}{}
	p.conns[b] = struct{}{}
	return true
}

func (p *EgressProxy) untrackTunnelConns(a, b net.Conn) {
	p.mu.Lock()
	delete(p.conns, a)
	delete(p.conns, b)
	p.mu.Unlock()
}

func (p *EgressProxy) authorized(r *http.Request) bool {
	hdr := strings.TrimSpace(r.Header.Get("Proxy-Authorization"))
	encoded, ok := strings.CutPrefix(hdr, "Basic ")
	if !ok {
		return false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return false
	}
	expected := "fleet:" + p.token
	return len(raw) == len(expected) && subtle.ConstantTimeCompare(raw, []byte(expected)) == 1
}

// ServeHTTP handles Proxy-Authorization checks and dispatches CONNECT tunnels or plain HTTP requests.
func (p *EgressProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !p.authorized(r) {
		w.Header().Set("Proxy-Authenticate", `Basic realm="fleet-sandbox"`)
		http.Error(w, "proxy authentication required", http.StatusProxyAuthRequired)
		return
	}
	if r.Method == http.MethodConnect {
		p.handleConnect(w, r)
		return
	}
	p.handleHTTP(w, r)
}

func (p *EgressProxy) dialVerified(ctx context.Context, _, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("invalid target address %q", addr)
	}
	if port != "80" && port != "443" {
		return nil, fmt.Errorf("egress port %s blocked (only 80 and 443 allowed)", port)
	}
	cleanHost := strings.Trim(strings.TrimSpace(host), "[]")
	if !HostMatchesAllowlist(cleanHost, p.allowedHosts) {
		return nil, fmt.Errorf("host %q is not in approved allowed_hosts", cleanHost)
	}
	var ips []netip.Addr
	if literal, parseErr := netip.ParseAddr(cleanHost); parseErr == nil {
		ips = []netip.Addr{literal}
	} else {
		ips, err = p.lookupIP(ctx, cleanHost)
		if err != nil {
			return nil, fmt.Errorf("dns lookup failed for %q: %w", cleanHost, err)
		}
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("no IP addresses resolved for %q", cleanHost)
	}
	for _, ip := range ips {
		if IsBlockedEgressAddr(ip) {
			return nil, fmt.Errorf("egress to %q (%s) blocked: private/internal/mesh IP", cleanHost, ip)
		}
	}
	p.mu.Lock()
	p.contacted[strings.ToLower(cleanHost)] = struct{}{}
	p.mu.Unlock()
	if p.dialTCP != nil {
		return p.dialTCP(ctx, "tcp", net.JoinHostPort(ips[0].Unmap().String(), port))
	}
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	return dialer.DialContext(ctx, "tcp", net.JoinHostPort(ips[0].Unmap().String(), port))
}

func (p *EgressProxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	target := r.Host
	if target == "" {
		target = r.URL.Host
	}
	upstream, err := p.dialVerified(r.Context(), "tcp", target)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		_ = upstream.Close()
		http.Error(w, "hijacking unsupported", http.StatusInternalServerError)
		return
	}
	clientConn, _, err := hj.Hijack()
	if err != nil {
		_ = upstream.Close()
		return
	}
	if _, err := io.WriteString(clientConn, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		_ = clientConn.Close()
		_ = upstream.Close()
		return
	}
	if !p.trackTunnelConns(clientConn, upstream) {
		_ = clientConn.Close()
		_ = upstream.Close()
		return
	}
	var copyWG sync.WaitGroup
	copyWG.Add(2)
	p.tunnelWG.Add(2)
	go func() {
		defer p.tunnelWG.Done()
		defer copyWG.Done()
		defer func() { _ = upstream.Close() }()
		defer func() { _ = clientConn.Close() }()
		_, _ = io.Copy(upstream, clientConn)
	}()
	go func() {
		defer p.tunnelWG.Done()
		defer copyWG.Done()
		defer func() { _ = upstream.Close() }()
		defer func() { _ = clientConn.Close() }()
		_, _ = io.Copy(clientConn, upstream)
	}()
	p.tunnelWG.Go(func() {
		copyWG.Wait()
		p.untrackTunnelConns(clientConn, upstream)
	})
}

func (p *EgressProxy) handleHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL == nil || (r.URL.Scheme != "http" && r.URL.Scheme != "https") || r.URL.Host == "" {
		http.Error(w, "invalid proxy URL", http.StatusBadRequest)
		return
	}
	outReq := r.Clone(r.Context())
	outReq.RequestURI = ""
	stripHopHeaders(outReq.Header)
	outReq.Header.Del("Proxy-Authorization")

	resp, err := p.transport.RoundTrip(outReq)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, context.Canceled) {
			status = http.StatusRequestTimeout
		} else if strings.Contains(err.Error(), "blocked") || strings.Contains(err.Error(), "allowed_hosts") {
			status = http.StatusForbidden
		}
		http.Error(w, err.Error(), status)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	dstHdr := w.Header()
	for k, vals := range resp.Header {
		for _, v := range vals {
			dstHdr.Add(k, v)
		}
	}
	stripHopHeaders(dstHdr)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func stripHopHeaders(h http.Header) {
	for _, k := range []string{
		"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
		"Proxy-Connection", "Te", "Trailers", "Transfer-Encoding", "Upgrade",
	} {
		h.Del(k)
	}
}
