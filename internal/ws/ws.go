// Package ws provides a minimal RFC 6455 WebSocket client built on Go stdlib networking.
package ws

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	wsMagicGUID     = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	wsMaxFrameBytes = 4 * 1024 * 1024
)

// WSConn is a minimal RFC 6455 WebSocket client built on Go stdlib networking.
type WSConn struct {
	conn    net.Conn
	br      *bufio.Reader
	writeMu sync.Mutex
	closeMu sync.Mutex
	closed  bool
}

// DialWS connects to a ws:// or wss:// endpoint using ctx for dial timeout/cancellation.
func DialWS(ctx context.Context, rawURL string, tlsCfg *tls.Config) (*WSConn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	host := u.Host
	switch u.Scheme {
	case "ws":
		if !strings.Contains(host, ":") {
			host += ":80"
		}
	case "wss":
		if !strings.Contains(host, ":") {
			host += ":443"
		}
	default:
		return nil, fmt.Errorf("unsupported websocket scheme %q", u.Scheme)
	}

	dialer := &net.Dialer{Timeout: 15 * time.Second}
	var conn net.Conn
	if u.Scheme == "wss" {
		cfg := &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12}
		if tlsCfg != nil {
			cfg = tlsCfg.Clone()
			if cfg.ServerName == "" {
				cfg.ServerName = u.Hostname()
			}
		}
		conn, err = (&tls.Dialer{NetDialer: dialer, Config: cfg}).DialContext(ctx, "tcp", host)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", host)
	}
	if err != nil {
		return nil, err
	}

	var keyRaw [16]byte
	if _, err := rand.Read(keyRaw[:]); err != nil {
		_ = conn.Close()
		return nil, err
	}
	wsKey := base64.StdEncoding.EncodeToString(keyRaw[:])

	path := u.RequestURI()
	if path == "" {
		path = "/"
	}
	reqStr := fmt.Sprintf(
		"GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n",
		path, u.Host, wsKey,
	)
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	if _, err := io.WriteString(conn, reqStr); err != nil {
		_ = conn.Close()
		return nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		_ = conn.Close()
		return nil, fmt.Errorf("websocket handshake failed: HTTP %d", resp.StatusCode)
	}
	expected := AcceptHash(wsKey)
	if strings.TrimSpace(resp.Header.Get("Sec-WebSocket-Accept")) != expected {
		_ = conn.Close()
		return nil, errors.New("websocket handshake failed: invalid Sec-WebSocket-Accept")
	}
	_ = conn.SetDeadline(time.Time{})
	return &WSConn{conn: conn, br: br}, nil
}

// AcceptHash computes the RFC 6455 Sec-WebSocket-Accept value for a client key.
func AcceptHash(key string) string {
	h := sha1.Sum([]byte(key + wsMagicGUID))
	return base64.StdEncoding.EncodeToString(h[:])
}

// WriteText sends a masked text frame to the server.
func (c *WSConn) WriteText(data []byte) error {
	return c.writeFrame(0x1, data)
}

func (c *WSConn) writeFrame(opcode byte, payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	var hdr [14]byte
	hdr[0] = 0x80 | (opcode & 0x0f)
	n := len(payload)
	hdrLen := 2
	switch {
	case n < 126:
		hdr[1] = 0x80 | byte(n)
	case n <= 65535:
		hdr[1] = 0x80 | 126
		binary.BigEndian.PutUint16(hdr[2:4], uint16(n))
		hdrLen = 4
	default:
		hdr[1] = 0x80 | 127
		binary.BigEndian.PutUint64(hdr[2:10], uint64(n))
		hdrLen = 10
	}
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return err
	}
	copy(hdr[hdrLen:hdrLen+4], mask[:])
	hdrLen += 4

	masked := make([]byte, n)
	for i := range n {
		masked[i] = payload[i] ^ mask[i%4]
	}
	_ = c.conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
	if _, err := c.conn.Write(hdr[:hdrLen]); err != nil {
		return err
	}
	if n > 0 {
		if _, err := c.conn.Write(masked); err != nil {
			return err
		}
	}
	return nil
}

// ReadText reads the next complete text message, answering pings transparently.
func (c *WSConn) ReadText(ctx context.Context) ([]byte, error) {
	var msgBuf []byte
	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if dl, ok := ctx.Deadline(); ok {
			_ = c.conn.SetReadDeadline(dl)
		} else {
			_ = c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		}
		fin, opcode, payload, err := c.readFrame()
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() && ctx.Err() == nil {
				continue
			}
			return nil, err
		}
		switch opcode {
		case 0x8: // close
			_ = c.writeFrame(0x8, payload)
			return nil, io.EOF
		case 0x9: // ping
			_ = c.writeFrame(0xA, payload)
			continue
		case 0xA: // pong
			continue
		case 0x1, 0x2, 0x0:
			msgBuf = append(msgBuf, payload...)
			if len(msgBuf) > wsMaxFrameBytes {
				return nil, errors.New("websocket message too large")
			}
			if fin {
				return msgBuf, nil
			}
		}
	}
}

func (c *WSConn) readFrame() (bool, byte, []byte, error) {
	var b2 [2]byte
	if _, err := io.ReadFull(c.br, b2[:]); err != nil {
		return false, 0, nil, err
	}
	fin := (b2[0] & 0x80) != 0
	opcode := b2[0] & 0x0f
	masked := (b2[1] & 0x80) != 0
	length := uint64(b2[1] & 0x7f)

	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return false, 0, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return false, 0, nil, err
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	if length > wsMaxFrameBytes {
		return false, 0, nil, fmt.Errorf("websocket frame too large: %d", length)
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(c.br, mask[:]); err != nil {
			return false, 0, nil, err
		}
	}
	payload := make([]byte, length)
	if length > 0 {
		if _, err := io.ReadFull(c.br, payload); err != nil {
			return false, 0, nil, err
		}
		if masked {
			for i := range payload {
				payload[i] ^= mask[i%4]
			}
		}
	}
	return fin, opcode, payload, nil
}

// Close closes the underlying WebSocket TCP/TLS connection.
func (c *WSConn) Close() error {
	c.closeMu.Lock()
	defer c.closeMu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	return c.conn.Close()
}
