package assistant

import (
	"bytes"
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
	"slices"
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

func opCommand(ctx context.Context, tokenPath string, stdin []byte, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "op", args...)
	if len(stdin) > 0 {
		cmd.Stdin = bytes.NewReader(stdin)
	} else {
		cmd.Stdin = nil
	}
	if tok := ResolveOPToken(tokenPath); tok != "" {
		cmd.Env = append(os.Environ(), "OP_SERVICE_ACCOUNT_TOKEN="+tok)
	}
	return cmd
}

func validateOPPositionalArg(val, label string) (string, error) {
	clean := strings.TrimSpace(val)
	if clean != "" && strings.HasPrefix(clean, "-") {
		return "", newErr(fmt.Sprintf("invalid %s: must not start with '-'", label), http.StatusBadRequest)
	}
	return clean, nil
}

// NewOPReader returns an OPReaderFunc that reads op:// refs using ResolveOPToken(tokenPath).
func NewOPReader(tokenPath string) OPReaderFunc {
	return func(ctx context.Context, ref string) (string, error) {
		cleanRef, err := validateOPPositionalArg(ref, "1Password reference")
		if err != nil {
			return "", err
		}
		if cleanRef == "" {
			return "", newErr("1Password reference is required", http.StatusBadRequest)
		}
		if _, err := exec.LookPath("op"); err != nil {
			return "", errors.New("1Password CLI (`op`) not found: install with `brew install 1password-cli`")
		}
		out, err := opCommand(ctx, tokenPath, nil, "read", "--", cleanRef).Output()
		if err != nil {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
				return "", fmt.Errorf("op read %s: %s", cleanRef, strings.TrimSpace(string(exitErr.Stderr)))
			}
			return "", fmt.Errorf("op read %s: %w", cleanRef, err)
		}
		val := strings.TrimSpace(string(out))
		if val == "" {
			return "", fmt.Errorf("op read %s returned empty secret", cleanRef)
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
	out, wErr := opCommand(cCtx, tokenPath, nil, "whoami", "--format", "json").Output()
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
	if vOut, vErr := opCommand(cCtx, tokenPath, nil, "vault", "list", "--format", "json").Output(); vErr == nil {
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
	return runOPJSONWithStdin(ctx, tokenPath, nil, args...)
}

func runOPJSONWithStdin(ctx context.Context, tokenPath string, stdin []byte, args ...string) ([]byte, error) {
	if _, err := exec.LookPath("op"); err != nil {
		return nil, errors.New("1Password CLI (`op`) not found on lighthouse")
	}
	cCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := opCommand(cCtx, tokenPath, stdin, args...).Output()
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
	cleanVault, err := validateOPPositionalArg(vault, "vault")
	if err != nil {
		return nil, err
	}
	args := []string{"item", "list", "--format", "json"}
	if cleanVault != "" {
		args = append(args, "--vault", cleanVault)
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
	cleanItem, err := validateOPPositionalArg(item, "item")
	if err != nil {
		return OPItemDetailMeta{}, err
	}
	if cleanItem == "" {
		return OPItemDetailMeta{}, newErr("item name or ID is required", http.StatusBadRequest)
	}
	cleanVault, err := validateOPPositionalArg(vault, "vault")
	if err != nil {
		return OPItemDetailMeta{}, err
	}
	args := []string{"item", "get", "--format", "json"}
	if cleanVault != "" {
		args = append(args, "--vault", cleanVault)
	}
	args = append(args, "--", cleanItem)
	out, err := runOPJSON(ctx, tokenPath, args...)
	if err != nil {
		return OPItemDetailMeta{}, err
	}
	return parseOPItemDetailMeta(out)
}

// StoreOPItem creates a new item in a scoped 1Password vault and returns metadata only.
func StoreOPItem(ctx context.Context, tokenPath, vault, title, category, itemURL string, fields map[string]string) (OPItemDetailMeta, error) {
	cleanVault, err := validateOPPositionalArg(vault, "vault")
	if err != nil {
		return OPItemDetailMeta{}, err
	}
	cleanTitle, err := validateOPPositionalArg(title, "title")
	if err != nil {
		return OPItemDetailMeta{}, err
	}
	if cleanVault == "" || cleanTitle == "" {
		return OPItemDetailMeta{}, newErr("vault and title are required", http.StatusBadRequest)
	}
	cleanCat, err := validateOPPositionalArg(category, "category")
	if err != nil {
		return OPItemDetailMeta{}, err
	}
	cleanURL, err := validateOPPositionalArg(itemURL, "url")
	if err != nil {
		return OPItemDetailMeta{}, err
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		ck, kErr := validateOPPositionalArg(k, "field key")
		if kErr != nil {
			return OPItemDetailMeta{}, kErr
		}
		if ck != "" {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)

	type opItemFieldTemplate struct {
		Label string `json:"label"`
		Type  string `json:"type"`
		Value string `json:"value"`
	}
	tmplFields := make([]opItemFieldTemplate, 0, len(keys))
	for _, k := range keys {
		tmplFields = append(tmplFields, opItemFieldTemplate{
			Label: strings.TrimSpace(k),
			Type:  "CONCEALED",
			Value: fields[k],
		})
	}
	tmpl := map[string]any{
		"title":    cleanTitle,
		"category": cmp.Or(cleanCat, "API Credential"),
		"fields":   tmplFields,
	}
	if cleanURL != "" {
		tmpl["urls"] = []map[string]any{{"href": cleanURL, "primary": true}}
	}
	stdinJSON, err := json.Marshal(tmpl)
	if err != nil {
		return OPItemDetailMeta{}, err
	}
	args := []string{
		"item", "create",
		"--vault", cleanVault,
		"--category", cmp.Or(cleanCat, "API Credential"),
		"--title", cleanTitle,
		"--format", "json",
	}
	if cleanURL != "" {
		args = append(args, "--url", cleanURL)
	}
	args = append(args, "-")
	out, err := runOPJSONWithStdin(ctx, tokenPath, stdinJSON, args...)
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

func credentialHTTPClient(base *http.Client, initialURL *url.URL, allowedHosts []string, hdrName, label string) *http.Client {
	var cli http.Client
	if base != nil {
		cli = *base
	} else {
		cli.Timeout = 30 * time.Second
	}
	prevRedirect := cli.CheckRedirect
	initialHost := initialURL.Hostname()
	initialScheme := initialURL.Scheme
	cli.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		if req.URL.Scheme != "https" && req.URL.Scheme != "http" {
			return newErr("redirect to non-http/https scheme blocked", http.StatusForbidden)
		}
		if initialScheme == "https" && req.URL.Scheme != "https" {
			return newErr("redirect HTTPS to HTTP downgrade blocked", http.StatusForbidden)
		}
		if len(via) > 0 && via[len(via)-1].URL.Scheme == "https" && req.URL.Scheme != "https" {
			return newErr("redirect HTTPS to HTTP downgrade blocked", http.StatusForbidden)
		}
		if !hostAllowed(req.URL.Hostname(), allowedHosts) {
			return newErr(fmt.Sprintf("redirect host %q is not allowlisted for credential %q", req.URL.Hostname(), label), http.StatusForbidden)
		}
		if !strings.EqualFold(req.URL.Hostname(), initialHost) {
			req.Header.Del(hdrName)
			return newErr(fmt.Sprintf("cross-domain credential redirect from %q to %q blocked", initialHost, req.URL.Hostname()), http.StatusForbidden)
		}
		if prevRedirect != nil {
			return prevRedirect(req, via)
		}
		return nil
	}
	return &cli
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

	cli := credentialHTTPClient(httpClient, u, allowedHosts, hdrName, label)
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
