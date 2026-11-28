package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"text/tabwriter"
	"time"

	"golang.org/x/term"

	"github.com/bdatdo0601/fleet/internal/codes"
)

// IO holds overridable streams and prompt hooks for CLI testing.
type IO struct {
	Stdin         io.Reader
	Stdout        io.Writer
	Stderr        io.Writer
	AskPassphrase func(newPass bool) (string, error)
	AskSecret     func(prompt string) (string, error)
	ReadOPRef     func(ref string) (string, error)
	ConfirmFn     func(question string) error
	br            *bufio.Reader
	brSrc         io.Reader
}

func (cio *IO) stdinReader() *bufio.Reader {
	if cio.br == nil || cio.brSrc != cio.Stdin {
		cio.brSrc = cio.Stdin
		cio.br = bufio.NewReader(cio.Stdin)
	}
	return cio.br
}

func defaultIO() *IO {
	return &IO{
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	}
}

func (cio *IO) printf(format string, args ...any) {
	_, _ = fmt.Fprintf(cio.Stdout, format, args...)
}

func (cio *IO) info(msg string) {
	cio.printf("==> %s\n", msg)
}

func (cio *IO) confirm(question string) error {
	if cio.ConfirmFn != nil {
		return cio.ConfirmFn(question)
	}
	cio.printf("%s [y/N] ", question)
	line, _ := cio.stdinReader().ReadString('\n')
	ans := strings.ToLower(strings.TrimSpace(line))
	if ans != "y" && ans != "yes" {
		return fmt.Errorf("cancelled")
	}
	return nil
}

func (cio *IO) askPassphrase(newPass bool) (string, error) {
	if cio.AskPassphrase != nil {
		return cio.AskPassphrase(newPass)
	}
	readPass := func(prompt string) (string, error) {
		_, _ = fmt.Fprint(cio.Stderr, prompt)
		if f, ok := cio.Stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
			b, err := term.ReadPassword(int(f.Fd()))
			_, _ = fmt.Fprintln(cio.Stderr)
			if err != nil {
				return "", err
			}
			return string(b), nil
		}
		ln, err := cio.stdinReader().ReadString('\n')
		if err != nil && len(ln) == 0 {
			return "", err
		}
		return strings.TrimRight(ln, "\r\n"), nil
	}
	pw, err := readPass("CA passphrase: ")
	if err != nil {
		return "", err
	}
	if newPass {
		if len(pw) < 12 {
			return "", fmt.Errorf("use at least 12 characters")
		}
		pw2, err := readPass("Repeat passphrase: ")
		if err != nil {
			return "", err
		}
		if pw2 != pw {
			return "", fmt.Errorf("passphrases don't match")
		}
	}
	return pw, nil
}

func (cio *IO) askSecret(prompt string) (string, error) {
	if cio.AskSecret != nil {
		return cio.AskSecret(prompt)
	}
	_, _ = fmt.Fprint(cio.Stderr, prompt)
	if f, ok := cio.Stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		b, err := term.ReadPassword(int(f.Fd()))
		_, _ = fmt.Fprintln(cio.Stderr)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	}
	ln, err := cio.stdinReader().ReadString('\n')
	if err != nil && len(ln) == 0 {
		return "", err
	}
	return strings.TrimSpace(ln), nil
}

func (cio *IO) readOPRef(ref string) (string, error) {
	if cio.ReadOPRef != nil {
		return cio.ReadOPRef(ref)
	}
	if _, err := exec.LookPath("op"); err != nil {
		return "", fmt.Errorf("1Password CLI (`op`) not found: install with `brew install 1password-cli`")
	}
	out, err := exec.Command("op", "read", ref).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
			return "", fmt.Errorf("op read %s: %s", ref, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return "", fmt.Errorf("op read %s: %w", ref, err)
	}
	val := strings.TrimSpace(string(out))
	if val == "" {
		return "", fmt.Errorf("op read %s returned empty secret", ref)
	}
	return val, nil
}

func (cio *IO) paste(prompt, endMarker string) (string, error) {
	if f, ok := cio.Stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		cio.printf("%s\n", prompt)
	}
	text, err := codes.ReadBlock(endMarker, cio.stdinReader())
	if err != nil {
		return "", err
	}
	if !strings.Contains(text, endMarker) {
		return "", fmt.Errorf("input ended before the end of the block")
	}
	return text, nil
}

func writeJSONOut(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func ago(ts *string) string {
	if ts == nil || *ts == "" {
		return "never"
	}
	t, err := time.Parse(time.RFC3339, *ts)
	if err != nil {
		return *ts
	}
	secs := max(int(time.Since(t).Seconds()), 0)
	for _, unit := range []struct {
		label string
		size  int
	}{{"d", 86400}, {"h", 3600}, {"m", 60}} {
		if secs >= unit.size {
			return fmt.Sprintf("%d%s ago", secs/unit.size, unit.label)
		}
	}
	return "just now"
}

func printTable(w io.Writer, rows [][]string) {
	if len(rows) == 0 {
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, r := range rows {
		_, _ = fmt.Fprintln(tw, strings.Join(r, "\t"))
	}
	_ = tw.Flush()
}
