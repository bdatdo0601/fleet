package assistant

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const transcribeOutputCapBytes = 256 * 1024

var audioExtensions = map[string]bool{
	".ogg":  true,
	".opus": true,
	".m4a":  true,
	".mp3":  true,
	".wav":  true,
	".flac": true,
	".aac":  true,
	".webm": true,
}

// IsAudioFile reports whether name or mimeType identifies an audio/voice-note file.
func IsAudioFile(name, mimeType string) bool {
	base := strings.ToLower(strings.TrimSpace(strings.Split(mimeType, ";")[0]))
	if strings.HasPrefix(base, "audio/") {
		return true
	}
	ext := strings.ToLower(filepath.Ext(strings.TrimSpace(name)))
	return audioExtensions[ext]
}

// AudioTranscriber abstracts local audio transcription so Engine is unit-testable without whisper-cli.
type AudioTranscriber interface {
	FindBinary(configured string) string
	FindFFmpeg() string
	ResolveModelPath(configured string) string
	Transcribe(ctx context.Context, cfg TranscribeConfig, audioPath string) (string, error)
}

// LocalWhisperTranscriber transcribes audio locally using ffmpeg + whisper-cli (whisper.cpp).
type LocalWhisperTranscriber struct{}

// NewLocalWhisperTranscriber constructs a LocalWhisperTranscriber.
func NewLocalWhisperTranscriber() *LocalWhisperTranscriber {
	return &LocalWhisperTranscriber{}
}

func findExecutable(names, candidates []string) string {
	for _, n := range names {
		if n == "" {
			continue
		}
		if strings.ContainsRune(n, os.PathSeparator) {
			if fi, err := os.Stat(n); err == nil && !fi.IsDir() {
				return n
			}
			continue
		}
		if p, err := exec.LookPath(n); err == nil {
			return p
		}
	}
	for _, cand := range candidates {
		if fi, err := os.Stat(cand); err == nil && !fi.IsDir() {
			return cand
		}
	}
	return ""
}

// FindBinary locates whisper-cli (or configured command) across PATH and Homebrew.
func (t *LocalWhisperTranscriber) FindBinary(configured string) string {
	var names []string
	if clean := strings.TrimSpace(configured); clean != "" {
		names = append(names, clean)
	}
	names = append(names, "whisper-cli", "whisper-cpp")
	return findExecutable(names, []string{
		"/opt/homebrew/bin/whisper-cli",
		"/usr/local/bin/whisper-cli",
		"/opt/homebrew/bin/whisper-cpp",
		"/usr/local/bin/whisper-cpp",
	})
}

// FindFFmpeg locates ffmpeg across PATH and Homebrew.
func (t *LocalWhisperTranscriber) FindFFmpeg() string {
	return findExecutable([]string{"ffmpeg"}, []string{
		"/opt/homebrew/bin/ffmpeg",
		"/usr/local/bin/ffmpeg",
	})
}

func expandHomePath(p string) string {
	clean := strings.TrimSpace(p)
	if clean == "" {
		return ""
	}
	if clean == "~" || strings.HasPrefix(clean, "~/") {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			if clean == "~" {
				return home
			}
			return filepath.Join(home, clean[2:])
		}
	}
	return clean
}

// ResolveModelPath resolves the configured whisper GGML model file or probes standard locations.
func (t *LocalWhisperTranscriber) ResolveModelPath(configured string) string {
	if exp := expandHomePath(configured); exp != "" {
		if fi, err := os.Stat(exp); err == nil && !fi.IsDir() {
			return exp
		}
		return ""
	}
	var cands []string
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		cands = append(cands,
			filepath.Join(home, ".fleet", "models", "ggml-large-v3-turbo-q5_0.bin"),
			filepath.Join(home, ".fleet", "models", "ggml-large-v3-turbo.bin"),
			filepath.Join(home, ".fleet", "models", "ggml-base.en.bin"),
		)
	}
	for _, glob := range []string{
		"/opt/homebrew/share/whisper-cpp/*.bin",
		"/usr/local/share/whisper-cpp/*.bin",
	} {
		if matches, err := filepath.Glob(glob); err == nil && len(matches) > 0 {
			sort.Strings(matches)
			cands = append(cands, matches...)
		}
	}
	for _, cand := range cands {
		if fi, err := os.Stat(cand); err == nil && !fi.IsDir() {
			return cand
		}
	}
	return ""
}

// Transcribe normalizes audioPath to 16 kHz mono WAV via ffmpeg and transcribes it using whisper-cli.
func (t *LocalWhisperTranscriber) Transcribe(ctx context.Context, cfg TranscribeConfig, audioPath string) (string, error) {
	bin := t.FindBinary(cfg.Command)
	if bin == "" {
		return "", newErr("whisper-cli not found (install with `brew install whisper-cpp`)", http.StatusServiceUnavailable)
	}
	ffmpegBin := t.FindFFmpeg()
	if ffmpegBin == "" {
		return "", newErr("ffmpeg not found (install with `brew install ffmpeg`)", http.StatusServiceUnavailable)
	}
	modelPath := t.ResolveModelPath(cfg.ModelPath)
	if modelPath == "" {
		return "", newErr("whisper model file not found (set transcribe.model_path or place ggml-large-v3-turbo-q5_0.bin in ~/.fleet/models/)", http.StatusFailedDependency)
	}

	runCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	tmpWav, err := os.CreateTemp("", "fleet-transcribe-*.wav")
	if err != nil {
		return "", err
	}
	wavPath := tmpWav.Name()
	_ = tmpWav.Close()
	defer func() { _ = os.Remove(wavPath) }()

	ffOut, ffErr := exec.CommandContext(runCtx, ffmpegBin, "-y", "-i", audioPath, "-ar", "16000", "-ac", "1", "-c:a", "pcm_s16le", wavPath).CombinedOutput()
	if ffErr != nil {
		return "", fmt.Errorf("ffmpeg audio conversion failed: %w (%s)", ffErr, truncateTailText(string(ffOut), 400))
	}

	args := []string{"-m", modelPath, "-f", wavPath, "-nt", "-np"}
	if lang := strings.TrimSpace(cfg.Language); lang != "" && !strings.EqualFold(lang, "auto") {
		args = append(args, "-l", lang)
	}
	cmd := exec.CommandContext(runCtx, bin, args...)
	stdout := &cappedBuffer{limit: transcribeOutputCapBytes}
	var stderr bytes.Buffer
	cmd.Stdout = stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("whisper-cli failed: %w (%s)", err, truncateTailText(cmp.Or(stderr.String(), stdout.buf.String()), 400))
	}
	text := strings.TrimSpace(stdout.buf.String())
	if text == "" {
		return "", nil
	}
	return text, nil
}
