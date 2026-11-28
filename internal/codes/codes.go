// Package codes encodes and decodes copy-paste blocks carrying join requests/responses.
//
// Blocks are PEM-style and wrapped at 64 columns because macOS terminals truncate a
// single pasted line at 1024 bytes, and a response is ~2 KB.
package codes

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
)

const (
	Request  = "FLEET JOIN REQUEST"
	Response = "FLEET JOIN RESPONSE"

	maxDecodedBytes = 1 << 20
	maxBlockBytes   = 1 << 20
)

// Encode serializes payload as compact JSON, zlib-compresses at level 9, base64-encodes,
// wraps at 64 columns, and surrounds with PEM markers.
func Encode(label string, payload any) (string, error) {
	var jsonBuf bytes.Buffer
	enc := json.NewEncoder(&jsonBuf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(payload); err != nil {
		return "", fmt.Errorf("encode JSON: %w", err)
	}
	jsonBytes := bytes.TrimSuffix(jsonBuf.Bytes(), []byte("\n"))

	var compBuf bytes.Buffer
	zw, err := zlib.NewWriterLevel(&compBuf, zlib.BestCompression)
	if err != nil {
		return "", err
	}
	if _, err := zw.Write(jsonBytes); err != nil {
		_ = zw.Close()
		return "", err
	}
	if err := zw.Close(); err != nil {
		return "", err
	}

	b64 := base64.StdEncoding.EncodeToString(compBuf.Bytes())
	var out strings.Builder
	out.WriteString("-----BEGIN ")
	out.WriteString(label)
	out.WriteString("-----")
	for i := 0; i < len(b64); i += 64 {
		end := min(i+64, len(b64))
		out.WriteByte('\n')
		out.WriteString(b64[i:end])
	}
	out.WriteString("\n-----END ")
	out.WriteString(label)
	out.WriteString("-----")
	return out.String(), nil
}

// Decode extracts and decodes a PEM-wrapped join block into out.
func Decode(label, text string, out any) error {
	begin := "-----BEGIN " + label + "-----"
	end := "-----END " + label + "-----"
	_, after, ok := strings.Cut(text, begin)
	if !ok {
		return fmt.Errorf("expected a %s block", label)
	}
	body, _, ok := strings.Cut(after, end)
	if !ok {
		return fmt.Errorf("expected a %s block", label)
	}
	cleaned := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, body)

	raw, err := base64.StdEncoding.DecodeString(cleaned)
	if err != nil {
		return fmt.Errorf("%s block is damaged; copy it again (%w)", label, err)
	}
	zr, err := zlib.NewReader(bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("%s block is damaged; copy it again (%w)", label, err)
	}
	defer func() { _ = zr.Close() }()
	plain, err := io.ReadAll(io.LimitReader(zr, maxDecodedBytes+1))
	if err != nil {
		return fmt.Errorf("%s block is damaged; copy it again (%w)", label, err)
	}
	if len(plain) > maxDecodedBytes {
		return fmt.Errorf("%s block exceeds maximum decoded size (%d bytes)", label, maxDecodedBytes)
	}
	if err := json.Unmarshal(plain, out); err != nil {
		return fmt.Errorf("%s block is damaged; copy it again (%w)", label, err)
	}
	return nil
}

// ReadBlock reads lines from r up to and including the first line containing endMarker
// without over-reading past that newline if r is not already buffered.
func ReadBlock(endMarker string, r io.Reader) (string, error) {
	var nextByte func() (byte, error)
	if br, ok := r.(*bufio.Reader); ok {
		nextByte = br.ReadByte
	} else {
		var b [1]byte
		nextByte = func() (byte, error) {
			for {
				n, err := r.Read(b[:])
				if n > 0 {
					return b[0], nil
				}
				if err != nil {
					return 0, err
				}
			}
		}
	}
	var out, lineBuf strings.Builder
	for {
		ch, err := nextByte()
		if err == nil {
			if out.Len() >= maxBlockBytes {
				return out.String(), fmt.Errorf("input block exceeds maximum size (%d bytes)", maxBlockBytes)
			}
			out.WriteByte(ch)
			lineBuf.WriteByte(ch)
			if ch == '\n' {
				if strings.Contains(lineBuf.String(), endMarker) {
					return out.String(), nil
				}
				lineBuf.Reset()
			}
			continue
		}
		if errors.Is(err, io.EOF) {
			return out.String(), nil
		}
		return out.String(), err
	}
}

// ShortID returns a human-comparable id (xxxx-xxxx-xxxx) from the SHA-256 of text.
func ShortID(text string) string {
	sum := sha256.Sum256([]byte(text))
	h := hex.EncodeToString(sum[:])[:12]
	return h[0:4] + "-" + h[4:8] + "-" + h[8:12]
}
