package assistant

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bdatdo0601/fleet/internal/registry"
)

const (
	// DefaultSkillRegistryURL is the default open agent skills search registry.
	DefaultSkillRegistryURL = "https://skills.sh"
	// DefaultGitHubAPIBaseURL is the default GitHub REST API base URL.
	DefaultGitHubAPIBaseURL = "https://api.github.com"
	// DefaultGitHubRawBaseURL is the default GitHub raw content base URL.
	DefaultGitHubRawBaseURL = "https://raw.githubusercontent.com"

	maxSkillBundleFiles = 32
	maxSkillFileBytes   = 256 * 1024
	maxSkillBundleBytes = 1024 * 1024
)

var (
	ghOwnerRepoRE = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	ghCommitSHARE = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)
)

// SkillSearchResult describes one match returned by skills.sh search.
type SkillSearchResult struct {
	ID          string `json:"id"`
	Source      string `json:"source"`
	SkillID     string `json:"skill_id"`
	Name        string `json:"name"`
	Installs    int    `json:"installs"`
	InstallSpec string `json:"install_spec"`
	URL         string `json:"url"`
}

// SkillProvenance records upstream source metadata for an installed skill bundle.
type SkillProvenance struct {
	Source      string `json:"source"`
	SkillID     string `json:"skill_id,omitempty"`
	Repo        string `json:"repo,omitempty"`
	Subpath     string `json:"subpath,omitempty"`
	Ref         string `json:"ref,omitempty"`
	CommitSHA   string `json:"commit_sha,omitempty"`
	URL         string `json:"url,omitempty"`
	InstalledAt string `json:"installed_at,omitempty"`
}

// FetchedSkillBundle holds a validated, sanitized skill bundle fetched from a remote source.
type FetchedSkillBundle struct {
	Slug                string            `json:"slug"`
	Name                string            `json:"name"`
	Description         string            `json:"description"`
	Content             string            `json:"content"`
	ExtraFiles          map[string][]byte `json:"-"`
	Files               []string          `json:"files,omitempty"`
	SkippedFiles        []string          `json:"skipped_files,omitempty"`
	HasScripts          bool              `json:"has_scripts,omitempty"`
	SyncToFleetStripped bool              `json:"sync_to_fleet_stripped,omitempty"`
	Provenance          SkillProvenance   `json:"provenance"`
}

// ParsedSkillSource represents a normalized skill source specifier.
type ParsedSkillSource struct {
	Kind      string // "github" or "url"
	Raw       string
	Owner     string
	Repo      string
	SkillID   string
	Subpath   string
	Ref       string
	DirectURL string
}

// SkillRegistry searches and fetches SKILL.md bundles from skills.sh, GitHub, and HTTPS URLs.
type SkillRegistry struct {
	RegistryBaseURL  string
	GitHubAPIBaseURL string
	GitHubRawBaseURL string
	HTTPClient       *http.Client
	AllowPrivateURLs bool
}

// NewSkillRegistry constructs a SkillRegistry using SSRF-hardened public-only HTTPS transport.
func NewSkillRegistry() *SkillRegistry {
	return &SkillRegistry{
		RegistryBaseURL:  DefaultSkillRegistryURL,
		GitHubAPIBaseURL: DefaultGitHubAPIBaseURL,
		GitHubRawBaseURL: DefaultGitHubRawBaseURL,
		HTTPClient:       newSSRFSafeSkillHTTPClient(),
	}
}

func newSSRFSafeSkillHTTPClient() *http.Client {
	tr := &http.Transport{
		DialContext:           dialPublicOnlyTCP,
		ForceAttemptHTTP2:     true,
		ResponseHeaderTimeout: 15 * time.Second,
		IdleConnTimeout:       15 * time.Second,
	}
	return &http.Client{
		Timeout:   20 * time.Second,
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if !strings.EqualFold(req.URL.Scheme, "https") {
				return errors.New("redirect to non-https URL blocked")
			}
			return nil
		},
	}
}

func dialPublicOnlyTCP(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("invalid address %q", addr)
	}
	if port != "80" && port != "443" {
		return nil, fmt.Errorf("port %s blocked for skill fetch", port)
	}
	cleanHost := strings.Trim(strings.TrimSpace(host), "[]")
	var ips []netip.Addr
	if literal, parseErr := netip.ParseAddr(cleanHost); parseErr == nil {
		ips = []netip.Addr{literal}
	} else {
		ips, err = net.DefaultResolver.LookupNetIP(ctx, "ip", cleanHost)
		if err != nil {
			return nil, fmt.Errorf("dns lookup failed for %q: %w", cleanHost, err)
		}
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("no IP addresses resolved for %q", cleanHost)
	}
	for _, ip := range ips {
		if IsBlockedEgressAddr(ip) {
			return nil, fmt.Errorf("egress to %q (%s) blocked: private or internal IP", cleanHost, ip)
		}
	}
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].Unmap().String(), port))
}

func (r *SkillRegistry) httpClient() *http.Client {
	if r != nil && r.HTTPClient != nil {
		return r.HTTPClient
	}
	if r != nil && r.AllowPrivateURLs {
		return &http.Client{Timeout: 20 * time.Second}
	}
	return newSSRFSafeSkillHTTPClient()
}

func (r *SkillRegistry) baseURLs() (regBase, apiBase, rawBase string) {
	regBase = DefaultSkillRegistryURL
	apiBase = DefaultGitHubAPIBaseURL
	rawBase = DefaultGitHubRawBaseURL
	if r != nil {
		regBase = cmp.Or(strings.TrimRight(strings.TrimSpace(r.RegistryBaseURL), "/"), regBase)
		apiBase = cmp.Or(strings.TrimRight(strings.TrimSpace(r.GitHubAPIBaseURL), "/"), apiBase)
		rawBase = cmp.Or(strings.TrimRight(strings.TrimSpace(r.GitHubRawBaseURL), "/"), rawBase)
	}
	return regBase, apiBase, rawBase
}

// NormalizeSkillSlug normalizes an arbitrary skill name into a valid _skills/<slug> directory name.
func NormalizeSkillSlug(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" {
		return ""
	}
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_':
			b.WriteRune(r)
			lastDash = false
		case r == '-':
			if b.Len() > 0 && !lastDash {
				b.WriteRune('-')
				lastDash = true
			}
		default:
			if b.Len() > 0 && !lastDash {
				b.WriteRune('-')
				lastDash = true
			}
		}
	}
	clean := strings.Trim(b.String(), "-_")
	if len(clean) > 64 {
		clean = strings.Trim(clean[:64], "-_")
	}
	if !skillSlugRE.MatchString(clean) {
		return ""
	}
	return clean
}

// SanitizeExternalSkillContent strips truthy sync_to_fleet flags from external SKILL.md frontmatter.
func SanitizeExternalSkillContent(content string) (string, bool) {
	trimmed := strings.TrimLeft(content, "\ufeff \t\r\n")
	if !strings.HasPrefix(trimmed, "---") {
		return content, false
	}
	lines := strings.Split(content, "\n")
	startIdx := -1
	endIdx := -1
	for i, ln := range lines {
		if strings.TrimSpace(ln) == "---" {
			if startIdx < 0 {
				startIdx = i
			} else {
				endIdx = i
				break
			}
		}
	}
	if startIdx < 0 || endIdx <= startIdx {
		return content, false
	}
	stripped := false
	out := make([]string, 0, len(lines))
	for i, ln := range lines {
		if i > startIdx && i < endIdx {
			k, val, ok := strings.Cut(strings.TrimSpace(ln), ":")
			if ok && strings.EqualFold(strings.TrimSpace(k), "sync_to_fleet") {
				cleanVal := strings.ToLower(strings.Trim(strings.TrimSpace(val), `"'`))
				if cleanVal == "true" || cleanVal == "yes" || cleanVal == "1" {
					stripped = true
					continue
				}
			}
		}
		out = append(out, ln)
	}
	if !stripped {
		return content, false
	}
	return strings.Join(out, "\n"), true
}

// ParseSkillSource parses skills.sh specs, GitHub shorthand/URLs, or direct HTTPS SKILL.md URLs.
func ParseSkillSource(rawSource, explicitSkill, explicitRef string) (ParsedSkillSource, error) {
	raw := strings.TrimSpace(rawSource)
	if raw == "" {
		return ParsedSkillSource{}, newErr("skill source is required", http.StatusBadRequest)
	}
	if strings.ContainsRune(raw, 0) || strings.ContainsAny(raw, " \t\r\n\\") {
		return ParsedSkillSource{}, newErr("invalid characters in skill source", http.StatusBadRequest)
	}
	expSkill := strings.TrimSpace(explicitSkill)
	expRef := strings.TrimSpace(explicitRef)

	if strings.Contains(raw, "://") {
		return parseSkillURLSource(raw, expSkill, expRef)
	}
	if strings.HasPrefix(raw, "skills.sh/") || strings.HasPrefix(raw, "www.skills.sh/") {
		return parseSkillURLSource("https://"+raw, expSkill, expRef)
	}
	if strings.HasPrefix(raw, "github.com/") || strings.HasPrefix(raw, "www.github.com/") {
		return parseSkillURLSource("https://"+raw, expSkill, expRef)
	}
	return parseSkillShorthand(raw, expSkill, expRef)
}

func parseSkillURLSource(rawURL, expSkill, expRef string) (ParsedSkillSource, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return ParsedSkillSource{}, newErr("invalid skill source URL", http.StatusBadRequest)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" && scheme != "http" {
		return ParsedSkillSource{}, newErr(fmt.Sprintf("unsupported skill URL scheme %q", u.Scheme), http.StatusBadRequest)
	}
	host := strings.ToLower(u.Hostname())
	segs := splitCleanPath(u.Path)
	if slices.Contains(segs, "..") {
		return ParsedSkillSource{}, newErr("invalid path traversal in skill URL", http.StatusBadRequest)
	}

	switch host {
	case "skills.sh", "www.skills.sh":
		if len(segs) < 2 {
			return ParsedSkillSource{}, newErr("skills.sh URL must include owner/repo", http.StatusBadRequest)
		}
		if err := validateOwnerRepo(segs[0], segs[1]); err != nil {
			return ParsedSkillSource{}, err
		}
		skillID := expSkill
		if skillID == "" && len(segs) >= 3 {
			skillID = segs[2]
		}
		return ParsedSkillSource{
			Kind:    "github",
			Raw:     rawURL,
			Owner:   segs[0],
			Repo:    segs[1],
			SkillID: skillID,
			Ref:     expRef,
		}, nil

	case "github.com", "www.github.com":
		if len(segs) < 2 {
			return ParsedSkillSource{}, newErr("GitHub URL must include owner/repo", http.StatusBadRequest)
		}
		repo := strings.TrimSuffix(segs[1], ".git")
		if err := validateOwnerRepo(segs[0], repo); err != nil {
			return ParsedSkillSource{}, err
		}
		ref := expRef
		subpath := ""
		if len(segs) >= 4 && (segs[2] == "tree" || segs[2] == "blob") {
			ref = cmp.Or(expRef, segs[3])
			if len(segs) > 4 {
				subpath = trimSkillMDParent(strings.Join(segs[4:], "/"))
			}
		} else if len(segs) > 2 {
			subpath = trimSkillMDParent(strings.Join(segs[2:], "/"))
		}
		return ParsedSkillSource{
			Kind:    "github",
			Raw:     rawURL,
			Owner:   segs[0],
			Repo:    repo,
			SkillID: expSkill,
			Subpath: subpath,
			Ref:     ref,
		}, nil

	default:
		return ParsedSkillSource{
			Kind:      "url",
			Raw:       rawURL,
			SkillID:   expSkill,
			Ref:       expRef,
			DirectURL: u.String(),
		}, nil
	}
}

func parseSkillShorthand(raw, expSkill, expRef string) (ParsedSkillSource, error) {
	if strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, ".") {
		return ParsedSkillSource{}, newErr("skill source must be owner/repo[@skill] or an https:// URL", http.StatusBadRequest)
	}
	repoPart, atSkill, hasAt := strings.Cut(raw, "@")
	if hasAt && strings.TrimSpace(atSkill) == "" {
		return ParsedSkillSource{}, newErr("empty skill name after @", http.StatusBadRequest)
	}
	segs := splitCleanPath(repoPart)
	if len(segs) < 2 || slices.Contains(segs, "..") {
		return ParsedSkillSource{}, newErr("skill shorthand must be owner/repo or owner/repo@skill", http.StatusBadRequest)
	}
	repo := strings.TrimSuffix(segs[1], ".git")
	if err := validateOwnerRepo(segs[0], repo); err != nil {
		return ParsedSkillSource{}, err
	}
	subpath := ""
	if len(segs) > 2 {
		subpath = trimSkillMDParent(strings.Join(segs[2:], "/"))
	}
	return ParsedSkillSource{
		Kind:    "github",
		Raw:     raw,
		Owner:   segs[0],
		Repo:    repo,
		SkillID: cmp.Or(expSkill, strings.TrimSpace(atSkill)),
		Subpath: subpath,
		Ref:     expRef,
	}, nil
}

func validateOwnerRepo(owner, repo string) error {
	if owner == "" || repo == "" || owner == "." || owner == ".." || repo == "." || repo == ".." {
		return newErr("invalid GitHub owner/repo", http.StatusBadRequest)
	}
	if !ghOwnerRepoRE.MatchString(owner) || !ghOwnerRepoRE.MatchString(repo) {
		return newErr("invalid GitHub owner/repo characters", http.StatusBadRequest)
	}
	return nil
}

func splitCleanPath(p string) []string {
	var out []string
	for seg := range strings.SplitSeq(p, "/") {
		if seg == "" || seg == "." {
			continue
		}
		out = append(out, seg)
	}
	return out
}

func trimSkillMDParent(sub string) string {
	sub = strings.Trim(sub, "/")
	if sub == "SKILL.md" {
		return ""
	}
	return strings.TrimSuffix(sub, "/SKILL.md")
}

// Search queries skills.sh for matching agent skills.
func (r *SkillRegistry) Search(ctx context.Context, query string, limit int) ([]SkillSearchResult, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return nil, newErr("search query is required", http.StatusBadRequest)
	}
	if limit <= 0 {
		limit = 8
	} else if limit > 20 {
		limit = 20
	}
	regBase, _, _ := r.baseURLs()
	if err := r.validateOutboundURL(regBase); err != nil {
		return nil, err
	}
	endpoint := fmt.Sprintf("%s/api/search?q=%s&limit=%d", regBase, url.QueryEscape(q), limit)
	body, status, err := r.doGET(ctx, endpoint, "application/json", maxSkillFileBytes)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, newErr(fmt.Sprintf("skill registry search failed (%d): %s", status, truncateText(string(body), 200)), http.StatusBadGateway)
	}
	var resp struct {
		Skills []struct {
			ID       string `json:"id"`
			Source   string `json:"source"`
			SkillID  string `json:"skillId"`
			Name     string `json:"name"`
			Installs int    `json:"installs"`
		} `json:"skills"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parse skill registry search response: %w", err)
	}
	out := make([]SkillSearchResult, 0, len(resp.Skills))
	for _, it := range resp.Skills {
		installSpec := it.Source
		if it.Source != "" && it.SkillID != "" {
			installSpec = it.Source + "@" + it.SkillID
		}
		skillURL := ""
		if it.Source != "" && it.SkillID != "" {
			skillURL = regBase + "/" + it.Source + "/" + it.SkillID
		}
		out = append(out, SkillSearchResult{
			ID:          it.ID,
			Source:      it.Source,
			SkillID:     it.SkillID,
			Name:        cmp.Or(it.Name, it.SkillID),
			Installs:    it.Installs,
			InstallSpec: installSpec,
			URL:         skillURL,
		})
	}
	return out, nil
}

// Fetch downloads and validates a skill bundle without writing it to the vault.
func (r *SkillRegistry) Fetch(ctx context.Context, rawSource, explicitSkill, explicitRef, slugOverride string) (*FetchedSkillBundle, error) {
	parsed, err := ParseSkillSource(rawSource, explicitSkill, explicitRef)
	if err != nil {
		return nil, err
	}
	if parsed.Kind == "url" {
		return r.fetchDirectURL(ctx, parsed, slugOverride)
	}
	return r.fetchGitHub(ctx, parsed, slugOverride)
}

func (r *SkillRegistry) validateOutboundURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return newErr("invalid URL", http.StatusBadRequest)
	}
	if r != nil && r.AllowPrivateURLs {
		if u.Scheme != "https" && u.Scheme != "http" {
			return newErr("only http/https URLs are supported", http.StatusBadRequest)
		}
		return nil
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return newErr("only https:// URLs are allowed for remote skill fetches", http.StatusBadRequest)
	}
	if literal, parseErr := netip.ParseAddr(strings.Trim(u.Hostname(), "[]")); parseErr == nil && IsBlockedEgressAddr(literal) {
		return newErr("private or internal IP addresses are not allowed", http.StatusForbidden)
	}
	return nil
}

func (r *SkillRegistry) doGET(ctx context.Context, endpoint, accept string, limit int64) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", "fleet-assistant")
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := r.httpClient().Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if int64(len(data)) > limit {
		return nil, resp.StatusCode, newErr(fmt.Sprintf("remote file exceeds %d byte limit", limit), http.StatusRequestEntityTooLarge)
	}
	return data, resp.StatusCode, nil
}

func isSafeUTF8Text(b []byte) bool {
	return !slices.Contains(b, 0) && utf8.Valid(b)
}

func (r *SkillRegistry) fetchDirectURL(ctx context.Context, parsed ParsedSkillSource, slugOverride string) (*FetchedSkillBundle, error) {
	if err := r.validateOutboundURL(parsed.DirectURL); err != nil {
		return nil, err
	}
	body, status, err := r.doGET(ctx, parsed.DirectURL, "text/markdown, text/plain, */*", maxSkillFileBytes)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return nil, newErr("skill URL returned 404 not found", http.StatusNotFound)
	}
	if status != http.StatusOK {
		return nil, newErr(fmt.Sprintf("skill URL returned HTTP %d", status), http.StatusBadGateway)
	}
	if len(strings.TrimSpace(string(body))) == 0 || !isSafeUTF8Text(body) {
		return nil, newErr("fetched skill content is empty or binary", http.StatusBadRequest)
	}
	sanitized, stripped := SanitizeExternalSkillContent(string(body))
	u, _ := url.Parse(parsed.DirectURL)
	parentName := ""
	if u != nil {
		dir := path.Dir(u.Path)
		if dir != "" && dir != "/" && dir != "." {
			parentName = path.Base(dir)
		}
	}
	meta := ParseSkillFrontmatter(parentName, sanitized)
	slug := resolveFinalSkillSlug(slugOverride, parsed.SkillID, meta.Name, parentName)
	if slug == "" {
		return nil, newErr("could not derive valid skill slug; pass explicit slug", http.StatusBadRequest)
	}
	return &FetchedSkillBundle{
		Slug:                slug,
		Name:                cmp.Or(meta.Name, slug),
		Description:         meta.Description,
		Content:             sanitized,
		SyncToFleetStripped: stripped,
		Provenance: SkillProvenance{
			Source:      parsed.DirectURL,
			SkillID:     parsed.SkillID,
			URL:         parsed.DirectURL,
			InstalledAt: registry.NowISO(),
		},
	}, nil
}

type ghTreeEntry struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	Type string `json:"type"`
	Size int64  `json:"size"`
}

func (r *SkillRegistry) fetchGitHub(ctx context.Context, parsed ParsedSkillSource, slugOverride string) (*FetchedSkillBundle, error) {
	_, apiBase, rawBase := r.baseURLs()
	if err := r.validateOutboundURL(apiBase); err != nil {
		return nil, err
	}
	if err := r.validateOutboundURL(rawBase); err != nil {
		return nil, err
	}

	ref := cmp.Or(parsed.Ref, "HEAD")
	commitSHA := r.resolveGitHubCommitSHA(ctx, apiBase, parsed.Owner, parsed.Repo, ref)
	resolvedRef := cmp.Or(commitSHA, ref)

	entries, status, err := r.fetchGitHubTree(ctx, apiBase, parsed.Owner, parsed.Repo, resolvedRef)
	if err == nil && status == http.StatusOK {
		return r.buildGitHubBundleFromTree(ctx, rawBase, parsed, ref, resolvedRef, commitSHA, entries, slugOverride)
	}
	if status == http.StatusForbidden || status == http.StatusTooManyRequests {
		if fallback, ok := r.fetchGitHubRawFallback(ctx, rawBase, parsed, resolvedRef, ref, commitSHA, slugOverride); ok {
			return fallback, nil
		}
	}
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return nil, newErr(fmt.Sprintf("GitHub repository %s/%s (ref %s) not found", parsed.Owner, parsed.Repo, ref), http.StatusNotFound)
	}
	return nil, newErr(fmt.Sprintf("GitHub tree lookup failed (%d)", status), http.StatusBadGateway)
}

func (r *SkillRegistry) resolveGitHubCommitSHA(ctx context.Context, apiBase, owner, repo, ref string) string {
	if ghCommitSHARE.MatchString(ref) {
		return strings.ToLower(ref)
	}
	endpoint := fmt.Sprintf("%s/repos/%s/%s/commits/%s", apiBase, url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(ref))
	body, status, err := r.doGET(ctx, endpoint, "application/vnd.github.sha", 1024)
	if err != nil || status != http.StatusOK {
		return ""
	}
	sha := strings.TrimSpace(string(body))
	if ghCommitSHARE.MatchString(sha) {
		return strings.ToLower(sha)
	}
	return ""
}

func (r *SkillRegistry) fetchGitHubTree(ctx context.Context, apiBase, owner, repo, resolvedRef string) ([]ghTreeEntry, int, error) {
	endpoint := fmt.Sprintf("%s/repos/%s/%s/git/trees/%s?recursive=1", apiBase, url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(resolvedRef))
	body, status, err := r.doGET(ctx, endpoint, "application/vnd.github+json", 2*1024*1024)
	if err != nil || status != http.StatusOK {
		return nil, status, err
	}
	var treeResp struct {
		Tree []ghTreeEntry `json:"tree"`
	}
	if err := json.Unmarshal(body, &treeResp); err != nil {
		return nil, status, fmt.Errorf("parse GitHub tree response: %w", err)
	}
	return treeResp.Tree, status, nil
}

func (r *SkillRegistry) buildGitHubBundleFromTree(
	ctx context.Context,
	rawBase string,
	parsed ParsedSkillSource,
	ref, resolvedRef, commitSHA string,
	entries []ghTreeEntry,
	slugOverride string,
) (*FetchedSkillBundle, error) {
	skillMDPath, skillDir, err := selectGitHubSkillMD(entries, parsed)
	if err != nil {
		return nil, err
	}
	rawSkillURL := buildGitHubRawURL(rawBase, parsed.Owner, parsed.Repo, resolvedRef, skillMDPath)
	rawSkillMD, status, err := r.doGET(ctx, rawSkillURL, "text/plain", maxSkillFileBytes)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK || len(strings.TrimSpace(string(rawSkillMD))) == 0 || !isSafeUTF8Text(rawSkillMD) {
		return nil, newErr(fmt.Sprintf("failed to download %s (HTTP %d)", skillMDPath, status), http.StatusBadGateway)
	}
	sanitized, stripped := SanitizeExternalSkillContent(string(rawSkillMD))

	extraFiles, files, skipped, hasScripts := r.downloadGitHubCompanions(ctx, rawBase, parsed.Owner, parsed.Repo, resolvedRef, skillDir, len(rawSkillMD), entries)
	dirName := path.Base(skillDir)
	if skillDir == "" || dirName == "." {
		dirName = parsed.Repo
	}
	meta := ParseSkillFrontmatter(dirName, sanitized)
	slug := resolveFinalSkillSlug(slugOverride, parsed.SkillID, dirName, meta.Name, parsed.Repo)
	if slug == "" {
		return nil, newErr("could not derive valid skill slug", http.StatusBadRequest)
	}
	skillID := cmp.Or(parsed.SkillID, dirName)
	installSpec := parsed.Owner + "/" + parsed.Repo
	if skillID != "" && skillDir != "" {
		installSpec += "@" + skillID
	}
	ghURL := fmt.Sprintf("https://github.com/%s/%s", parsed.Owner, parsed.Repo)
	if skillDir != "" {
		ghURL = fmt.Sprintf("%s/tree/%s/%s", ghURL, cmp.Or(commitSHA, ref), skillDir)
	}
	return &FetchedSkillBundle{
		Slug:                slug,
		Name:                cmp.Or(meta.Name, slug),
		Description:         meta.Description,
		Content:             sanitized,
		ExtraFiles:          extraFiles,
		Files:               files,
		SkippedFiles:        skipped,
		HasScripts:          hasScripts,
		SyncToFleetStripped: stripped,
		Provenance: SkillProvenance{
			Source:      installSpec,
			SkillID:     skillID,
			Repo:        parsed.Owner + "/" + parsed.Repo,
			Subpath:     skillDir,
			Ref:         ref,
			CommitSHA:   commitSHA,
			URL:         ghURL,
			InstalledAt: registry.NowISO(),
		},
	}, nil
}

func (r *SkillRegistry) downloadGitHubCompanions(
	ctx context.Context,
	rawBase, owner, repo, resolvedRef, skillDir string,
	initialBytes int,
	entries []ghTreeEntry,
) (extra map[string][]byte, files, skipped []string, hasScripts bool) {
	extra = make(map[string][]byte)
	totalBytes := initialBytes
	prefix := ""
	if skillDir != "" {
		prefix = skillDir + "/"
	}
	for _, ent := range entries {
		if ent.Type != "blob" {
			continue
		}
		if prefix != "" && !strings.HasPrefix(ent.Path, prefix) {
			continue
		}
		rel := strings.TrimPrefix(ent.Path, prefix)
		if rel == "" || rel == "SKILL.md" {
			continue
		}
		if hasHiddenSegment(rel) || ent.Mode == "120000" {
			skipped = append(skipped, rel)
			continue
		}
		if len(files) >= maxSkillBundleFiles-1 || ent.Size > maxSkillFileBytes || totalBytes+int(ent.Size) > maxSkillBundleBytes {
			skipped = append(skipped, rel)
			continue
		}
		rawURL := buildGitHubRawURL(rawBase, owner, repo, resolvedRef, ent.Path)
		data, status, err := r.doGET(ctx, rawURL, "text/plain", maxSkillFileBytes)
		if err != nil || status != http.StatusOK || !isSafeUTF8Text(data) || totalBytes+len(data) > maxSkillBundleBytes {
			skipped = append(skipped, rel)
			continue
		}
		totalBytes += len(data)
		extra[rel] = data
		files = append(files, rel)
		if isScriptRelPath(rel) {
			hasScripts = true
		}
	}
	slices.Sort(files)
	slices.Sort(skipped)
	return extra, files, skipped, hasScripts
}

func (r *SkillRegistry) fetchGitHubRawFallback(
	ctx context.Context,
	rawBase string,
	parsed ParsedSkillSource,
	resolvedRef, ref, commitSHA, slugOverride string,
) (*FetchedSkillBundle, bool) {
	var candidates []string
	if parsed.Subpath != "" {
		candidates = append(candidates, parsed.Subpath+"/SKILL.md")
	}
	if parsed.SkillID != "" {
		s := parsed.SkillID
		candidates = append(candidates,
			"skills/"+s+"/SKILL.md",
			s+"/SKILL.md",
			".agents/skills/"+s+"/SKILL.md",
			".claude/skills/"+s+"/SKILL.md",
		)
	}
	if parsed.Subpath == "" && parsed.SkillID == "" {
		candidates = append(candidates, "SKILL.md")
	}
	for _, cand := range candidates {
		rawURL := buildGitHubRawURL(rawBase, parsed.Owner, parsed.Repo, resolvedRef, cand)
		data, status, err := r.doGET(ctx, rawURL, "text/plain", maxSkillFileBytes)
		if err != nil || status != http.StatusOK || len(strings.TrimSpace(string(data))) == 0 || !isSafeUTF8Text(data) {
			continue
		}
		sanitized, stripped := SanitizeExternalSkillContent(string(data))
		skillDir := trimSkillMDParent(cand)
		dirName := path.Base(skillDir)
		if skillDir == "" || dirName == "." {
			dirName = parsed.Repo
		}
		meta := ParseSkillFrontmatter(dirName, sanitized)
		slug := resolveFinalSkillSlug(slugOverride, parsed.SkillID, dirName, meta.Name, parsed.Repo)
		if slug == "" {
			continue
		}
		skillID := cmp.Or(parsed.SkillID, dirName)
		installSpec := parsed.Owner + "/" + parsed.Repo
		if skillID != "" && skillDir != "" {
			installSpec += "@" + skillID
		}
		return &FetchedSkillBundle{
			Slug:                slug,
			Name:                cmp.Or(meta.Name, slug),
			Description:         meta.Description,
			Content:             sanitized,
			SyncToFleetStripped: stripped,
			Provenance: SkillProvenance{
				Source:      installSpec,
				SkillID:     skillID,
				Repo:        parsed.Owner + "/" + parsed.Repo,
				Subpath:     skillDir,
				Ref:         ref,
				CommitSHA:   commitSHA,
				URL:         rawURL,
				InstalledAt: registry.NowISO(),
			},
		}, true
	}
	return nil, false
}

func selectGitHubSkillMD(entries []ghTreeEntry, parsed ParsedSkillSource) (skillMDPath, skillDir string, err error) {
	var skillFiles []string
	for _, ent := range entries {
		if ent.Type == "blob" && path.Base(ent.Path) == "SKILL.md" {
			skillFiles = append(skillFiles, ent.Path)
		}
	}
	slices.Sort(skillFiles)
	if len(skillFiles) == 0 {
		return "", "", newErr(fmt.Sprintf("no SKILL.md found in %s/%s", parsed.Owner, parsed.Repo), http.StatusNotFound)
	}
	if parsed.Subpath != "" {
		want := parsed.Subpath + "/SKILL.md"
		for _, p := range skillFiles {
			if p == want {
				return p, parsed.Subpath, nil
			}
		}
		return "", "", newErr(fmt.Sprintf("SKILL.md not found at subpath %q in %s/%s", parsed.Subpath, parsed.Owner, parsed.Repo), http.StatusNotFound)
	}
	if parsed.SkillID != "" {
		return matchSkillIDInPaths(skillFiles, parsed.Owner, parsed.Repo, parsed.SkillID)
	}
	if len(skillFiles) == 1 {
		p := skillFiles[0]
		return p, trimSkillMDParent(p), nil
	}
	var names []string
	for _, p := range skillFiles {
		dir := trimSkillMDParent(p)
		if dir == "" {
			names = append(names, parsed.Repo)
		} else {
			names = append(names, path.Base(dir))
		}
		if len(names) >= 20 {
			break
		}
	}
	return "", "", newErr(fmt.Sprintf("repository %s/%s contains multiple skills (%s); specify %s/%s@<skill>", parsed.Owner, parsed.Repo, strings.Join(names, ", "), parsed.Owner, parsed.Repo), http.StatusBadRequest)
}

func matchSkillIDInPaths(skillFiles []string, owner, repo, skillID string) (string, string, error) {
	preferred := []string{
		"skills/" + skillID + "/SKILL.md",
		skillID + "/SKILL.md",
		".agents/skills/" + skillID + "/SKILL.md",
		".claude/skills/" + skillID + "/SKILL.md",
	}
	for _, want := range preferred {
		for _, p := range skillFiles {
			if strings.EqualFold(p, want) {
				return p, trimSkillMDParent(p), nil
			}
		}
	}
	suffix := "/" + strings.ToLower(skillID) + "/skill.md"
	for _, p := range skillFiles {
		if strings.HasSuffix(strings.ToLower(p), suffix) {
			return p, trimSkillMDParent(p), nil
		}
	}
	if len(skillFiles) == 1 && skillFiles[0] == "SKILL.md" && strings.EqualFold(skillID, repo) {
		return "SKILL.md", "", nil
	}
	return "", "", newErr(fmt.Sprintf("skill %q not found in %s/%s", skillID, owner, repo), http.StatusNotFound)
}

func buildGitHubRawURL(rawBase, owner, repo, ref, filePath string) string {
	segs := strings.Split(filePath, "/")
	escaped := make([]string, 0, len(segs))
	for _, s := range segs {
		escaped = append(escaped, url.PathEscape(s))
	}
	return fmt.Sprintf("%s/%s/%s/%s/%s", rawBase, url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(ref), strings.Join(escaped, "/"))
}

func resolveFinalSkillSlug(candidates ...string) string {
	for _, c := range candidates {
		if slug := NormalizeSkillSlug(c); slug != "" {
			return slug
		}
	}
	return ""
}

func hasHiddenSegment(rel string) bool {
	for seg := range strings.SplitSeq(rel, "/") {
		if strings.HasPrefix(seg, ".") {
			return true
		}
	}
	return false
}

func isScriptRelPath(rel string) bool {
	low := strings.ToLower(rel)
	if strings.HasPrefix(low, "scripts/") {
		return true
	}
	switch path.Ext(low) {
	case ".sh", ".bash", ".zsh", ".py", ".js", ".ts", ".rb":
		return true
	default:
		return false
	}
}
