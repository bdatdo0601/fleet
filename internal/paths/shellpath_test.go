package paths

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMergePathEnv(t *testing.T) {
	got := MergePathEnv(
		"/a/bin/:/b/bin::/bad\nline:/bad\rline:/bad\x00line:/",
		"/b/bin:/a/bin:/c/bin/:/",
		"",
	)
	want := "/a/bin/:/b/bin:/:/c/bin/"
	if got != want {
		t.Fatalf("MergePathEnv = %q, want %q", got, want)
	}
}

func TestParseShellPathOutput(t *testing.T) {
	cases := []struct {
		name   string
		input  string
		want   string
		wantOK bool
	}{
		{
			name:   "clean",
			input:  "\n" + shellPathStartMarker + "/a/bin:/b/bin" + shellPathEndMarker + "\n",
			want:   "/a/bin:/b/bin",
			wantOK: true,
		},
		{
			name:   "noisy_ansi_and_invalid_utf8",
			input:  "\x1b]1337;Warp\x9c\nprompt noise\n" + shellPathStartMarker + "/proto/shims:/opt/homebrew/bin" + shellPathEndMarker + "\x1b[0m\nbye",
			want:   "/proto/shims:/opt/homebrew/bin",
			wantOK: true,
		},
		{
			name: "multiple_sentinel_pairs_picks_last",
			input: shellPathStartMarker + "/old/bin" + shellPathEndMarker + "\n" +
				shellPathStartMarker + "/new/bin:/usr/bin" + shellPathEndMarker + "\n",
			want:   "/new/bin:/usr/bin",
			wantOK: true,
		},
		{
			name:   "missing_sentinels",
			input:  "/usr/bin:/bin",
			want:   "",
			wantOK: false,
		},
		{
			name:   "embedded_newline_rejected",
			input:  shellPathStartMarker + "/a/bin\n/b/bin" + shellPathEndMarker,
			want:   "",
			wantOK: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseShellPathOutput(tc.input)
			if ok != tc.wantOK || got != tc.want {
				t.Fatalf("parseShellPathOutput = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestParseDSCLUserShell(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "normal",
			input: "UserShell: /bin/zsh\n",
			want:  "/bin/zsh",
		},
		{
			name:  "extra_lines_and_whitespace",
			input: "RecordName: alice\n  UserShell:   /opt/homebrew/bin/fish  \nRealName: Alice\n",
			want:  "/opt/homebrew/bin/fish",
		},
		{
			name:  "missing_key",
			input: "RecordName: alice\nNFSHomeDirectory: /Users/alice\n",
			want:  "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseDSCLUserShell(tc.input); got != tc.want {
				t.Fatalf("parseDSCLUserShell = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLoginShellPathFromDotfiles(t *testing.T) {
	if _, err := os.Stat("/bin/zsh"); err != nil {
		t.Skip("/bin/zsh not installed")
	}
	t.Setenv("ZDOTDIR", "")

	home := t.TempDir()
	zprofile := "export PATH=\"$HOME/zprofile-bin:$PATH\"\n"
	zshrc := "echo 'noisy startup'\nprintf '\\033]0;title\\234'\nexport PATH=\"$HOME/zshrc-bin:$PATH\"\n"
	if err := os.WriteFile(filepath.Join(home, ".zprofile"), []byte(zprofile), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".zshrc"), []byte(zshrc), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := LoginShellPath(context.Background(), "/bin/zsh", home, "tester")
	if err != nil {
		t.Fatalf("LoginShellPath failed: %v", err)
	}
	parts := strings.Split(got, string(os.PathListSeparator))
	rcIdx := -1
	profIdx := -1
	for i, p := range parts {
		if p == filepath.Join(home, "zshrc-bin") && rcIdx < 0 {
			rcIdx = i
		}
		if p == filepath.Join(home, "zprofile-bin") && profIdx < 0 {
			profIdx = i
		}
	}
	if rcIdx < 0 || profIdx < 0 {
		t.Fatalf("expected both zshrc-bin and zprofile-bin in PATH, got %q", got)
	}
	if rcIdx > profIdx {
		t.Fatalf("expected zshrc-bin (idx %d) before zprofile-bin (idx %d) in %q", rcIdx, profIdx, got)
	}
	if strings.Contains(got, "noisy startup") {
		t.Fatalf("startup noise leaked into PATH: %q", got)
	}
}

func TestLoginShellPathFallbackToNonInteractive(t *testing.T) {
	dir := t.TempDir()
	fakeShell := filepath.Join(dir, "fakeshell")
	script := `#!/bin/sh
has_i=0
for arg in "$@"; do
  if [ "$arg" = "-i" ]; then
    has_i=1
  fi
done
if [ "$has_i" = "1" ]; then
  exit 1
fi
printf '\n%s/fallback/bin:/usr/bin%s\n' "` + shellPathStartMarker + `" "` + shellPathEndMarker + `"
`
	if err := os.WriteFile(fakeShell, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := LoginShellPath(context.Background(), fakeShell, dir, "tester")
	if err != nil {
		t.Fatalf("expected fallback to succeed, got err: %v", err)
	}
	if got != "/fallback/bin:/usr/bin" {
		t.Fatalf("LoginShellPath = %q, want %q", got, "/fallback/bin:/usr/bin")
	}
}
