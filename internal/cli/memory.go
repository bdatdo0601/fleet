package cli

import (
	"cmp"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bdatdo0601/fleet/internal/client"
	"github.com/bdatdo0601/fleet/internal/memory"
)

func memoryUsageText() string {
	return `usage: fleet memory <subcommand> [options]

Subcommands:
  ls        [--scope S] [--kind K] [--tier T] [--all] [--limit N]
  search    QUERY [--scope S] [--kind K] [--tier T] [--limit N]
  get       TARGET [--scope S] [--history]
  set       TARGET [BODY] [--scope S] [--kind K] [--tier T] [--summary S] [--lock] [--file PATH] [--stdin]
  edit      TARGET [--scope S] [--kind K] [--tier T] [--summary S] [--lock]
  rm        TARGET [--scope S] [--hard]
  promote   TARGET [--to SCOPE[/KEY]] [--tier T] [--summary S] [--lock]
  history   [TARGET] [--scope S] [--limit N]
  revert    TARGET --rev N [--scope S]
  context   [--repo R] [--device D]
  import    [--dry-run] [--force]
  review    [ID] [--all]
  approve   [--all | ID ...]
  reject    [--all | ID ...]
  curator   setup|status|run|disable
`
}

func parseMemoryTarget(target, explicitScope string) (string, string, error) {
	t := strings.TrimSpace(target)
	if t == "" {
		return "", "", fmt.Errorf("memory target is required")
	}
	if explicitScope == "" {
		if scopePart, keyPart, ok := strings.Cut(t, "/"); ok && scopePart != "" && keyPart != "" {
			return scopePart, keyPart, nil
		}
		return "global", t, nil
	}
	return explicitScope, t, nil
}

func printMemoriesTable(cio *IO, items []memory.Item) {
	if len(items) == 0 {
		cio.printf("no memories found\n")
		return
	}
	rows := [][]string{{"SCOPE", "KEY", "KIND", "TIER", "STATUS", "UPDATED", "SUMMARY"}}
	for _, it := range items {
		up := it.UpdatedAt
		rows = append(rows, []string{
			it.Scope, it.Key, it.Kind, it.Tier, it.Status, ago(&up), memory.TruncateRunes(it.Summary, 64),
		})
	}
	printTable(cio.Stdout, rows)
}

func cmdMemoryLS(args []string, cio *IO) error {
	fs := flag.NewFlagSet("memory ls", flag.ContinueOnError)
	scope := fs.String("scope", "", "scope filter (global, repo:<name>, device:<name>)")
	kind := fs.String("kind", "", "kind filter")
	tier := fs.String("tier", "", "tier filter")
	all := fs.Bool("all", false, "include archived and superseded entries")
	limit := fs.Int("limit", 30, "max entries")
	pos, err := parseFlagsInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return fmt.Errorf("usage: fleet memory ls [--scope S] [--kind K] [--tier T] [--all] [--limit N]")
	}
	return fetchAndPrintMemories(cio, "", *scope, *kind, *tier, *all, *limit)
}

func cmdMemorySearch(args []string, cio *IO) error {
	fs := flag.NewFlagSet("memory search", flag.ContinueOnError)
	scope := fs.String("scope", "", "scope filter")
	kind := fs.String("kind", "", "kind filter")
	tier := fs.String("tier", "", "tier filter")
	all := fs.Bool("all", false, "include archived entries")
	limit := fs.Int("limit", 20, "max entries")
	pos, err := parseFlagsInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return fmt.Errorf("usage: fleet memory search QUERY [--scope S] [--kind K] [--tier T] [--limit N]")
	}
	return fetchAndPrintMemories(cio, strings.Join(pos, " "), *scope, *kind, *tier, *all, *limit)
}

func fetchAndPrintMemories(cio *IO, query, scope, kind, tier string, includeAll bool, limit int) error {
	api, err := client.ResolveAPI("")
	if err != nil {
		return err
	}
	q := url.Values{}
	if query != "" {
		q.Set("q", query)
	}
	if scope != "" {
		q.Set("scope", scope)
	}
	if kind != "" {
		q.Set("kind", kind)
	}
	if tier != "" {
		q.Set("tier", tier)
	}
	if includeAll {
		q.Set("include_archived", "true")
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	qs := ""
	if enc := q.Encode(); enc != "" {
		qs = "?" + enc
	}
	var resp struct {
		Memories []memory.Item `json:"memories"`
	}
	if err := client.Call(api, http.MethodGet, "/v1/memory"+qs, nil, 15*time.Second, nil, &resp); err != nil {
		return err
	}
	printMemoriesTable(cio, resp.Memories)
	return nil
}

func cmdMemoryGet(args []string, cio *IO) error {
	fs := flag.NewFlagSet("memory get", flag.ContinueOnError)
	scopeFlag := fs.String("scope", "", "memory scope")
	histFlag := fs.Bool("history", false, "show revision history")
	pos, err := parseFlagsInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("usage: fleet memory get TARGET [--scope S] [--history]")
	}
	scope, key, err := parseMemoryTarget(pos[0], *scopeFlag)
	if err != nil {
		return err
	}
	api, err := client.ResolveAPI("")
	if err != nil {
		return err
	}
	q := url.Values{"scope": {scope}, "key": {key}}
	if *histFlag {
		q.Set("history", "true")
	}
	var detail memory.ItemDetail
	if err := client.Call(api, http.MethodGet, "/v1/memory/item?"+q.Encode(), nil, 15*time.Second, nil, &detail); err != nil {
		return err
	}
	it := detail.Item
	cio.printf("scope:    %s\nkey:      %s\nkind:     %s\ntier:     %s\nstatus:   %s\nsource:   %s (rev %d, locked=%v)\nupdated:  %s (%s)\nsummary:  %s\n\n%s\n",
		it.Scope, it.Key, it.Kind, it.Tier, it.Status, it.Source, it.Revision, it.Locked,
		it.UpdatedAt, cmp.Or(it.Device, "-"), it.Summary, it.Body)
	if len(detail.History) > 0 {
		cio.printf("\nHistory:\n")
		for _, r := range detail.History {
			cio.printf("  r%d  %s  %-7s  %-10s  %s\n", r.Revision, r.CreatedAt, r.Status, r.Source, r.Reason)
		}
	}
	return nil
}

func readBodyInput(pos []string, filePath string, fromStdin bool, cio *IO) (string, error) {
	if filePath != "" {
		b, err := os.ReadFile(filePath)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	if len(pos) > 0 {
		return strings.Join(pos, " "), nil
	}
	if fromStdin {
		b, err := io.ReadAll(cio.Stdin)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	if f, ok := cio.Stdin.(*os.File); !ok || !isTerminalFile(f) {
		if cio.Stdin != nil {
			b, err := io.ReadAll(cio.Stdin)
			if err != nil {
				return "", err
			}
			if strings.TrimSpace(string(b)) != "" {
				return string(b), nil
			}
		}
	}
	return "", fmt.Errorf("body is required (pass inline, --file PATH, or --stdin)")
}

func isTerminalFile(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

func cmdMemorySet(args []string, cio *IO) error {
	fs := flag.NewFlagSet("memory set", flag.ContinueOnError)
	scopeFlag := fs.String("scope", "", "target scope (default global)")
	kind := fs.String("kind", "preference", "kind (preference|feedback|project|reference)")
	tier := fs.String("tier", "", "tier (always|searchable)")
	summary := fs.String("summary", "", "concise 1-line summary")
	lock := fs.Bool("lock", false, "lock against automated edits")
	filePath := fs.String("file", "", "read body from file")
	fromStdin := fs.Bool("stdin", false, "read body from stdin")
	pos, err := parseFlagsInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return fmt.Errorf("usage: fleet memory set TARGET [BODY] [--scope S] [--kind K] [--tier T] [--summary S] [--lock] [--file PATH] [--stdin]")
	}
	scope, key, err := parseMemoryTarget(pos[0], *scopeFlag)
	if err != nil {
		return err
	}
	body, err := readBodyInput(pos[1:], *filePath, *fromStdin, cio)
	if err != nil {
		return err
	}
	api, err := client.ResolveAPI("")
	if err != nil {
		return err
	}
	payload := map[string]any{
		"scope":          scope,
		"key":            key,
		"kind":           *kind,
		"tier":           *tier,
		"summary":        *summary,
		"body":           body,
		"source":         "human",
		"reason":         "cli_set",
		"enqueue_curate": false,
	}
	if *lock {
		payload["locked"] = true
	}
	var res memory.UpsertResult
	if err := client.Call(api, http.MethodPost, "/v1/memory", payload, 15*time.Second, nil, &res); err != nil {
		return err
	}
	cio.printf("%s %s/%s (tier=%s rev=%d)\n", res.Action, res.Item.Scope, res.Item.Key, res.Item.Tier, res.Item.Revision)
	return nil
}

func cmdMemoryEdit(args []string, cio *IO) error {
	fs := flag.NewFlagSet("memory edit", flag.ContinueOnError)
	scopeFlag := fs.String("scope", "", "memory scope")
	kindFlag := fs.String("kind", "", "override kind")
	tierFlag := fs.String("tier", "", "override tier")
	summaryFlag := fs.String("summary", "", "override summary")
	lock := fs.Bool("lock", false, "lock entry")
	pos, err := parseFlagsInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("usage: fleet memory edit TARGET [--scope S] [--kind K] [--tier T] [--summary S] [--lock]")
	}
	scope, key, err := parseMemoryTarget(pos[0], *scopeFlag)
	if err != nil {
		return err
	}
	api, err := client.ResolveAPI("")
	if err != nil {
		return err
	}
	q := url.Values{"scope": {scope}, "key": {key}}
	var detail memory.ItemDetail
	var existingBody, curKind, curTier, curSummary string
	err = client.Call(api, http.MethodGet, "/v1/memory/item?"+q.Encode(), nil, 15*time.Second, nil, &detail)
	if err == nil {
		existingBody, curKind, curTier, curSummary = detail.Item.Body, detail.Item.Kind, detail.Item.Tier, detail.Item.Summary
	} else {
		var apiErr *client.APIError
		if !errors.As(err, &apiErr) || apiErr.StatusCode != 404 {
			return err
		}
	}

	tmp, err := os.CreateTemp("", "fleet-memory-*.md")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.WriteString(existingBody); err != nil {
		_ = tmp.Close()
		return err
	}
	_ = tmp.Close()

	editor := cmp.Or(os.Getenv("VISUAL"), os.Getenv("EDITOR"), "vi")
	cmd := exec.Command(editor, tmpPath)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, cio.Stdout, cio.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("editor failed: %w", err)
	}
	edited, err := os.ReadFile(tmpPath)
	if err != nil {
		return err
	}
	payload := map[string]any{
		"scope":          scope,
		"key":            key,
		"kind":           cmp.Or(*kindFlag, curKind, "preference"),
		"tier":           cmp.Or(*tierFlag, curTier),
		"summary":        cmp.Or(*summaryFlag, curSummary),
		"body":           string(edited),
		"source":         "human",
		"reason":         "cli_edit",
		"enqueue_curate": false,
	}
	if *lock {
		payload["locked"] = true
	}
	var res memory.UpsertResult
	if err := client.Call(api, http.MethodPost, "/v1/memory", payload, 15*time.Second, nil, &res); err != nil {
		return err
	}
	cio.printf("%s %s/%s (rev=%d)\n", res.Action, res.Item.Scope, res.Item.Key, res.Item.Revision)
	return nil
}

func cmdMemoryRM(args []string, cio *IO) error {
	fs := flag.NewFlagSet("memory rm", flag.ContinueOnError)
	scopeFlag := fs.String("scope", "", "memory scope")
	hard := fs.Bool("hard", false, "permanently delete row (keeps audit revisions)")
	pos, err := parseFlagsInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("usage: fleet memory rm TARGET [--scope S] [--hard]")
	}
	scope, key, err := parseMemoryTarget(pos[0], *scopeFlag)
	if err != nil {
		return err
	}
	api, err := client.ResolveAPI("")
	if err != nil {
		return err
	}
	q := url.Values{"scope": {scope}, "key": {key}}
	if *hard {
		q.Set("hard", "true")
	}
	if err := client.Call(api, http.MethodDelete, "/v1/memory/item?"+q.Encode(), nil, 15*time.Second, nil, nil); err != nil {
		return err
	}
	cio.printf("removed %s/%s\n", scope, key)
	return nil
}

func cmdMemoryPromote(args []string, cio *IO) error {
	fs := flag.NewFlagSet("memory promote", flag.ContinueOnError)
	fromScopeFlag := fs.String("scope", "", "source scope")
	toFlag := fs.String("to", "global", "destination scope or scope/key")
	tier := fs.String("tier", "always", "destination tier (always|searchable)")
	summary := fs.String("summary", "", "override summary")
	lock := fs.Bool("lock", false, "lock destination entry")
	pos, err := parseFlagsInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("usage: fleet memory promote TARGET [--to SCOPE[/KEY]] [--tier always|searchable] [--summary S] [--lock]")
	}
	fromScope, fromKey, err := parseMemoryTarget(pos[0], *fromScopeFlag)
	if err != nil {
		return err
	}
	toScope, toKey := "global", fromKey
	if *toFlag != "" {
		if sc, k, ok := strings.Cut(*toFlag, "/"); ok && sc != "" && k != "" {
			toScope, toKey = sc, k
		} else if *toFlag == "global" || strings.HasPrefix(*toFlag, "repo:") || strings.HasPrefix(*toFlag, "device:") {
			toScope = *toFlag
		} else {
			toKey = *toFlag
		}
	}
	api, err := client.ResolveAPI("")
	if err != nil {
		return err
	}
	var resp struct {
		Memory memory.Item `json:"memory"`
	}
	if err := client.Call(api, http.MethodPost, "/v1/memory/promote", map[string]any{
		"from_scope": fromScope,
		"from_key":   fromKey,
		"to_scope":   toScope,
		"to_key":     toKey,
		"tier":       *tier,
		"summary":    *summary,
		"lock":       *lock,
	}, 15*time.Second, nil, &resp); err != nil {
		return err
	}
	cio.printf("promoted -> %s/%s (tier=%s rev=%d)\n", resp.Memory.Scope, resp.Memory.Key, resp.Memory.Tier, resp.Memory.Revision)
	return nil
}

func cmdMemoryHistory(args []string, cio *IO) error {
	fs := flag.NewFlagSet("memory history", flag.ContinueOnError)
	scopeFlag := fs.String("scope", "", "memory scope")
	limit := fs.Int("limit", 30, "max revisions")
	pos, err := parseFlagsInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return fmt.Errorf("usage: fleet memory history [TARGET] [--scope S] [--limit N]")
	}
	q := url.Values{}
	if len(pos) == 1 {
		scope, key, err := parseMemoryTarget(pos[0], *scopeFlag)
		if err != nil {
			return err
		}
		q.Set("scope", scope)
		q.Set("key", key)
	}
	if *limit > 0 {
		q.Set("limit", strconv.Itoa(*limit))
	}
	api, err := client.ResolveAPI("")
	if err != nil {
		return err
	}
	qs := ""
	if enc := q.Encode(); enc != "" {
		qs = "?" + enc
	}
	var resp struct {
		Revisions []memory.Revision `json:"revisions"`
	}
	if err := client.Call(api, http.MethodGet, "/v1/memory/history"+qs, nil, 15*time.Second, nil, &resp); err != nil {
		return err
	}
	if len(resp.Revisions) == 0 {
		cio.printf("no revisions found\n")
		return nil
	}
	rows := [][]string{{"TARGET", "REV", "STATUS", "SOURCE", "DEVICE", "WHEN", "REASON"}}
	for _, r := range resp.Revisions {
		cr := r.CreatedAt
		rows = append(rows, []string{
			r.Scope + "/" + r.Key, fmt.Sprintf("r%d", r.Revision), r.Status, r.Source,
			cmp.Or(r.Device, "-"), ago(&cr), r.Reason,
		})
	}
	printTable(cio.Stdout, rows)
	return nil
}

func cmdMemoryRevert(args []string, cio *IO) error {
	fs := flag.NewFlagSet("memory revert", flag.ContinueOnError)
	scopeFlag := fs.String("scope", "", "memory scope")
	rev := fs.Int("rev", 0, "revision number to restore")
	pos, err := parseFlagsInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || *rev <= 0 {
		return fmt.Errorf("usage: fleet memory revert TARGET --rev N [--scope S]")
	}
	scope, key, err := parseMemoryTarget(pos[0], *scopeFlag)
	if err != nil {
		return err
	}
	api, err := client.ResolveAPI("")
	if err != nil {
		return err
	}
	var resp struct {
		Memory memory.Item `json:"memory"`
	}
	if err := client.Call(api, http.MethodPost, "/v1/memory/revert", map[string]any{
		"scope": scope, "key": key, "revision": *rev,
	}, 15*time.Second, nil, &resp); err != nil {
		return err
	}
	cio.printf("reverted %s/%s -> rev %d\n", resp.Memory.Scope, resp.Memory.Key, resp.Memory.Revision)
	return nil
}

func cmdMemoryContext(args []string, cio *IO) error {
	fs := flag.NewFlagSet("memory context", flag.ContinueOnError)
	repoFlag := fs.String("repo", "", "repo slug (defaults to current git repo)")
	devFlag := fs.String("device", "", "device name")
	pos, err := parseFlagsInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return fmt.Errorf("usage: fleet memory context [--repo R] [--device D]")
	}
	repo := *repoFlag
	if repo == "" {
		if cwd, cErr := os.Getwd(); cErr == nil {
			repo = memory.CanonicalRepo(cwd)
		}
	}
	api, err := client.ResolveAPI("")
	if err != nil {
		return err
	}
	q := url.Values{}
	if repo != "" {
		q.Set("repo", repo)
	}
	if *devFlag != "" {
		q.Set("device", *devFlag)
	}
	qs := ""
	if enc := q.Encode(); enc != "" {
		qs = "?" + enc
	}
	var res memory.ContextResult
	if err := client.Call(api, http.MethodGet, "/v1/memory/context"+qs, nil, 15*time.Second, nil, &res); err != nil {
		return err
	}
	cio.printf("%s\n", res.Rendered)
	return nil
}

func cmdMemoryImport(args []string, cio *IO) error {
	fs := flag.NewFlagSet("memory import", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "preview files without uploading")
	force := fs.Bool("force", false, "re-upload even if already cached")
	pos, err := parseFlagsInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return fmt.Errorf("usage: fleet memory import [--dry-run] [--force]")
	}
	rep, err := memory.HarvestLocal(memory.HarvestOpts{
		DryRun: *dryRun,
		Force:  *force,
	})
	if err != nil {
		return err
	}
	if *dryRun {
		cio.printf("scanned %d file(s): %d pending, %d already synced\n", rep.Scanned, rep.Pending, rep.Skipped)
		for _, it := range rep.Items {
			cio.printf("  %s/%s (%s) <- %s\n", it.Scope, it.Key, it.Kind, it.OriginPath)
		}
		return nil
	}
	cio.printf("scanned %d file(s): +%d created, ~%d updated, %d unchanged, %d protected, %d queued for curation (skipped %d cached)\n",
		rep.Scanned, rep.Created, rep.Updated, rep.Unchanged, rep.Protected, rep.Queued, rep.Skipped)
	return nil
}

func cmdMemoryReview(args []string, cio *IO) error {
	fs := flag.NewFlagSet("memory review", flag.ContinueOnError)
	all := fs.Bool("all", false, "include approved and rejected proposals")
	pos, err := parseFlagsInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return fmt.Errorf("usage: fleet memory review [ID] [--all]")
	}
	api, err := client.ResolveAPI("")
	if err != nil {
		return err
	}
	if len(pos) == 1 {
		id, err := strconv.ParseInt(pos[0], 10, 64)
		if err != nil || id <= 0 {
			return fmt.Errorf("invalid proposal id %q", pos[0])
		}
		var resp struct {
			Proposal memory.Proposal `json:"proposal"`
		}
		if err := client.Call(api, http.MethodGet, "/v1/memory/proposals?id="+strconv.FormatInt(id, 10), nil, 15*time.Second, nil, &resp); err != nil {
			return err
		}
		p := resp.Proposal
		cio.printf("id:       #%d\naction:   %s\ntarget:   %s/%s\nkind:     %s\ntier:     %s\nstatus:   %s\nmodel:    %s\ncreated:  %s\nreason:   %s\nsummary:  %s\n\n%s\n",
			p.ID, p.Action, p.TargetScope, p.TargetKey, p.Kind, p.Tier, p.Status, cmp.Or(p.Model, "-"), p.CreatedAt, p.Reason, p.Summary, p.Body)
		return nil
	}
	qs := "?status=pending"
	if *all {
		qs = ""
	}
	var resp struct {
		Proposals []memory.Proposal `json:"proposals"`
	}
	if err := client.Call(api, http.MethodGet, "/v1/memory/proposals"+qs, nil, 15*time.Second, nil, &resp); err != nil {
		return err
	}
	if len(resp.Proposals) == 0 {
		cio.printf("no pending proposals\n")
		return nil
	}
	rows := [][]string{{"ID", "ACTION", "TARGET", "TIER", "STATUS", "REASON", "SUMMARY"}}
	for _, p := range resp.Proposals {
		rows = append(rows, []string{
			strconv.FormatInt(p.ID, 10), p.Action, p.TargetScope + "/" + p.TargetKey,
			p.Tier, p.Status, memory.TruncateRunes(p.Reason, 32), memory.TruncateRunes(p.Summary, 48),
		})
	}
	printTable(cio.Stdout, rows)
	return nil
}

func fetchPendingProposalIDs(api string) ([]int64, error) {
	var resp struct {
		Proposals []memory.Proposal `json:"proposals"`
	}
	if err := client.Call(api, http.MethodGet, "/v1/memory/proposals?status=pending&limit=200", nil, 15*time.Second, nil, &resp); err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(resp.Proposals))
	for _, p := range resp.Proposals {
		ids = append(ids, p.ID)
	}
	slices.Sort(ids)
	return ids, nil
}

func cmdMemoryResolve(args []string, decision string, cio *IO) error {
	fs := flag.NewFlagSet("memory "+decision, flag.ContinueOnError)
	all := fs.Bool("all", false, "resolve all pending proposals")
	pos, err := parseFlagsInterspersed(fs, args)
	if err != nil {
		return err
	}
	if (*all && len(pos) > 0) || (!*all && len(pos) == 0) {
		return fmt.Errorf("usage: fleet memory %s [--all | ID ...]", decision)
	}
	api, err := client.ResolveAPI("")
	if err != nil {
		return err
	}
	var ids []int64
	if *all {
		if ids, err = fetchPendingProposalIDs(api); err != nil {
			return err
		}
		if len(ids) == 0 {
			cio.printf("no pending proposals\n")
			return nil
		}
	} else {
		ids = make([]int64, 0, len(pos))
		for _, raw := range pos {
			id, pErr := strconv.ParseInt(raw, 10, 64)
			if pErr != nil || id <= 0 {
				return fmt.Errorf("invalid proposal id %q", raw)
			}
			ids = append(ids, id)
		}
	}
	for _, id := range ids {
		var resp struct {
			Proposal memory.Proposal `json:"proposal"`
			Memory   *memory.Item    `json:"memory"`
		}
		if err := client.Call(api, http.MethodPost, "/v1/memory/proposals/resolve", map[string]any{
			"id": id, "decision": decision,
		}, 15*time.Second, nil, &resp); err != nil {
			return err
		}
		if resp.Memory != nil {
			cio.printf("%s #%d -> %s/%s (rev=%d)\n", resp.Proposal.Status, resp.Proposal.ID, resp.Memory.Scope, resp.Memory.Key, resp.Memory.Revision)
		} else {
			cio.printf("%s #%d\n", resp.Proposal.Status, resp.Proposal.ID)
		}
	}
	return nil
}

func printCuratorStatus(cio *IO, st memory.CuratorStatus) {
	state := "disabled"
	if st.Enabled {
		state = "enabled"
	} else if !st.Configured {
		state = "not configured"
	}
	cio.printf("status:      %s\n", state)
	if st.Configured {
		cio.printf("provider:    %s (%s)\n", st.Provider, st.BaseURL)
		cio.printf("model:       %s\n", st.Model)
		cio.printf("api key:     %s\n", cmp.Or(st.APIKeyHint, "-"))
		if st.Provider == "openrouter" {
			cio.printf("privacy:     data_collection=%s, zdr=%v\n", cmp.Or(st.DataCollection, "deny"), st.ZDR)
		}
		gMode := "auto-apply (opt-out)"
		if st.ReviewGlobal {
			gMode = "review queue (opt-in)"
		}
		cio.printf("global mode: %s\n", gMode)
	}
	cio.printf("queue:       %d pending, %d failed, %d proposal(s) awaiting review\n", st.PendingJobs, st.FailedJobs, st.PendingProposals)
	cio.printf("processed:   %d\n", st.ProcessedCount)
	if st.LastRunAt != "" {
		cio.printf("last run:    %s\n", st.LastRunAt)
	}
	if st.LastError != "" {
		cio.printf("last error:  %s\n", st.LastError)
	}
}

func cmdMemoryCuratorSetup(args []string, cio *IO) error {
	fs := flag.NewFlagSet("memory curator setup", flag.ContinueOnError)
	provider := fs.String("provider", "openrouter", "LLM provider (openrouter|openai|ollama|custom)")
	baseURL := fs.String("url", "", "OpenAI-compatible base URL override")
	model := fs.String("model", "", "model slug (default openai/gpt-6-luna for openrouter)")
	opRef := fs.String("op-ref", "", "1Password secret reference (e.g. op://Vault/OpenRouter/credential)")
	apiKeyStdin := fs.Bool("api-key-stdin", false, "read API key from stdin")
	zdr := fs.Bool("zdr", false, "require OpenRouter Zero Data Retention routing")
	reviewGlobal := fs.Bool("review-global", false, "queue global promotions for manual review instead of auto-applying")
	keepAlive := fs.String("keep-alive", "", "Ollama keep_alive duration (e.g. 2m)")
	numCtx := fs.Int("num-ctx", 0, "Ollama context length")
	noTest := fs.Bool("no-test", false, "skip preflight test request before saving")
	pos, err := parseFlagsInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(pos, " "))
	}

	api, err := client.ResolveAPI("")
	if err != nil {
		return err
	}

	var apiKey string
	switch {
	case *opRef != "":
		if apiKey, err = cio.readOPRef(*opRef); err != nil {
			return err
		}
	case *apiKeyStdin:
		b, rErr := io.ReadAll(cio.Stdin)
		if rErr != nil {
			return rErr
		}
		apiKey = strings.TrimSpace(string(b))
		if apiKey == "" {
			return fmt.Errorf("empty API key on stdin")
		}
	case *provider == "openrouter" || *provider == "openai":
		var existing memory.CuratorStatus
		_ = client.Call(api, http.MethodGet, "/v1/memory/curator", nil, 10*time.Second, nil, &existing)
		prompt := "API key: "
		if existing.APIKeyHint != "" && existing.Provider == *provider {
			prompt = fmt.Sprintf("API key (Enter to keep %s): ", existing.APIKeyHint)
		}
		if apiKey, err = cio.askSecret(prompt); err != nil {
			return err
		}
	}

	var st memory.CuratorStatus
	if err := client.Call(api, http.MethodPost, "/v1/memory/curator", map[string]any{
		"enabled":       true,
		"provider":      *provider,
		"base_url":      *baseURL,
		"model":         *model,
		"api_key":       apiKey,
		"zdr":           *zdr,
		"review_global": *reviewGlobal,
		"keep_alive":    *keepAlive,
		"num_ctx":       *numCtx,
		"test":          !*noTest,
	}, 45*time.Second, nil, &st); err != nil {
		return err
	}
	printCuratorStatus(cio, st)
	return nil
}

func cmdMemoryCuratorRun(args []string, cio *IO) error {
	fs := flag.NewFlagSet("memory curator run", flag.ContinueOnError)
	limit := fs.Int("limit", 20, "max items to curate")
	dryRun := fs.Bool("dry-run", false, "preview decisions without modifying memory.db")
	all := fs.Bool("all", false, "evaluate active preference/feedback entries even if not queued")
	pos, err := parseFlagsInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return fmt.Errorf("usage: fleet memory curator run [--limit N] [--dry-run] [--all]")
	}
	api, err := client.ResolveAPI("")
	if err != nil {
		return err
	}
	var resp struct {
		Reports []memory.CurateActionReport `json:"reports"`
		Status  memory.CuratorStatus        `json:"status"`
	}
	if err := client.Call(api, http.MethodPost, "/v1/memory/curator/run", map[string]any{
		"limit":       *limit,
		"dry_run":     *dryRun,
		"include_all": *all,
	}, 120*time.Second, nil, &resp); err != nil {
		return err
	}
	if len(resp.Reports) == 0 {
		cio.printf("nothing to curate\n")
		return nil
	}
	rows := [][]string{{"MEMORY", "ACTION", "TARGET", "APPLIED", "PROPOSAL", "NOTE"}}
	for _, r := range resp.Reports {
		prop := "-"
		if r.QueuedProposalID > 0 {
			prop = fmt.Sprintf("#%d", r.QueuedProposalID)
		}
		target := "-"
		if r.TargetScope != "" || r.TargetKey != "" {
			target = r.TargetScope + "/" + r.TargetKey
		}
		rows = append(rows, []string{
			r.MemoryScope + "/" + r.MemoryKey, r.Action, target,
			strconv.FormatBool(r.Applied), prop, memory.TruncateRunes(cmp.Or(r.Error, r.Reason), 48),
		})
	}
	printTable(cio.Stdout, rows)
	return nil
}

func cmdMemoryCurator(args []string, cio *IO) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: fleet memory curator setup|status|run|disable")
	}
	switch args[0] {
	case "setup":
		return cmdMemoryCuratorSetup(args[1:], cio)
	case "run":
		return cmdMemoryCuratorRun(args[1:], cio)
	case "status":
		api, err := client.ResolveAPI("")
		if err != nil {
			return err
		}
		var st memory.CuratorStatus
		if err := client.Call(api, http.MethodGet, "/v1/memory/curator", nil, 15*time.Second, nil, &st); err != nil {
			return err
		}
		printCuratorStatus(cio, st)
		return nil
	case "disable":
		api, err := client.ResolveAPI("")
		if err != nil {
			return err
		}
		var st memory.CuratorStatus
		if err := client.Call(api, http.MethodPost, "/v1/memory/curator", map[string]any{"disable": true}, 15*time.Second, nil, &st); err != nil {
			return err
		}
		printCuratorStatus(cio, st)
		return nil
	default:
		return fmt.Errorf("unknown curator subcommand %q", args[0])
	}
}

func cmdMemory(args []string, cio *IO) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		_, _ = io.WriteString(cio.Stdout, memoryUsageText())
		return nil
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "ls":
		return cmdMemoryLS(rest, cio)
	case "search":
		return cmdMemorySearch(rest, cio)
	case "get":
		return cmdMemoryGet(rest, cio)
	case "set":
		return cmdMemorySet(rest, cio)
	case "edit":
		return cmdMemoryEdit(rest, cio)
	case "rm":
		return cmdMemoryRM(rest, cio)
	case "promote":
		return cmdMemoryPromote(rest, cio)
	case "history":
		return cmdMemoryHistory(rest, cio)
	case "revert":
		return cmdMemoryRevert(rest, cio)
	case "context":
		return cmdMemoryContext(rest, cio)
	case "import":
		return cmdMemoryImport(rest, cio)
	case "review":
		return cmdMemoryReview(rest, cio)
	case "approve":
		return cmdMemoryResolve(rest, "approve", cio)
	case "reject":
		return cmdMemoryResolve(rest, "reject", cio)
	case "curator":
		return cmdMemoryCurator(rest, cio)
	default:
		return fmt.Errorf("unknown memory subcommand %q\n\n%s", sub, memoryUsageText())
	}
}
