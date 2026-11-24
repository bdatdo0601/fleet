package codes

import (
	"crypto/rand"
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
