package codes

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
)

func TestRoundtripWrapsLinesForTerminalPaste(t *testing.T) {
	raw := make([]byte, 3000)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	payload := map[string]string{"blob": hex.EncodeToString(raw)}
	block, err := Encode(Response, payload)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(block, "\n")
	for _, line := range lines[1 : len(lines)-1] {
		if len(line) > 64 {
			t.Fatalf("line too long: %d", len(line))
		}
	}
	noisy := "some prompt text\n  " + strings.ReplaceAll(block, "\n", "\n  ") + "  \ntrailing"
	var got map[string]string
	if err := Decode(Response, noisy, &got); err != nil {
		t.Fatal(err)
	}
	if got["blob"] != payload["blob"] {
		t.Fatalf("mismatch after decode")
	}
}

func TestDamagedOrWrongBlock(t *testing.T) {
	block, err := Encode(Request, map[string]int{"a": 1})
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := Decode(Response, block, &out); err == nil {
		t.Fatal("expected error for mismatched label")
	}
	lines := strings.Split(block, "\n")
	if len(lines) < 3 || len(lines[1]) < 4 {
		t.Fatalf("unexpected encoded shape: %q", block)
	}
	lines[1] = lines[1][:len(lines[1])-4] + "AAAA"
	if err := Decode(Request, strings.Join(lines, "\n"), &out); err == nil {
		t.Fatal("expected error for corrupted block")
	}
}

func TestReadBlockStopsAtEndMarker(t *testing.T) {
	stream := strings.NewReader("x\n-----END FLEET JOIN REQUEST-----\nnot read\n")
	got, err := ReadBlock("END FLEET JOIN REQUEST", stream)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "not read") {
		t.Fatalf("read past end marker: %q", got)
	}
}

func TestShortIDFormat(t *testing.T) {
	id := ShortID("hello world")
	parts := strings.Split(id, "-")
	if len(parts) != 3 || len(id) != 14 {
		t.Fatalf("unexpected short id: %q", id)
	}
}

func TestDecodeRejectsZipBombAndReadBlockCap(t *testing.T) {
	// Compress > 1 MiB of zeros (highly compressible zip bomb).
	var comp bytes.Buffer
	zw := zlib.NewWriter(&comp)
	zeros := make([]byte, maxDecodedBytes+4096)
	if _, err := zw.Write(zeros); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	bomb := "-----BEGIN " + Response + "-----\n" +
		base64.StdEncoding.EncodeToString(comp.Bytes()) +
		"\n-----END " + Response + "-----"

	var out map[string]any
	err := Decode(Response, bomb, &out)
	if err == nil || !strings.Contains(err.Error(), "maximum decoded size") {
		t.Fatalf("expected zip-bomb rejection, got %v", err)
	}

	oversized := strings.Repeat("x", maxBlockBytes+64)
	if _, err := ReadBlock("END FLEET JOIN REQUEST", strings.NewReader(oversized)); err == nil || !strings.Contains(err.Error(), "maximum size") {
		t.Fatalf("expected ReadBlock unbuffered cap error, got %v", err)
	}
	if _, err := ReadBlock("END FLEET JOIN REQUEST", bufio.NewReader(strings.NewReader(oversized))); err == nil || !strings.Contains(err.Error(), "maximum size") {
		t.Fatalf("expected ReadBlock buffered cap error, got %v", err)
	}
}
