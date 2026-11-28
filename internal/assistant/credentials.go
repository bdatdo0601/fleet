package assistant

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// OPReaderFunc resolves a 1Password op:// reference without persisting plaintext.
type OPReaderFunc func(ctx context.Context, ref string) (string, error)

// CredentialHTTPRequest defines an outbound HTTP call authenticated via a 1Password alias or op:// ref.
type CredentialHTTPRequest struct {
	Alias        string            `json:"alias,omitempty"`
	OPRef        string            `json:"op_ref,omitempty"`
	AllowedHosts []string          `json:"allowed_hosts,omitempty"`
	HeaderName   string            `json:"header_name,omitempty"`
	HeaderPrefix string            `json:"header_prefix,omitempty"`
	Method       string            `json:"method"`
	URL          string            `json:"url"`
	Headers      map[string]string `json:"headers,omitempty"`
	Body         string            `json:"body,omitempty"`
}

// OPVaultMeta describes a 1Password vault visible to the assistant's Service Account.
type OPVaultMeta struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// OPItemMeta describes a 1Password item without any secret values.
type OPItemMeta struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Category  string   `json:"category"`
	VaultID   string   `json:"vault_id,omitempty"`
	VaultName string   `json:"vault_name,omitempty"`
	URLs      []string `json:"urls,omitempty"`
	Tags      []string `json:"tags,omitempty"`
}

// OPFieldMeta describes a field inside a 1Password item without its secret value.
type OPFieldMeta struct {
	ID        string `json:"id,omitempty"`
	Label     string `json:"label"`
	Type      string `json:"type"`
	Reference string `json:"reference,omitempty"`
}

// OPItemDetailMeta describes a 1Password item's metadata and op:// field references (never values).
type OPItemDetailMeta struct {
	ID        string        `json:"id"`
	Title     string        `json:"title"`
	Category  string        `json:"category"`
	VaultName string        `json:"vault_name,omitempty"`
	URLs      []string      `json:"urls,omitempty"`
	Tags      []string      `json:"tags,omitempty"`
	Fields    []OPFieldMeta `json:"fields"`
}

// ResolveOPToken returns OP_SERVICE_ACCOUNT_TOKEN from env or tokenPath (0600 file).
func ResolveOPToken(tokenPath string) string {
	if v := strings.TrimSpace(os.Getenv("OP_SERVICE_ACCOUNT_TOKEN")); v != "" {
		return v
	}
	if strings.TrimSpace(tokenPath) == "" {
		return ""
	}
	b, err := os.ReadFile(tokenPath)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// SaveOPToken atomically persists or removes the 1Password Service Account token file (0600).
func SaveOPToken(tokenPath, token string) error {
	if strings.TrimSpace(tokenPath) == "" {
		return errors.New("op token path is required")
	}
	tok := strings.TrimSpace(token)
	if tok == "" {
		if err := os.Remove(tokenPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(tokenPath), 0o700); err != nil {
		return err
	}
	tmp := tokenPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(tok+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, tokenPath)
}

func opCommand(ctx context.Context, tokenPath string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "op", args...)
	cmd.Stdin = nil
	if tok := ResolveOPToken(tokenPath); tok != "" {
		cmd.Env = append(os.Environ(), "OP_SERVICE_ACCOUNT_TOKEN="+tok)
	}
	return cmd
}

// NewOPReader returns an OPReaderFunc that reads op:// refs using ResolveOPToken(tokenPath).
func NewOPReader(tokenPath string) OPReaderFunc {
	return func(ctx context.Context, ref string) (string, error) {
		if _, err := exec.LookPath("op"); err != nil {
			return "", errors.New("1Password CLI (`op`) not found: install with `brew install 1password-cli`")
		}
		out, err := opCommand(ctx, tokenPath, "read", ref).Output()
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
}

func defaultOPReader(ctx context.Context, ref string) (string, error) {
	return NewOPReader("")(ctx, ref)
}

// CheckOPHealth verifies `op` CLI availability, service account token presence, and non-interactive auth.
func CheckOPHealth(ctx context.Context, tokenPath string) (binPath string, hasToken bool, identity string, err error) {
	binPath, err = exec.LookPath("op")
	if err != nil {
		return "", false, "", errors.New("op CLI not found")
	}
	hasToken = ResolveOPToken(tokenPath) != ""
	if !hasToken {
		return binPath, false, "", errors.New("OP_SERVICE_ACCOUNT_TOKEN not configured")
	}
	cCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, wErr := opCommand(cCtx, tokenPath, "whoami", "--format", "json").Output()
	if wErr == nil {
		var who struct {
			Email       string `json:"email"`
			AccountUUID string `json:"account_uuid"`
			UserUUID    string `json:"user_uuid"`
			URL         string `json:"url"`
		}
		if json.Unmarshal(out, &who) == nil {
			identity = cmp.Or(who.Email, who.URL, who.UserUUID, "service-account")
		} else {
			identity = "service-account"
		}
		return binPath, true, identity, nil
	}
	// Some service accounts only allow `op vault list`.
	if vOut, vErr := opCommand(cCtx, tokenPath, "vault", "list", "--format", "json").Output(); vErr == nil {
		var vaults []struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(vOut, &vaults) == nil {
			return binPath, true, fmt.Sprintf("%d vault(s)", len(vaults)), nil
		}
		return binPath, true, "authenticated", nil
	}
	var exitErr *exec.ExitError
	if errors.As(wErr, &exitErr) && len(exitErr.Stderr) > 0 {
		return binPath, true, "", errors.New(strings.TrimSpace(string(exitErr.Stderr)))
	}
	return binPath, true, "", wErr
}

func runOPJSON(ctx context.Context, tokenPath string, args ...string) ([]byte, error) {
	if _, err := exec.LookPath("op"); err != nil {
		return nil, errors.New("1Password CLI (`op`) not found on lighthouse")
	}
	cCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := opCommand(cCtx, tokenPath, args...).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
			return nil, fmt.Errorf("op %s: %s", strings.Join(args[:min(2, len(args))], " "), strings.TrimSpace(string(exitErr.Stderr)))
		}
		return nil, err
	}
	return out, nil
}

// ListOPVaults lists 1Password vaults accessible to the assistant Service Account.
func ListOPVaults(ctx context.Context, tokenPath string) ([]OPVaultMeta, error) {
	out, err := runOPJSON(ctx, tokenPath, "vault", "list", "--format", "json")
	if err != nil {
		return nil, err
	}
	var vaults []OPVaultMeta
	if err := json.Unmarshal(out, &vaults); err != nil {
		return nil, err
	}
	return vaults, nil
}

// ListOPItems lists item metadata (title, category, vault, URLs, tags — no secrets).
func ListOPItems(ctx context.Context, tokenPath, vault string) ([]OPItemMeta, error) {
	args := []string{"item", "list", "--format", "json"}
	if v := strings.TrimSpace(vault); v != "" {
		args = append(args, "--vault", v)
	}
	out, err := runOPJSON(ctx, tokenPath, args...)
	if err != nil {
		return nil, err
	}
	var raw []struct {
		ID       string `json:"id"`
		Title    string `json:"title"`
		Category string `json:"category"`
		Vault    struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"vault"`
		URLs []struct {
			Href string `json:"href"`
		} `json:"urls"`
		Tags []string `json:"tags"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, err
	}
	items := make([]OPItemMeta, 0, len(raw))
	for _, r := range raw {
		urls := make([]string, 0, len(r.URLs))
		for _, u := range r.URLs {
			if u.Href != "" {
				urls = append(urls, u.Href)
			}
		}
		items = append(items, OPItemMeta{
			ID:        r.ID,
			Title:     r.Title,
			Category:  r.Category,
			VaultID:   r.Vault.ID,
			VaultName: r.Vault.Name,
			URLs:      urls,
			Tags:      r.Tags,
		})
	}
	return items, nil
}

func parseOPItemDetailMeta(raw []byte) (OPItemDetailMeta, error) {
	var doc struct {
		ID       string `json:"id"`
		Title    string `json:"title"`
		Category string `json:"category"`
		Vault    struct {
			Name string `json:"name"`
		} `json:"vault"`
		URLs []struct {
			Href string `json:"href"`
		} `json:"urls"`
		Tags   []string `json:"tags"`
		Fields []struct {
			ID        string `json:"id"`
			Label     string `json:"label"`
			Type      string `json:"type"`
			Reference string `json:"reference"`
		} `json:"fields"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return OPItemDetailMeta{}, err
	}
	urls := make([]string, 0, len(doc.URLs))
	for _, u := range doc.URLs {
		if u.Href != "" {
			urls = append(urls, u.Href)
		}
	}
	fields := make([]OPFieldMeta, 0, len(doc.Fields))
	for _, f := range doc.Fields {
		if f.Label == "" && f.Reference == "" {
			continue
		}
		fields = append(fields, OPFieldMeta{
			ID:        f.ID,
			Label:     f.Label,
			Type:      f.Type,
			Reference: f.Reference,
		})
	}
	return OPItemDetailMeta{
		ID:        doc.ID,
		Title:     doc.Title,
		Category:  doc.Category,
		VaultName: doc.Vault.Name,
		URLs:      urls,
		Tags:      doc.Tags,
		Fields:    fields,
	}, nil
}

// GetOPItemMeta returns field labels, types, URLs, and op:// references without any secret values.
func GetOPItemMeta(ctx context.Context, tokenPath, item, vault string) (OPItemDetailMeta, error) {
	if strings.TrimSpace(item) == "" {
		return OPItemDetailMeta{}, newErr("item name or ID is required", http.StatusBadRequest)
	}
	args := []string{"item", "get", strings.TrimSpace(item), "--format", "json"}
	if v := strings.TrimSpace(vault); v != "" {
		args = append(args, "--vault", v)
	}
	out, err := runOPJSON(ctx, tokenPath, args...)
	if err != nil {
		return OPItemDetailMeta{}, err
	}
	return parseOPItemDetailMeta(out)
}

// StoreOPItem creates a new item in a scoped 1Password vault and returns metadata only.
func StoreOPItem(ctx context.Context, tokenPath, vault, title, category, itemURL string, fields map[string]string) (OPItemDetailMeta, error) {
	if strings.TrimSpace(vault) == "" || strings.TrimSpace(title) == "" {
		return OPItemDetailMeta{}, newErr("vault and title are required", http.StatusBadRequest)
	}
	args := []string{
		"item", "create",
		"--vault", strings.TrimSpace(vault),
		"--category", cmp.Or(strings.TrimSpace(category), "API Credential"),
		"--title", strings.TrimSpace(title),
		"--format", "json",
	}
	if u := strings.TrimSpace(itemURL); u != "" {
		args = append(args, "--url", u)
	}
	for k, v := range fields {
		if strings.TrimSpace(k) == "" {
			continue
		}
		args = append(args, strings.TrimSpace(k)+"="+v)
	}
	out, err := runOPJSON(ctx, tokenPath, args...)
	if err != nil {
		return OPItemDetailMeta{}, err
	}
	return parseOPItemDetailMeta(out)
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
	var opRef, headerName, headerPrefix, label string
	var allowedHosts []string
	if strings.TrimSpace(reqSpec.OPRef) != "" {
		opRef = strings.TrimSpace(reqSpec.OPRef)
		allowedHosts = reqSpec.AllowedHosts
		headerName = reqSpec.HeaderName
		headerPrefix = reqSpec.HeaderPrefix
		label = opRef
	} else {
		cp, ok := findCredentialPolicy(cfg, reqSpec.Alias)
		if !ok {
			return nil, newErr(fmt.Sprintf("unknown credential alias %q", reqSpec.Alias), http.StatusNotFound)
		}
		opRef = cp.OPRef
		allowedHosts = cp.AllowedHosts
		headerName = cp.HeaderName
		headerPrefix = cp.HeaderPrefix
		label = cp.Alias
	}
	u, err := url.Parse(strings.TrimSpace(reqSpec.URL))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" {
		return nil, newErr("valid http/https url is required", http.StatusBadRequest)
	}
	if !hostAllowed(u.Hostname(), allowedHosts) {
		return nil, newErr(fmt.Sprintf("host %q is not allowlisted for credential %q", u.Hostname(), label), http.StatusForbidden)
	}
	if reader == nil {
		reader = defaultOPReader
	}
	secret, err := reader(ctx, opRef)
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
	hdrName := cmp.Or(strings.TrimSpace(headerName), "Authorization")
	hdrVal := headerPrefix + secret
	if headerName == "" && headerPrefix == "" {
		hdrVal = "Bearer " + secret
	}
	req.Header.Set(hdrName, hdrVal)

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
