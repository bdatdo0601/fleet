package ws

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func readServerFrame(t *testing.T, r io.Reader) (fin bool, opcode byte, masked bool, payload []byte) {
	t.Helper()
	var b2 [2]byte
	if _, err := io.ReadFull(r, b2[:]); err != nil {
		t.Fatalf("read frame header: %v", err)
	}
	fin = (b2[0] & 0x80) != 0
	opcode = b2[0] & 0x0f
	masked = (b2[1] & 0x80) != 0
	length := uint64(b2[1] & 0x7f)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			t.Fatalf("read 16-bit len: %v", err)
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			t.Fatalf("read 64-bit len: %v", err)
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(r, mask[:]); err != nil {
			t.Fatalf("read mask: %v", err)
		}
	}
	payload = make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		t.Fatalf("read payload: %v", err)
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return fin, opcode, masked, payload
}

func writeServerFrame(t *testing.T, w io.Writer, fin bool, opcode byte, payload []byte) {
	t.Helper()
	var hdr [10]byte
	if fin {
		hdr[0] = 0x80 | (opcode & 0x0f)
	} else {
		hdr[0] = opcode & 0x0f
	}
	n := len(payload)
	hdrLen := 2
	switch {
	case n < 126:
		hdr[1] = byte(n)
	case n <= 65535:
		hdr[1] = 126
		binary.BigEndian.PutUint16(hdr[2:4], uint16(n))
		hdrLen = 4
	default:
		hdr[1] = 127
		binary.BigEndian.PutUint64(hdr[2:10], uint64(n))
		hdrLen = 10
	}
	if _, err := w.Write(hdr[:hdrLen]); err != nil {
		t.Fatalf("write frame hdr: %v", err)
	}
	if n > 0 {
		if _, err := w.Write(payload); err != nil {
			t.Fatalf("write frame payload: %v", err)
		}
	}
}

func acceptWSUpgrade(t *testing.T, ln net.Listener) (net.Conn, *bufio.Reader, string) {
	t.Helper()
	conn, err := ln.Accept()
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	br := bufio.NewReader(conn)
	req, err := http.ReadRequest(br)
	if err != nil {
		_ = conn.Close()
		t.Fatalf("read upgrade req: %v", err)
	}
	key := req.Header.Get("Sec-WebSocket-Key")
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + AcceptHash(key) + "\r\n\r\n"
	if _, err := io.WriteString(conn, resp); err != nil {
		_ = conn.Close()
		t.Fatalf("write 101: %v", err)
	}
	return conn, br, req.Host
}

func TestFramingMaskingPingFragmentationAndClose(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	srvDone := make(chan struct{})
	go func() {
		defer close(srvDone)
		conn, br, _ := acceptWSUpgrade(t, ln)
		defer func() { _ = conn.Close() }()

		// 1. Client small frame must be masked.
		fin, op, masked, p := readServerFrame(t, br)
		if !fin || op != 0x1 || !masked || string(p) != "hello" {
			t.Errorf("unexpected small frame: fin=%v op=%x masked=%v payload=%q", fin, op, masked, p)
		}

		// 2. Client medium (>126B) frame must be masked and use 126-length encoding.
		fin, op, masked, p = readServerFrame(t, br)
		if !fin || op != 0x1 || !masked || string(p) != strings.Repeat("a", 300) {
			t.Errorf("unexpected medium frame: fin=%v op=%x masked=%v len=%d", fin, op, masked, len(p))
		}

		// 3. Server sends ping -> client must reply pong; then fragmented text -> client reassembles.
		writeServerFrame(t, conn, true, 0x9, []byte("ping-1"))
		fin, op, masked, p = readServerFrame(t, br)
		if !fin || op != 0xA || !masked || string(p) != "ping-1" {
			t.Errorf("expected masked pong, got fin=%v op=%x masked=%v payload=%q", fin, op, masked, p)
		}

		writeServerFrame(t, conn, false, 0x1, []byte("part1-"))
		writeServerFrame(t, conn, true, 0x0, []byte("part2"))

		// 4. Server sends close frame -> client echoes close and returns io.EOF.
		writeServerFrame(t, conn, true, 0x8, nil)
		_, op, masked, _ = readServerFrame(t, br)
		if op != 0x8 || !masked {
			t.Errorf("expected masked close echo, got op=%x masked=%v", op, masked)
		}
	}()

	c, err := DialWS(t.Context(), "ws://"+ln.Addr().String()+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	if err := c.WriteText([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := c.WriteText([]byte(strings.Repeat("a", 300))); err != nil {
		t.Fatal(err)
	}
	got, err := c.ReadText(t.Context())
	if err != nil || string(got) != "part1-part2" {
		t.Fatalf("ReadText fragmented got (%q, %v)", got, err)
	}
	if _, err := c.ReadText(t.Context()); !errors.Is(err, io.EOF) {
		t.Fatalf("expected io.EOF on close frame, got %v", err)
	}
	<-srvDone
}

func TestReadTextCancelMidRead(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, _, _ := acceptWSUpgrade(t, ln)
		accepted <- conn
	}()

	c, err := DialWS(t.Context(), "ws://"+ln.Addr().String()+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	srvConn := <-accepted
	defer func() { _ = srvConn.Close() }()

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err = c.ReadText(ctx)
	elapsed := time.Since(start)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if elapsed > time.Second {
		t.Fatalf("ReadText cancellation took too long: %v", elapsed)
	}
}

func TestIPv6URLHostPortAndDial(t *testing.T) {
	for _, tc := range []struct {
		url      string
		wantHost string
	}{
		{"ws://[::1]/chat", "[::1]:80"},
		{"wss://[::1]/chat", "[::1]:443"},
		{"ws://[::1]:8443/chat", "[::1]:8443"},
		{"ws://example.com/ws", "example.com:80"},
		{"wss://example.com/ws", "example.com:443"},
	} {
		_, host, _, err := wsDialTarget(tc.url)
		if err != nil || host != tc.wantHost {
			t.Fatalf("wsDialTarget(%q) = (%q, %v), want %q", tc.url, host, err, tc.wantHost)
		}
	}

	ln, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback unavailable: %v", err)
	}
	defer func() { _ = ln.Close() }()

	gotHost := make(chan string, 1)
	go func() {
		conn, _, host := acceptWSUpgrade(t, ln)
		gotHost <- host
		_ = conn.Close()
	}()

	addr := ln.Addr().String()
	c, err := DialWS(t.Context(), "ws://"+addr+"/v1/ws", nil)
	if err != nil {
		t.Fatalf("DialWS IPv6 failed: %v", err)
	}
	_ = c.Close()
	if h := <-gotHost; h != addr {
		t.Fatalf("expected Host %q, got %q", addr, h)
	}
}

func TestDialWSHandshakeTimeout(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	hold := make(chan struct{})
	defer close(hold)
	go func() {
		conn, aErr := ln.Accept()
		if aErr == nil {
			defer func() { _ = conn.Close() }()
			<-hold
		}
	}()

	ctx, cancel := context.WithTimeout(t.Context(), 80*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err = DialWS(ctx, "ws://"+ln.Addr().String()+"/ws", nil)
	if err == nil {
		t.Fatal("expected handshake timeout error")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("expected deadline/timeout error, got %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("DialWS did not honor handshake deadline promptly")
	}
}
