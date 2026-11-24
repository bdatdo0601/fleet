// Package ddns keeps the lighthouse's public DNS A record pointed at home via AWS Route 53.
package ddns

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os/exec"
	"sort"
	"strings"
	"time"
)

const TTL = 60

// CheckIPURLs lists public IP echo endpoints tried in order.
var CheckIPURLs = []string{
	"https://checkip.amazonaws.com",
	"https://api.ipify.org",
}

// DdnsError represents a DDNS lookup or Route 53 failure.
type DdnsError struct {
	Msg string
}

func (e *DdnsError) Error() string { return e.Msg }

// Route53 configures Route 53 A-record synchronization.
type Route53 struct {
	Name        string
	ZoneID      string
	Profile     string
	Nameservers []string
}

// FetchFn fetches a URL body as text.
type FetchFn func(url string) (string, error)

// DigFn executes dig-like queries returning answer lines.
type DigFn func(args ...string) ([]string, error)

// RunCmdFn executes an external command and returns stdout, stderr, and error.
type RunCmdFn func(name string, args ...string) (stdout, stderr string, err error)

var nonGlobalPrefixes = mustParsePrefixes([]string{
	"0.0.0.0/8",
	"100.64.0.0/10", // RFC 6598 CGNAT
	"192.0.0.0/24",
	"192.0.2.0/24",
	"198.18.0.0/15",
	"198.51.100.0/24",
	"203.0.113.0/24",
	"240.0.0.0/4",
})

func mustParsePrefixes(cidrs []string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(cidrs))
	for _, c := range cidrs {
		out = append(out, netip.MustParsePrefix(c))
	}
	return out
}

func isGlobalPublicIPv4(addr netip.Addr) bool {
	if !addr.Is4() {
		return false
	}
	if addr.IsUnspecified() || addr.IsLoopback() || addr.IsPrivate() ||
		addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() || addr.IsMulticast() {
		return false
	}
	for _, p := range nonGlobalPrefixes {
		if p.Contains(addr) {
			return false
		}
	}
	return true
}

func isASCIIAlnum(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')) {
			return false
		}
	}
	return true
}

// NormalizeZoneID strips any /hostedzone/ prefix and validates the Route 53 zone ID.
func NormalizeZoneID(zoneID string) (string, error) {
	trimmed := strings.TrimSpace(zoneID)
	if idx := strings.LastIndex(trimmed, "/"); idx >= 0 {
		trimmed = trimmed[idx+1:]
	}
	if !strings.HasPrefix(trimmed, "Z") || !isASCIIAlnum(trimmed) {
		return "", &DdnsError{Msg: fmt.Sprintf("%q doesn't look like a Route 53 hosted zone id (Z…)", zoneID)}
	}
	return trimmed, nil
}

// Policy builds the least-privilege IAM policy allowing UPSERT of only this A record.
func Policy(zoneID, name string) (map[string]any, error) {
	zid, err := NormalizeZoneID(zoneID)
	if err != nil {
		return nil, err
	}
	normName := strings.ToLower(strings.TrimRight(name, "."))
	return map[string]any{
		"Version": "2012-10-17",
		"Statement": []map[string]any{
			{
				"Effect":   "Allow",
				"Action":   "route53:ChangeResourceRecordSets",
				"Resource": fmt.Sprintf("arn:aws:route53:::hostedzone/%s", zid),
				"Condition": map[string]any{
					"ForAllValues:StringEquals": map[string]any{
						"route53:ChangeResourceRecordSetsNormalizedRecordNames": []string{normName},
						"route53:ChangeResourceRecordSetsRecordTypes":           []string{"A"},
						"route53:ChangeResourceRecordSetsActions":               []string{"UPSERT"},
					},
				},
			},
			{
				"Effect":   "Allow",
				"Action":   "route53:GetChange",
				"Resource": "arn:aws:route53:::change/*",
			},
		},
	}, nil
}

// DefaultFetch performs an HTTPS GET with a 10-second timeout.
func DefaultFetch(url string) (string, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// PublicIPv4 returns the host's internet-routable IPv4 address or errors on failure/CGNAT.
func PublicIPv4(fetch FetchFn) (string, error) {
	if fetch == nil {
		fetch = DefaultFetch
	}
	var errs []string
	for _, u := range CheckIPURLs {
		raw, err := fetch(u)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", u, err))
			continue
		}
		addr, err := netip.ParseAddr(strings.TrimSpace(raw))
		if err != nil || !addr.Is4() {
			errs = append(errs, fmt.Sprintf("%s: invalid IPv4 %q", u, strings.TrimSpace(raw)))
			continue
		}
		if !isGlobalPublicIPv4(addr) {
			return "", &DdnsError{Msg: fmt.Sprintf("public address %s is not internet-routable (CGNAT?); the lighthouse can't be reached", addr)}
		}
		return addr.String(), nil
	}
	return "", &DdnsError{Msg: "couldn't determine the public IP: " + strings.Join(errs, "; ")}
}

// DefaultDig executes `dig +short +time=3 +tries=2 <args...>`.
func DefaultDig(args ...string) ([]string, error) {
	bin, err := exec.LookPath("dig")
	if err != nil {
		return nil, &DdnsError{Msg: "dig not found"}
	}
	cmdArgs := append([]string{"+short", "+time=3", "+tries=2"}, args...)
	cmd := exec.Command(bin, cmdArgs...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stdout.String() + stderr.String())
		return nil, &DdnsError{Msg: fmt.Sprintf("dig %s failed: %s", strings.Join(args, " "), detail)}
	}
	var lines []string
	for _, ln := range strings.Split(stdout.String(), "\n") {
		ln = strings.TrimSpace(ln)
		if ln != "" && !strings.HasPrefix(ln, ";") {
			lines = append(lines, ln)
		}
	}
	return lines, nil
}

// FindNameservers walks up the DNS labels to locate authoritative nameservers for name.
func FindNameservers(name string, digFn DigFn) ([]string, error) {
	if digFn == nil {
		digFn = DefaultDig
	}
	labels := strings.Split(strings.TrimRight(name, "."), ".")
	for i := 0; i < len(labels)-1; i++ {
		ns, err := digFn(strings.Join(labels[i:], "."), "NS")
		if err != nil {
			return nil, err
		}
		if len(ns) > 0 {
			cleaned := make([]string, len(ns))
			for j, n := range ns {
				cleaned[j] = strings.TrimRight(n, ".")
			}
			sort.Strings(cleaned)
			return cleaned, nil
		}
	}
	return nil, &DdnsError{Msg: fmt.Sprintf("no nameservers found for %s", name)}
}

// CurrentRecord queries cfg.Nameservers directly for cfg.Name's A record.
func CurrentRecord(cfg Route53, digFn DigFn) (*string, error) {
	if digFn == nil {
		digFn = DefaultDig
	}
	var errs []string
	for _, ns := range cfg.Nameservers {
		answers, err := digFn(cfg.Name, "A", "@"+ns)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		for _, a := range answers {
			addr, err := netip.ParseAddr(a)
			if err == nil && addr.Is4() {
				s := addr.String()
				return &s, nil
			}
		}
		return nil, nil
	}
	return nil, &DdnsError{Msg: "no authoritative nameserver answered: " + strings.Join(errs, "; ")}
}

// ChangeBatch constructs the Route 53 change-batch payload.
func ChangeBatch(name, ip string) map[string]any {
	return map[string]any{
		"Comment": "fleet ddns",
		"Changes": []map[string]any{
			{
				"Action": "UPSERT",
				"ResourceRecordSet": map[string]any{
					"Name":            name,
					"Type":            "A",
					"TTL":             TTL,
					"ResourceRecords": []map[string]any{{"Value": ip}},
				},
			},
		},
	}
}

func defaultRunCmd(name string, args ...string) (string, string, error) {
	cmd := exec.Command(name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// Upsert applies an A record UPSERT via the AWS CLI.
func Upsert(cfg Route53, ip string, awsBin string, runFn RunCmdFn) (string, error) {
	if awsBin == "" {
		found, err := exec.LookPath("aws")
		if err != nil {
			return "", &DdnsError{Msg: "aws CLI not found (brew install awscli)"}
		}
		awsBin = found
	}
	if runFn == nil {
		runFn = defaultRunCmd
	}
	batchJSON, err := json.Marshal(ChangeBatch(cfg.Name, ip))
	if err != nil {
		return "", err
	}
	stdout, stderr, err := runFn(
		awsBin,
		"route53", "change-resource-record-sets",
		"--profile", cfg.Profile,
		"--hosted-zone-id", cfg.ZoneID,
		"--change-batch", string(batchJSON),
		"--output", "json",
	)
	if err != nil {
		detail := strings.TrimSpace(stderr)
		if detail == "" {
			detail = strings.TrimSpace(stdout)
		}
		if detail == "" {
			detail = err.Error()
		}
		return "", &DdnsError{Msg: "Route 53 update failed: " + detail}
	}
	var parsed struct {
		ChangeInfo struct {
			ID string `json:"Id"`
		} `json:"ChangeInfo"`
	}
	if err := json.Unmarshal([]byte(stdout), &parsed); err != nil {
		return "", err
	}
	return parsed.ChangeInfo.ID, nil
}

// Sync checks public IPv4 against authoritative DNS and updates Route 53 if changed.
func Sync(cfg Route53, fetch FetchFn, digFn DigFn, awsBin string, runFn RunCmdFn) (*string, string, bool, error) {
	ip, err := PublicIPv4(fetch)
	if err != nil {
		return nil, "", false, err
	}
	before, err := CurrentRecord(cfg, digFn)
	if err != nil {
		return nil, "", false, err
	}
	if before != nil && *before == ip {
		return before, ip, false, nil
	}
	if _, err := Upsert(cfg, ip, awsBin, runFn); err != nil {
		return nil, "", false, err
	}
	return before, ip, true, nil
}
