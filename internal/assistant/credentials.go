package assistant

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"time"
)

// OPReaderFunc resolves a 1Password op:// reference without persisting plaintext.
type OPReaderFunc func(ctx context.Context, ref string) (string, error)

// CredentialHTTPRequest defines an outbound HTTP call authenticated via a 1Password alias.
type CredentialHTTPRequest struct {
	Alias   string            `json:"alias"`
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
}

func defaultOPReader(ctx context.Context, ref string) (string, error) {
	if _, err := exec.LookPath("op"); err != nil {
		return "", errors.New("1Password CLI (`op`) not found: install with `brew install 1password-cli`")
	}
	out, err := exec.CommandContext(ctx, "op", "read", ref).Output()
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

func findCredentialPolicy(cfg Config, alias string) (CredentialPolicy, bool) {
	want := strings.TrimSpace(alias)
	for _, cp := range cfg.Credentials {
		if strings.EqualFold(strings.TrimSpace(cp.Alias), want) {
			return cp, true
		}
	}
	return CredentialPolicy{}, false
}

func hostAllowed(hostname string, allowed []string) bool {
	h := strings.ToLower(strings.TrimSpace(hostname))
	if h == "" {
		return false
	}
	for _, raw := range allowed {
		pat := strings.ToLower(strings.TrimSpace(raw))
		if pat == "" {
			continue
		}
		if strings.HasPrefix(pat, "*.") {
			suffix := pat[1:] // ".example.com"
			if strings.HasSuffix(h, suffix) && len(h) > len(suffix) {
				return true
			}
			continue
		}
		if h == pat {
			return true
		}
	}
	return false
}

func scrubSecret(text, secret string) string {
	if secret == "" {
		return text
	}
	out := strings.ReplaceAll(text, secret, "[REDACTED]")
	if esc := url.QueryEscape(secret); esc != "" && esc != secret {
		out = strings.ReplaceAll(out, esc, "[REDACTED]")
	}
	return out
}

// ExecuteCredentialHTTP resolves a 1Password secret in memory, enforces host allowlisting,
// sends the HTTP request, and scrubs any echoed secret before returning the response.
func ExecuteCredentialHTTP(
	ctx context.Context,
	cfg Config,
	reader OPReaderFunc,
	httpClient *http.Client,
	reqSpec CredentialHTTPRequest,
) (map[string]any, error) {
	cp, ok := findCredentialPolicy(cfg, reqSpec.Alias)
	if !ok {
		return nil, newErr(fmt.Sprintf("unknown credential alias %q", reqSpec.Alias), http.StatusNotFound)
	}
	u, err := url.Parse(strings.TrimSpace(reqSpec.URL))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" {
		return nil, newErr("valid http/https url is required", http.StatusBadRequest)
	}
	if !hostAllowed(u.Hostname(), cp.AllowedHosts) {
		return nil, newErr(fmt.Sprintf("host %q is not allowlisted for credential %q", u.Hostname(), cp.Alias), http.StatusForbidden)
	}
	if reader == nil {
		reader = defaultOPReader
	}
	secret, err := reader(ctx, cp.OPRef)
	if err != nil {
		return nil, err
	}

	method := strings.ToUpper(cmp.Or(strings.TrimSpace(reqSpec.Method), http.MethodGet))
	var bodyReader io.Reader
	if reqSpec.Body != "" {
		bodyReader = strings.NewReader(reqSpec.Body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bodyReader)
	if err != nil {
		return nil, err
	}
	for k, v := range reqSpec.Headers {
		req.Header.Set(k, v)
	}
	headerName := cmp.Or(strings.TrimSpace(cp.HeaderName), "Authorization")
	headerVal := cp.HeaderPrefix + secret
	if cp.HeaderName == "" && cp.HeaderPrefix == "" {
		headerVal = "Bearer " + secret
	}
	req.Header.Set(headerName, headerVal)

	cli := httpClient
	if cli == nil {
		cli = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := cli.Do(req)
	if err != nil {
		return nil, errors.New(scrubSecret(err.Error(), secret))
	}
	defer func() { _ = resp.Body.Close() }()

	rawResp, err := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if err != nil {
		return nil, errors.New(scrubSecret(err.Error(), secret))
	}
	return map[string]any{
		"status":       resp.StatusCode,
		"content_type": resp.Header.Get("Content-Type"),
		"body":         scrubSecret(string(rawResp), secret),
	}, nil
}
