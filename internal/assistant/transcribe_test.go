package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeAudioTranscriber struct {
	text string
	err  error
	seen []string
}

func (f *fakeAudioTranscriber) FindBinary(_ string) string { return "/usr/local/bin/whisper-cli" }
func (f *fakeAudioTranscriber) FindFFmpeg() string         { return "/usr/local/bin/ffmpeg" }
func (f *fakeAudioTranscriber) ResolveModelPath(_ string) string {
	return "/tmp/ggml-base.en.bin"
}

func (f *fakeAudioTranscriber) Transcribe(_ context.Context, _ TranscribeConfig, audioPath string) (string, error) {
	f.seen = append(f.seen, audioPath)
	if f.err != nil {
		return "", f.err
	}
	return f.text, nil
}

func TestIsAudioFileAndLocalWhisperTranscriber(t *testing.T) {
	if !IsAudioFile("voice.ogg", "") || !IsAudioFile("memo.m4a", "application/octet-stream") || !IsAudioFile("blob.bin", "audio/mp4; codecs=mp4a") {
		t.Fatalf("expected audio files/MIME to be recognized")
	}
	if IsAudioFile("report.pdf", "application/pdf") || IsAudioFile("notes.md", "text/markdown") {
		t.Fatalf("expected non-audio files to return false")
	}

	tmp := t.TempDir()
	binDir := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ffLog := filepath.Join(tmp, "ff.log")
	ffBin := filepath.Join(binDir, "ffmpeg")
	if err := os.WriteFile(ffBin, []byte("#!/bin/sh\necho \"$*\" > \""+ffLog+"\"\nout=\"\"\nfor a in \"$@\"; do out=\"$a\"; done\nprintf 'RIFF' > \"$out\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	whBin := filepath.Join(binDir, "whisper-cli")
	if err := os.WriteFile(whBin, []byte("#!/bin/sh\necho \"  Buy oat milk tomorrow morning.  \"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	modelFile := filepath.Join(tmp, "ggml-test.bin")
	if err := os.WriteFile(modelFile, []byte("ggml"), 0o644); err != nil {
		t.Fatal(err)
	}
	audioFile := filepath.Join(tmp, "note.m4a")
	if err := os.WriteFile(audioFile, []byte("fake-audio"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	tr := NewLocalWhisperTranscriber()
	out, err := tr.Transcribe(t.Context(), TranscribeConfig{
		Enabled:   true,
		ModelPath: modelFile,
		Language:  "en",
	}, audioFile)
	if err != nil {
		t.Fatalf("LocalWhisperTranscriber failed: %v", err)
	}
	if out != "Buy oat milk tomorrow morning." {
		t.Fatalf("unexpected transcript %q", out)
	}
	ffArgs, _ := os.ReadFile(ffLog)
	if !strings.Contains(string(ffArgs), "-ar 16000 -ac 1") {
		t.Fatalf("expected ffmpeg 16kHz mono normalization, got %q", string(ffArgs))
	}
}

func TestAutoTranscribeChatIngressAndTools(t *testing.T) {
	var seenPrompt string
	llmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content any    `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		for _, m := range body.Messages {
			if m.Role == "user" {
				seenPrompt, _ = m.Content.(string)
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"Noted!\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
	}))
	defer llmSrv.Close()

	tmp := t.TempDir()
	eng, err := Open(filepath.Join(tmp, "assistant.db"), filepath.Join(tmp, "assistant.json"), filepath.Join(tmp, "vault"), &fakeDeps{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = eng.Close() }()

	fakeTr := &fakeAudioTranscriber{text: "Remind me to water the plants"}
	eng.cfgMu.Lock()
	eng.transcriber = fakeTr
	eng.cfgMu.Unlock()

	cfg := DefaultConfig()
	cfg.Enabled = true
	cfg.APIKey = "sk-test"
	cfg.BaseURL = llmSrv.URL
	cfg.Routing.Mode = RoutingModeOff
	cfg.Transcribe.Enabled = true
	if _, err := eng.UpdateConfig(cfg, false); err != nil {
		t.Fatal(err)
	}

	subCh := eng.Subscribe()
	defer eng.Unsubscribe(subCh)

	uploaded, err := eng.SaveUploadedFile("general", "memo.m4a", "audio/mp4", strings.NewReader("audio-bytes"))
	if err != nil {
		t.Fatal(err)
	}
	msg, _, err := eng.EnqueueUserMessageWithFiles("general", "", "", []string{uploaded.ID})
	if err != nil {
		t.Fatal(err)
	}
	ev, err := eng.store.ClaimNextInbox()
	if err != nil || ev == nil {
		t.Fatalf("ClaimNextInbox failed: %+v err=%v", ev, err)
	}
	if err := eng.processInboxEvent(t.Context(), *ev); err != nil {
		t.Fatalf("processInboxEvent failed: %v", err)
	}
	if !strings.Contains(seenPrompt, "Remind me to water the plants") {
		t.Fatalf("expected transcribed text in LLM user prompt, got %q", seenPrompt)
	}
	msgs, err := eng.store.ListMessages("general", 20)
	if err != nil {
		t.Fatal(err)
	}
	var updatedMsg *Message
	for i := range msgs {
		if msgs[i].ID == msg.ID {
			updatedMsg = &msgs[i]
			break
		}
	}
	if updatedMsg == nil || !strings.Contains(updatedMsg.Content, "Remind me to water the plants") {
		t.Fatalf("expected stored user message updated with transcript, got %+v", updatedMsg)
	}

	var sawUpdate bool
drainLoop:
	for {
		select {
		case evt := <-subCh:
			if evt.Type == "message_updated" {
				sawUpdate = true
			}
		default:
			break drainLoop
		}
	}
	if !sawUpdate {
		t.Fatalf("expected message_updated SSE event after auto-transcription")
	}

	// Graceful fallback when transcriber errors on arrival.
	fakeTr.err = errors.New("whisper crashed")
	up2, err := eng.SaveUploadedFile("general", "broken.ogg", "audio/ogg", strings.NewReader("bad-audio"))
	if err != nil {
		t.Fatal(err)
	}
	msg2, _, err := eng.EnqueueUserMessageWithFiles("general", "Listen to this", "", []string{up2.ID})
	if err != nil {
		t.Fatal(err)
	}
	ev2, err := eng.store.ClaimNextInbox()
	if err != nil || ev2 == nil {
		t.Fatal(err)
	}
	if err := eng.processInboxEvent(t.Context(), *ev2); err != nil {
		t.Fatalf("expected graceful fallback on transcription failure, got %v", err)
	}
	msgs2, _ := eng.store.ListMessages("general", 20)
	var m2 *Message
	for i := range msgs2 {
		if msgs2[i].ID == msg2.ID {
			m2 = &msgs2[i]
			break
		}
	}
	if m2 == nil || !strings.Contains(m2.Content, "Listen to this") || !strings.Contains(m2.Content, "transcription unavailable") {
		t.Fatalf("expected preserved caption + fallback note, got %+v", m2)
	}

	// transcribe_audio tool for sandbox and vault + path traversal rejection.
	fakeTr.err = nil
	fakeTr.text = "Vault recording transcript"
	workDir, _ := eng.sandbox.WorkDir("general")
	if err := os.WriteFile(filepath.Join(workDir, "in", "call.wav"), []byte("wav"), 0o644); err != nil {
		t.Fatal(err)
	}
	if v, err := eng.vault(); err == nil {
		_ = os.WriteFile(filepath.Join(v.Root, "Inbox", "voice.mp3"), []byte("mp3"), 0o644)
	}
	tctx := &toolExecContext{ChannelID: "general", Source: "chat", Config: eng.Config(), Widgets: &[]WidgetPayload{}}
	rawSB, err := eng.executeTool(t.Context(), ToolCall{
		ID: "tr1", Type: "function",
		Function: ToolCallFunction{Name: "transcribe_audio", Arguments: `{"source":"sandbox","path":"in/call.wav"}`},
	}, tctx)
	if err != nil || !strings.Contains(rawSB, "Vault recording transcript") {
		t.Fatalf("sandbox transcribe_audio failed: raw=%s err=%v", rawSB, err)
	}
	rawVault, err := eng.executeTool(t.Context(), ToolCall{
		ID: "tr2", Type: "function",
		Function: ToolCallFunction{Name: "transcribe_audio", Arguments: `{"source":"vault","path":"Inbox/voice.mp3"}`},
	}, tctx)
	if err != nil || !strings.Contains(rawVault, `"source":"vault"`) {
		t.Fatalf("vault transcribe_audio failed: raw=%s err=%v", rawVault, err)
	}
	if _, err := eng.executeTool(t.Context(), ToolCall{
		ID: "tr3", Type: "function",
		Function: ToolCallFunction{Name: "transcribe_audio", Arguments: `{"source":"vault","path":"../outside.mp3"}`},
	}, tctx); err == nil {
		t.Fatalf("expected path traversal rejection for transcribe_audio")
	}

	if doc := eng.checkTranscribeDoctor(eng.Config()); doc.Status != "ok" {
		t.Fatalf("expected transcribe doctor ok, got %+v", doc)
	}
}
