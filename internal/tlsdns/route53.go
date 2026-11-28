package tlsdns

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/bdatdo0601/fleet/internal/ddns"
)

const (
	staticATTL           = 300
	challengeTTL         = 60
	maxRoute53PollErrors = 5
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

// UpsertStaticA upserts an A record for cfg.Domain pointing to ip (typically 10.213.0.1).
func UpsertStaticA(cfg Config, ip, awsBin string, runFn ddns.RunCmdFn) (string, error) {
	return UpsertStaticAContext(context.Background(), cfg, ip, awsBin, runFn)
}

// UpsertStaticAContext upserts an A record for cfg.Domain pointing to ip bounded by ctx.
func UpsertStaticAContext(ctx context.Context, cfg Config, ip, awsBin string, runFn ddns.RunCmdFn) (string, error) {
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
	return ddns.RunRoute53ChangeContext(ctx, cfg.Profile, cfg.ZoneID, batch, awsBin, runFn)
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

// ChangeTXTContext upserts or deletes a Route 53 TXT record for ACME DNS-01 challenges bounded by ctx.
func ChangeTXTContext(ctx context.Context, cfg Config, fqdn, action, value, awsBin string, runFn ddns.RunCmdFn) (string, error) {
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
	return ddns.RunRoute53ChangeContext(ctx, cfg.Profile, cfg.ZoneID, batch, awsBin, runFn)
}

// WaitChangeInSync polls Route 53 until changeID reaches INSYNC, ctx expires, or consecutive errors reach the threshold.
func WaitChangeInSync(
	ctx context.Context,
	profile, changeID, awsBin string,
	runFn ddns.RunCmdFn,
	pollInterval time.Duration,
) error {
	if changeID == "" {
		return nil
	}
	bin, err := ddns.ResolveAWSBin(awsBin)
	if err != nil {
		return err
	}
	if pollInterval <= 0 {
		pollInterval = 2 * time.Second
	}
	args := []string{"route53", "get-change", "--profile", profile, "--id", changeID, "--output", "json"}
	consecutiveErrs := 0
	for {
		var stdout, stderr string
		if runFn != nil {
			stdout, stderr, err = runFn(bin, args...)
		} else {
			stdout, stderr, err = ddns.DefaultRunCmdContext(ctx, bin, args...)
		}
		if err == nil {
			var parsed struct {
				ChangeInfo struct {
					Status string `json:"Status"`
				} `json:"ChangeInfo"`
			}
			if jErr := json.Unmarshal([]byte(stdout), &parsed); jErr == nil {
				consecutiveErrs = 0
				if strings.EqualFold(parsed.ChangeInfo.Status, "INSYNC") {
					return nil
				}
			} else {
				consecutiveErrs++
				if ctx.Err() != nil || consecutiveErrs >= maxRoute53PollErrors {
					return fmt.Errorf("route 53 get-change %s invalid response: %w", changeID, jErr)
				}
			}
		} else {
			consecutiveErrs++
			if ctx.Err() != nil || consecutiveErrs >= maxRoute53PollErrors {
				return fmt.Errorf("route 53 get-change %s failed: %w (%s)", changeID, err, strings.TrimSpace(stderr))
			}
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
		digFn = func(args ...string) ([]string, error) {
			return ddns.DefaultDigContext(ctx, args...)
		}
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
