package ddns

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

var testCfg = Route53{
	Name:        "lighthouse.example.com",
	ZoneID:      "Z0123ABC",
	Profile:     "fleet-ddns",
	Nameservers: []string{"ns-1.awsdns.com", "ns-2.awsdns.net"},
}

func makeFetcher(replies ...any) FetchFn {
	i := 0
	return func(_ string) (string, error) {
		r := replies[i]
		i++
		if err, ok := r.(error); ok {
			return "", err
		}
		return r.(string), nil
	}
}

func TestPublicIPv4(t *testing.T) {
	ip, err := PublicIPv4(makeFetcher("9.9.9.9\n"))
	if err != nil || ip != "9.9.9.9" {
		t.Fatalf("got (%q, %v)", ip, err)
	}
	ip, err = PublicIPv4(makeFetcher(errors.New("down"), "1.1.1.1"))
	if err != nil || ip != "1.1.1.1" {
		t.Fatalf("fallback got (%q, %v)", ip, err)
	}
	ip, err = PublicIPv4(makeFetcher("<html>", "8.8.8.8"))
	if err != nil || ip != "8.8.8.8" {
		t.Fatalf("html fallback got (%q, %v)", ip, err)
	}

	for _, addr := range []string{"100.64.1.2", "192.168.1.5", "10.0.0.1", "127.0.0.1", "198.18.0.1"} {
		_, err := PublicIPv4(makeFetcher(addr))
		if err == nil || !strings.Contains(err.Error(), "not internet-routable") {
			t.Fatalf("expected non-routable error for %s, got %v", addr, err)
		}
	}

	_, err = PublicIPv4(makeFetcher(errors.New("a"), errors.New("b")))
	if err == nil || !strings.Contains(err.Error(), "couldn't determine") {
		t.Fatalf("expected couldn't determine error, got %v", err)
	}
}

func TestDNSHelpers(t *testing.T) {
	var seen [][]string
	dig := func(args ...string) ([]string, error) {
		seen = append(seen, args)
		if args[0] == "example.com" {
			return []string{"ns-2.awsdns.net.", "ns-1.awsdns.com."}, nil
		}
		return nil, nil
	}
	ns, err := FindNameservers("lighthouse.example.com", dig)
	if err != nil {
		t.Fatal(err)
	}
	if len(ns) != 2 || ns[0] != "ns-1.awsdns.com" || ns[1] != "ns-2.awsdns.net" {
		t.Fatalf("unexpected ns: %v", ns)
	}
	if seen[0][0] != "lighthouse.example.com" || seen[1][0] != "example.com" {
		t.Fatalf("unexpected dig walk: %v", seen)
	}

	var calls [][]string
	curDig := func(args ...string) ([]string, error) {
		calls = append(calls, args)
		if args[len(args)-1] == "@ns-1.awsdns.com" {
			return nil, errors.New("timeout")
		}
		return []string{"9.9.9.9"}, nil
	}
	rec, err := CurrentRecord(testCfg, curDig)
	if err != nil || rec == nil || *rec != "9.9.9.9" {
		t.Fatalf("unexpected CurrentRecord: (%v, %v)", rec, err)
	}
	last := calls[len(calls)-1]
	if last[0] != "lighthouse.example.com" || last[1] != "A" || last[2] != "@ns-2.awsdns.net" {
		t.Fatalf("unexpected last call: %v", last)
	}
	rec, err = CurrentRecord(testCfg, func(...string) ([]string, error) { return nil, nil })
	if err != nil || rec != nil {
		t.Fatalf("expected nil record, got (%v, %v)", rec, err)
	}
	_, err = CurrentRecord(testCfg, func(...string) ([]string, error) { return nil, errors.New("x") })
	if err == nil || !strings.Contains(err.Error(), "no authoritative") {
		t.Fatalf("expected error, got %v", err)
	}
}

func TestSync(t *testing.T) {
	var calls [][]string
	fakeAWS := func(name string, args ...string) (string, string, error) {
		calls = append(calls, append([]string{name}, args...))
		return `{"ChangeInfo":{"Id":"/change/C1","Status":"PENDING"}}`, "", nil
	}
	before, ip, changed, err := Sync(testCfg, makeFetcher("9.9.9.9"), func(...string) ([]string, error) { return []string{"9.9.9.9"}, nil }, "/opt/homebrew/bin/aws", fakeAWS)
	if err != nil || before == nil || *before != "9.9.9.9" || ip != "9.9.9.9" || changed || len(calls) != 0 {
		t.Fatalf("unexpected no-op sync: before=%v ip=%q changed=%v err=%v calls=%v", before, ip, changed, err, calls)
	}

	before, ip, changed, err = Sync(testCfg, makeFetcher("1.1.1.9"), func(...string) ([]string, error) { return nil, nil }, "/opt/homebrew/bin/aws", fakeAWS)
	if err != nil || before != nil || ip != "1.1.1.9" || !changed || len(calls) != 1 {
		t.Fatalf("unexpected update sync: before=%v ip=%q changed=%v err=%v", before, ip, changed, err)
	}
	cmd := calls[0]
	var batchJSON string
	for i := 0; i < len(cmd)-1; i++ {
		if cmd[i] == "--change-batch" {
			batchJSON = cmd[i+1]
		}
	}
	var batch map[string]any
	if err := json.Unmarshal([]byte(batchJSON), &batch); err != nil {
		t.Fatal(err)
	}
	changes := batch["Changes"].([]any)
	if len(changes) != 1 {
		t.Fatalf("unexpected changes: %v", changes)
	}

	failAWS := func(_ string, _ ...string) (string, string, error) {
		return "", "AccessDenied", errors.New("exit 255")
	}
	_, _, _, err = Sync(testCfg, makeFetcher("1.1.1.9"), func(...string) ([]string, error) { return nil, nil }, "/opt/homebrew/bin/aws", failAWS)
	if err == nil || !strings.Contains(err.Error(), "AccessDenied") {
		t.Fatalf("expected AccessDenied, got %v", err)
	}
}

func TestPolicyAndZoneID(t *testing.T) {
	p, err := Policy("/hostedzone/Z0123ABC", "Lighthouse.example.com.")
	if err != nil {
		t.Fatal(err)
	}
	stmts := p["Statement"].([]map[string]any)
	if stmts[0]["Resource"] != "arn:aws:route53:::hostedzone/Z0123ABC" {
		t.Fatalf("unexpected resource: %v", stmts[0]["Resource"])
	}
	if _, err := NormalizeZoneID("example.com"); err == nil {
		t.Fatal("expected error for invalid zone id")
	}
}
