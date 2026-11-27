package tlsdns

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os/exec"
	"strings"
	"time"

	"github.com/bdatdo0601/fleet/internal/ddns"
)

const (
	staticATTL   = 300
	challengeTTL = 60
)

// PolicyOpts configures least-privilege Route 53 IAM policy generation.
type PolicyOpts struct {
	ZoneID         string
	Domain         string
	DDNSName       string
	IncludePublicA bool
}

// Policy builds a least-privilege IAM policy for ACME DNS-01 TXT challenges and optional A records.
func Policy(opts PolicyOpts) (map[string]any, error) {
	zid, err := ddns.NormalizeZoneID(opts.ZoneID)
	if err != nil {
		return nil, err
	}
	domain, err := NormalizeDomain(opts.Domain)
	if err != nil {
		return nil, err
	}
	zoneARN := fmt.Sprintf("arn:aws:route53:::hostedzone/%s", zid)
	stmts := []map[string]any{
		{
			"Effect":   "Allow",
			"Action":   "route53:ChangeResourceRecordSets",
			"Resource": zoneARN,
			"Condition": map[string]any{
				"ForAllValues:StringEquals": map[string]any{
					"route53:ChangeResourceRecordSetsNormalizedRecordNames": []string{"_acme-challenge." + domain},
					"route53:ChangeResourceRecordSetsRecordTypes":           []string{"TXT"},
					"route53:ChangeResourceRecordSetsActions":               []string{"UPSERT", "DELETE"},
				},
			},
		},
	}

	var aNames []string
	if opts.IncludePublicA {
		aNames = append(aNames, domain)
	}
	if ddnsName := strings.ToLower(strings.TrimRight(strings.TrimSpace(opts.DDNSName), ".")); ddnsName != "" && ddnsName != domain {
		aNames = append(aNames, ddnsName)
	}
	if len(aNames) > 0 {
		stmts = append(stmts, map[string]any{
			"Effect":   "Allow",
			"Action":   "route53:ChangeResourceRecordSets",
			"Resource": zoneARN,
			"Condition": map[string]any{
				"ForAllValues:StringEquals": map[string]any{
					"route53:ChangeResourceRecordSetsNormalizedRecordNames": aNames,
					"route53:ChangeResourceRecordSetsRecordTypes":           []string{"A"},
					"route53:ChangeResourceRecordSetsActions":               []string{"UPSERT"},
				},
			},
		})
	}

	stmts = append(stmts, map[string]any{
		"Effect":   "Allow",
		"Action":   "route53:GetChange",
		"Resource": "arn:aws:route53:::change/*",
	})
	return map[string]any{
		"Version":   "2012-10-17",
		"Statement": stmts,
	}, nil
}

func defaultRunCmd(name string, args ...string) (string, string, error) {
	cmd := exec.Command(name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

func resolveAWSBin(awsBin string) (string, error) {
	if awsBin != "" {
		return awsBin, nil
	}
	found, err := exec.LookPath("aws")
	if err != nil {
		return "", errors.New("aws CLI not found (brew install awscli)")
	}
	return found, nil
}

func runRoute53Change(cfg Config, batch map[string]any, awsBin string, runFn ddns.RunCmdFn) (string, error) {
	bin, err := resolveAWSBin(awsBin)
	if err != nil {
		return "", err
	}
	if runFn == nil {
		runFn = defaultRunCmd
	}
	batchJSON, err := json.Marshal(batch)
	if err != nil {
		return "", err
	}
	stdout, stderr, err := runFn(
		bin,
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
		return "", fmt.Errorf("route 53 update failed: %s", detail)
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

// UpsertStaticA upserts an A record for cfg.Domain pointing to ip (typically 10.213.0.1).
func UpsertStaticA(cfg Config, ip, awsBin string, runFn ddns.RunCmdFn) (string, error) {
	domain, err := NormalizeDomain(cfg.Domain)
	if err != nil {
		return "", err
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(ip))
	if err != nil || !addr.Is4() {
		return "", fmt.Errorf("invalid IPv4 address %q", ip)
	}
	batch := map[string]any{
		"Comment": "fleet static mesh A record",
		"Changes": []map[string]any{
			{
				"Action": "UPSERT",
				"ResourceRecordSet": map[string]any{
					"Name":            domain,
					"Type":            "A",
					"TTL":             staticATTL,
					"ResourceRecords": []map[string]any{{"Value": addr.String()}},
				},
			},
		},
	}
	return runRoute53Change(cfg, batch, awsBin, runFn)
}

// CheckStaticA queries cfg.Nameservers for cfg.Domain's public A record.
func CheckStaticA(cfg Config, digFn ddns.DigFn) (*string, error) {
	if len(cfg.Nameservers) == 0 {
		return nil, errors.New("no authoritative nameservers configured")
	}
	return ddns.CurrentRecord(ddns.Route53{
		Name:        cfg.Domain,
		ZoneID:      cfg.ZoneID,
		Profile:     cfg.Profile,
		Nameservers: cfg.Nameservers,
	}, digFn)
}

// ChangeTXT upserts or deletes a Route 53 TXT record for ACME DNS-01 challenges.
func ChangeTXT(cfg Config, fqdn, action, value, awsBin string, runFn ddns.RunCmdFn) (string, error) {
	name := strings.ToLower(strings.TrimRight(strings.TrimSpace(fqdn), "."))
	if name == "" {
		return "", errors.New("TXT record name is required")
	}
	act := strings.ToUpper(strings.TrimSpace(action))
	if act != "UPSERT" && act != "DELETE" {
		return "", fmt.Errorf("invalid Route 53 TXT action %q", action)
	}
	quoted := `"` + strings.ReplaceAll(value, `"`, `\"`) + `"`
	batch := map[string]any{
		"Comment": "fleet acme dns-01",
		"Changes": []map[string]any{
			{
				"Action": act,
				"ResourceRecordSet": map[string]any{
					"Name":            name,
					"Type":            "TXT",
					"TTL":             challengeTTL,
					"ResourceRecords": []map[string]any{{"Value": quoted}},
				},
			},
		},
	}
	return runRoute53Change(cfg, batch, awsBin, runFn)
}

// WaitChangeInSync polls Route 53 until changeID reaches INSYNC or ctx expires.
func WaitChangeInSync(
	ctx context.Context,
	profile, changeID, awsBin string,
	runFn ddns.RunCmdFn,
	pollInterval time.Duration,
) error {
	if changeID == "" {
		return nil
	}
	bin, err := resolveAWSBin(awsBin)
	if err != nil {
		return err
	}
	if runFn == nil {
		runFn = defaultRunCmd
	}
	if pollInterval <= 0 {
		pollInterval = 2 * time.Second
	}
	for {
		stdout, stderr, err := runFn(bin, "route53", "get-change", "--profile", profile, "--id", changeID, "--output", "json")
		if err == nil {
			var parsed struct {
				ChangeInfo struct {
					Status string `json:"Status"`
				} `json:"ChangeInfo"`
			}
			if jErr := json.Unmarshal([]byte(stdout), &parsed); jErr == nil && strings.EqualFold(parsed.ChangeInfo.Status, "INSYNC") {
				return nil
			}
		} else if ctx.Err() != nil {
			return fmt.Errorf("route 53 get-change %s failed: %w (%s)", changeID, err, strings.TrimSpace(stderr))
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for Route 53 change %s to sync: %w", changeID, ctx.Err())
		case <-time.After(pollInterval):
		}
	}
}

func cleanTXTRecord(raw string) string {
	s := strings.TrimSpace(raw)
	if strings.HasPrefix(s, `"`) && strings.HasSuffix(s, `"`) && len(s) >= 2 {
		s = s[1 : len(s)-1]
	}
	// Route 53 or dig may split long TXT strings across `"part1" "part2"`.
	s = strings.ReplaceAll(s, `" "`, "")
	return s
}

// WaitTXTPropagated polls authoritative nameservers until fqdn returns want on all reachable NS.
func WaitTXTPropagated(
	ctx context.Context,
	fqdn, want string,
	nameservers []string,
	digFn ddns.DigFn,
	pollInterval time.Duration,
) error {
	if len(nameservers) == 0 {
		return nil
	}
	if digFn == nil {
		digFn = ddns.DefaultDig
	}
	if pollInterval <= 0 {
		pollInterval = 2 * time.Second
	}
	target := strings.TrimRight(fqdn, ".")
	for {
		answered := 0
		allMatched := true
		for _, ns := range nameservers {
			lines, err := digFn(target, "TXT", "@"+ns)
			if err != nil {
				continue
			}
			answered++
			found := false
			for _, ln := range lines {
				if cleanTXTRecord(ln) == want {
					found = true
					break
				}
			}
			if !found {
				allMatched = false
				break
			}
		}
		if answered > 0 && allMatched {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for DNS-01 TXT %s propagation: %w", target, ctx.Err())
		case <-time.After(pollInterval):
		}
	}
}
