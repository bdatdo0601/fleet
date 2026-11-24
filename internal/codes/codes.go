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
)

// CodeError indicates an invalid or damaged copy-paste join block.
type CodeError struct {
	Msg string
}

func (e *CodeError) Error() string { return e.Msg }

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
		end := i + 64
		if end > len(b64) {
			end = len(b64)
		}
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
	bi := strings.Index(text, begin)
	if bi < 0 {
		return &CodeError{Msg: fmt.Sprintf("expected a %s block", label)}
	}
	afterBegin := text[bi+len(begin):]
	ei := strings.Index(afterBegin, end)
	if ei < 0 {
		return &CodeError{Msg: fmt.Sprintf("expected a %s block", label)}
	}
	body := afterBegin[:ei]
	cleaned := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, body)

	raw, err := base64.StdEncoding.DecodeString(cleaned)
	if err != nil {
		return &CodeError{Msg: fmt.Sprintf("%s block is damaged; copy it again (%v)", label, err)}
	}
	zr, err := zlib.NewReader(bytes.NewReader(raw))
	if err != nil {
		return &CodeError{Msg: fmt.Sprintf("%s block is damaged; copy it again (%v)", label, err)}
	}
	defer func() { _ = zr.Close() }()
	plain, err := io.ReadAll(zr)
	if err != nil {
		return &CodeError{Msg: fmt.Sprintf("%s block is damaged; copy it again (%v)", label, err)}
	}
	if err := json.Unmarshal(plain, out); err != nil {
		return &CodeError{Msg: fmt.Sprintf("%s block is damaged; copy it again (%v)", label, err)}
	}
	return nil
}

// ReadBlock reads lines from r up to and including the first line containing endMarker
// without over-reading past that newline if r is not already buffered.
func ReadBlock(endMarker string, r io.Reader) (string, error) {
	if br, ok := r.(*bufio.Reader); ok {
		var out strings.Builder
		for {
			line, err := br.ReadString('\n')
			if line != "" {
				out.WriteString(line)
				if strings.Contains(line, endMarker) {
					return out.String(), nil
				}
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					return out.String(), nil
				}
				return out.String(), err
			}
		}
	}
	var out strings.Builder
	var lineBuf strings.Builder
	var b [1]byte
	for {
		n, err := r.Read(b[:])
		if n > 0 {
			out.WriteByte(b[0])
			lineBuf.WriteByte(b[0])
			if b[0] == '\n' {
				if strings.Contains(lineBuf.String(), endMarker) {
					return out.String(), nil
				}
				lineBuf.Reset()
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return out.String(), nil
			}
			return out.String(), err
		}
	}
}

// ShortID returns a human-comparable id (xxxx-xxxx-xxxx) from the SHA-256 of text.
func ShortID(text string) string {
	sum := sha256.Sum256([]byte(text))
	h := hex.EncodeToString(sum[:])[:12]
	return h[0:4] + "-" + h[4:8] + "-" + h[8:12]
}
