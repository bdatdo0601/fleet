package tlsdns

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/bdatdo0601/fleet/internal/paths"
)

const (
	dnsRecordTTL     = 60
	dnsSOATTL        = 60
	maxUDPPacketSize = 1232
	maxTCPMsgSize    = 65535
	maxDNSTCPConns   = 64
)

var meshPrefix = netip.MustParsePrefix(paths.MeshCIDR)

// DNSSnapshot describes the authoritative private zone and active A-record mappings.
type DNSSnapshot struct {
	Domain       string            `json:"domain"`
	Enabled      bool              `json:"enabled"`
	LighthouseIP string            `json:"lighthouse_ip"`
	Records      map[string]string `json:"records"`
}

// RecordProvider returns the latest zone snapshot for incoming DNS queries.
type RecordProvider func(ctx context.Context) (DNSSnapshot, error)

// IsAllowedDNSPeer allows only loopback and Nebula mesh (10.213.0.0/16) peers.
func IsAllowedDNSPeer(addr net.Addr) bool {
	if addr == nil {
		return false
	}
	var ip netip.Addr
	switch a := addr.(type) {
	case *net.UDPAddr:
		if a == nil {
			return false
		}
		ip, _ = netip.AddrFromSlice(a.IP)
	case *net.TCPAddr:
		if a == nil {
			return false
		}
		ip, _ = netip.AddrFromSlice(a.IP)
	default:
		host, _, err := net.SplitHostPort(addr.String())
		if err != nil {
			return false
		}
		ip, err = netip.ParseAddr(host)
		if err != nil {
			return false
		}
	}
	ip = ip.Unmap()
	return ip.IsValid() && (ip.IsLoopback() || meshPrefix.Contains(ip))
}

func toFQDN(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	n = strings.TrimSuffix(n, ".")
	if n == "" {
		return "."
	}
	return n + "."
}

func inZone(qname, zoneFQDN string) bool {
	if qname == zoneFQDN {
		return true
	}
	return strings.HasSuffix(qname, "."+zoneFQDN)
}

func soaResource(zoneFQDN string) (dnsmessage.Resource, error) {
	zoneName, err := dnsmessage.NewName(zoneFQDN)
	if err != nil {
		return dnsmessage.Resource{}, err
	}
	nsName, err := dnsmessage.NewName("ns1." + zoneFQDN)
	if err != nil {
		return dnsmessage.Resource{}, err
	}
	mboxName, err := dnsmessage.NewName("hostmaster." + zoneFQDN)
	if err != nil {
		return dnsmessage.Resource{}, err
	}
	return dnsmessage.Resource{
		Header: dnsmessage.ResourceHeader{
			Name:  zoneName,
			Type:  dnsmessage.TypeSOA,
			Class: dnsmessage.ClassINET,
			TTL:   dnsSOATTL,
		},
		Body: &dnsmessage.SOAResource{
			NS:      nsName,
			MBox:    mboxName,
			Serial:  1,
			Refresh: 300,
			Retry:   60,
			Expire:  86400,
			MinTTL:  dnsSOATTL,
		},
	}, nil
}

func aResource(fqdn string, addr netip.Addr) (dnsmessage.Resource, error) {
	name, err := dnsmessage.NewName(fqdn)
	if err != nil {
		return dnsmessage.Resource{}, err
	}
	return dnsmessage.Resource{
		Header: dnsmessage.ResourceHeader{
			Name:  name,
			Type:  dnsmessage.TypeA,
			Class: dnsmessage.ClassINET,
			TTL:   dnsRecordTTL,
		},
		Body: &dnsmessage.AResource{A: addr.As4()},
	}, nil
}

func resolveZoneIP(qname, zoneFQDN string, snap DNSSnapshot) (netip.Addr, bool) {
	bare := strings.TrimSuffix(qname, ".")
	if raw, ok := snap.Records[bare]; ok {
		if addr, err := netip.ParseAddr(raw); err == nil && addr.Is4() {
			return addr, true
		}
	}
	if qname == zoneFQDN || qname == "ns1."+zoneFQDN {
		lh := snap.LighthouseIP
		if lh == "" {
			lh = paths.LighthouseIP
		}
		if addr, err := netip.ParseAddr(lh); err == nil && addr.Is4() {
			return addr, true
		}
	}
	return netip.Addr{}, false
}

// HandleQuery processes a raw DNS wire query against snap and returns the wire response.
func HandleQuery(req []byte, snap DNSSnapshot) ([]byte, error) {
	var p dnsmessage.Parser
	hdr, err := p.Start(req)
	if err != nil {
		return nil, err
	}
	if hdr.Response {
		return nil, errors.New("ignoring DNS response packet")
	}
	q, err := p.Question()
	if err != nil {
		return nil, err
	}
	_ = p.SkipAllQuestions()

	respHdr := dnsmessage.Header{
		ID:                 hdr.ID,
		Response:           true,
		Authoritative:      true,
		RecursionDesired:   hdr.RecursionDesired,
		RecursionAvailable: false,
		RCode:              dnsmessage.RCodeSuccess,
	}

	if !snap.Enabled || snap.Domain == "" {
		respHdr.RCode = dnsmessage.RCodeRefused
		return packDNSReply(respHdr, q, nil, nil, nil)
	}
	zoneFQDN := toFQDN(snap.Domain)
	qname := strings.ToLower(q.Name.String())
	if !inZone(qname, zoneFQDN) {
		respHdr.RCode = dnsmessage.RCodeRefused
		return packDNSReply(respHdr, q, nil, nil, nil)
	}
	if q.Class != dnsmessage.ClassINET && q.Class != dnsmessage.ClassANY {
		respHdr.RCode = dnsmessage.RCodeNotImplemented
		return packDNSReply(respHdr, q, nil, nil, nil)
	}

	soa, err := soaResource(zoneFQDN)
	if err != nil {
		return nil, err
	}
	ip, exists := resolveZoneIP(qname, zoneFQDN, snap)
	if !exists {
		respHdr.RCode = dnsmessage.RCodeNameError
		return packDNSReply(respHdr, q, nil, []dnsmessage.Resource{soa}, nil)
	}

	var answers, authority, additional []dnsmessage.Resource
	switch q.Type {
	case dnsmessage.TypeA, dnsmessage.TypeALL:
		if r, rErr := aResource(qname, ip); rErr == nil {
			answers = append(answers, r)
		}
	case dnsmessage.TypeSOA:
		if qname == zoneFQDN {
			answers = append(answers, soa)
		} else {
			authority = append(authority, soa)
		}
	case dnsmessage.TypeNS:
		if qname == zoneFQDN {
			nsName, _ := dnsmessage.NewName("ns1." + zoneFQDN)
			answers = append(answers, dnsmessage.Resource{
				Header: dnsmessage.ResourceHeader{
					Name:  q.Name,
					Type:  dnsmessage.TypeNS,
					Class: dnsmessage.ClassINET,
					TTL:   dnsRecordTTL,
				},
				Body: &dnsmessage.NSResource{NS: nsName},
			})
			if glueIP, ok := resolveZoneIP("ns1."+zoneFQDN, zoneFQDN, snap); ok {
				if glue, gErr := aResource("ns1."+zoneFQDN, glueIP); gErr == nil {
					additional = append(additional, glue)
				}
			}
		} else {
			authority = append(authority, soa)
		}
	default:
		// NODATA (NOERROR + SOA authority) for AAAA and other record types on an existing name.
		authority = append(authority, soa)
	}
	return packDNSReply(respHdr, q, answers, authority, additional)
}

func packDNSReply(
	hdr dnsmessage.Header,
	q dnsmessage.Question,
	answers, authority, additional []dnsmessage.Resource,
) ([]byte, error) {
	b := dnsmessage.NewBuilder(make([]byte, 0, 512), hdr)
	if err := b.StartQuestions(); err != nil {
		return nil, err
	}
	if err := b.Question(q); err != nil {
		return nil, err
	}
	if len(answers) > 0 {
		if err := b.StartAnswers(); err != nil {
			return nil, err
		}
		for _, r := range answers {
			if err := addResource(&b, r); err != nil {
				return nil, err
			}
		}
	}
	if len(authority) > 0 {
		if err := b.StartAuthorities(); err != nil {
			return nil, err
		}
		for _, r := range authority {
			if err := addResource(&b, r); err != nil {
				return nil, err
			}
		}
	}
	if len(additional) > 0 {
		if err := b.StartAdditionals(); err != nil {
			return nil, err
		}
		for _, r := range additional {
			if err := addResource(&b, r); err != nil {
				return nil, err
			}
		}
	}
	return b.Finish()
}

func addResource(b *dnsmessage.Builder, r dnsmessage.Resource) error {
	switch body := r.Body.(type) {
	case *dnsmessage.AResource:
		return b.AResource(r.Header, *body)
	case *dnsmessage.NSResource:
		return b.NSResource(r.Header, *body)
	case *dnsmessage.SOAResource:
		return b.SOAResource(r.Header, *body)
	default:
		return fmt.Errorf("unsupported DNS resource type %T", r.Body)
	}
}

func bindDNSAddr(host string, port int) (net.PacketConn, net.Listener, error) {
	addr := fmt.Sprintf("%s:%d", host, port)
	pc, err := net.ListenPacket("udp4", addr)
	if err != nil {
		return nil, nil, err
	}
	ln, err := net.Listen("tcp4", addr)
	if err != nil {
		_ = pc.Close()
		return nil, nil, err
	}
	return pc, ln, nil
}

func serveDNSPacketConn(ctx context.Context, pc net.PacketConn, provider RecordProvider) {
	buf := make([]byte, maxUDPPacketSize)
	for {
		n, peer, err := pc.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		if !IsAllowedDNSPeer(peer) {
			continue
		}
		snap, err := provider(ctx)
		if err != nil {
			continue
		}
		reply, err := HandleQuery(buf[:n], snap)
		if err != nil || len(reply) == 0 {
			continue
		}
		_, _ = pc.WriteTo(reply, peer)
	}
}

func handleDNSTCPConn(ctx context.Context, conn net.Conn, provider RecordProvider) {
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer func() {
		stop()
		_ = conn.Close()
	}()
	if !IsAllowedDNSPeer(conn.RemoteAddr()) {
		return
	}
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	var lenBuf [2]byte
	if _, err := io.ReadFull(conn, lenBuf[:]); err != nil {
		return
	}
	msgLen := int(binary.BigEndian.Uint16(lenBuf[:]))
	if msgLen <= 0 || msgLen > maxTCPMsgSize {
		return
	}
	req := make([]byte, msgLen)
	if _, err := io.ReadFull(conn, req); err != nil {
		return
	}
	snap, err := provider(ctx)
	if err != nil {
		return
	}
	reply, err := HandleQuery(req, snap)
	if err != nil || len(reply) == 0 || len(reply) > maxTCPMsgSize {
		return
	}
	var outLen [2]byte
	binary.BigEndian.PutUint16(outLen[:], uint16(len(reply)))
	_, _ = conn.Write(outLen[:])
	_, _ = conn.Write(reply)
}

func serveDNSListeners(
	ctx context.Context,
	pc net.PacketConn,
	ln net.Listener,
	provider RecordProvider,
	wg *sync.WaitGroup,
) {
	go func() {
		defer wg.Done()
		serveDNSPacketConn(ctx, pc, provider)
	}()
	go func() {
		var connWg sync.WaitGroup
		sem := make(chan struct{}, maxDNSTCPConns)
		defer func() {
			connWg.Wait()
			wg.Done()
		}()
		for {
			conn, err := ln.Accept()
			if err != nil {
				if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
					return
				}
				continue
			}
			select {
			case sem <- struct{}{}:
				connWg.Go(func() {
					defer func() { <-sem }()
					handleDNSTCPConn(ctx, conn, provider)
				})
			default:
				_ = conn.Close()
			}
		}
	}()
}

// ServeDNS runs UDP and TCP authoritative DNS listeners on loopback and the Nebula lighthouse IP
// (never 0.0.0.0, so unrelated virtual bridges such as Apple container vmnet keep their own :53).
func ServeDNS(ctx context.Context, bindPort int, provider RecordProvider, logf func(string, ...any)) error {
	var (
		mu      sync.Mutex
		closed  bool
		closers []io.Closer
		wg      sync.WaitGroup
	)
	registerAndServe := func(pc net.PacketConn, ln net.Listener) bool {
		mu.Lock()
		if closed {
			mu.Unlock()
			_ = pc.Close()
			_ = ln.Close()
			return false
		}
		closers = append(closers, pc, ln)
		wg.Add(2)
		mu.Unlock()
		if logf != nil {
			logf("fleet dns listening on %s (udp+tcp)", ln.Addr().String())
		}
		serveDNSListeners(ctx, pc, ln, provider, &wg)
		return true
	}

	pc, ln, err := bindDNSAddr("127.0.0.1", bindPort)
	if err != nil {
		return fmt.Errorf("bind DNS on 127.0.0.1:%d: %w", bindPort, err)
	}
	registerAndServe(pc, ln)

	if meshPC, meshLN, meshErr := bindDNSAddr(paths.LighthouseIP, bindPort); meshErr == nil {
		registerAndServe(meshPC, meshLN)
	} else {
		if logf != nil {
			logf("fleet dns waiting for %s:%d (%v)", paths.LighthouseIP, bindPort, meshErr)
		}
		wg.Go(func() {
			ticker := time.NewTicker(2 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					mpc, mln, err := bindDNSAddr(paths.LighthouseIP, bindPort)
					if err == nil {
						registerAndServe(mpc, mln)
						return
					}
				}
			}
		})
	}

	<-ctx.Done()
	mu.Lock()
	closed = true
	toClose := append([]io.Closer(nil), closers...)
	mu.Unlock()
	for _, c := range toClose {
		_ = c.Close()
	}
	wg.Wait()
	return nil
}
