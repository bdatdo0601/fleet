package memory

import (
	"cmp"
	"database/sql"
	"fmt"
	"slices"
	"strings"
)

func contextScopes(repo, device string) ([]string, string) {
	scopes := []string{"global"}
	var repoScope string
	if r := NormalizeSlug(repo, 80); r != "" {
		if ns, err := NormalizeScope("repo:" + r); err == nil {
			scopes = append(scopes, ns)
			repoScope = ns
		}
	}
	if d := NormalizeSlug(device, 80); d != "" {
		if ns, err := NormalizeScope("device:" + d); err == nil && !slices.Contains(scopes, ns) {
			scopes = append(scopes, ns)
		}
	}
	return scopes, repoScope
}

func (s *Store) queryAlwaysForContext(db *sql.DB, scopes []string) ([]Item, error) {
	ph := strings.Repeat(",?", len(scopes))[1:]
	args := make([]any, 0, len(scopes))
	for _, sc := range scopes {
		args = append(args, sc)
	}
	q := "SELECT " + itemSelectCols + ` FROM memories m
		WHERE m.status = 'active' AND m.tier = 'always' AND m.scope IN (` + ph + `)
		ORDER BY CASE
		  WHEN m.scope = 'global' THEN 0
		  WHEN m.scope LIKE 'repo:%' THEN 1
		  ELSE 2
		END, CASE WHEN m.kind IN ('preference', 'feedback') THEN 0 ELSE 1 END, m.updated_at DESC`
	return queryItems(db, q, args, false)
}

func (s *Store) querySearchableForContext(db *sql.DB, repoScope string) ([]Item, error) {
	scopes := []string{"global"}
	if repoScope != "" {
		scopes = append(scopes, repoScope)
	}
	ph := strings.Repeat(",?", len(scopes))[1:]
	args := make([]any, 0, len(scopes))
	for _, sc := range scopes {
		args = append(args, sc)
	}
	q := "SELECT " + itemSelectCols + ` FROM memories m
		WHERE m.status = 'active' AND m.tier = 'searchable' AND m.scope IN (` + ph + `)
		ORDER BY CASE WHEN m.scope = 'global' THEN 0 ELSE 1 END,
		         CASE WHEN m.kind IN ('preference', 'feedback') THEN 0 ELSE 1 END,
		         m.updated_at DESC
		LIMIT 12`
	return queryItems(db, q, args, false)
}

func formatAlwaysLine(it Item) string {
	text := cmp.Or(it.Summary, DeriveSummary("", it.Body))
	return fmt.Sprintf("- [%s/%s] %s", it.Scope, it.Key, TruncateRunes(text, MaxSummaryRunes))
}

func formatIndexLine(it Item) string {
	text := cmp.Or(it.Summary, DeriveSummary("", it.Body))
	return fmt.Sprintf("- %s/%s (%s): %s", it.Scope, it.Key, it.Kind, TruncateRunes(text, 140))
}

// RenderContext renders the SessionStart fleet memory prompt block capped at MaxContextBytes.
func (s *Store) RenderContext(repo, device string) (ContextResult, error) {
	scopes, repoScope := contextScopes(repo, device)

	s.mu.Lock()
	db, err := s.dbLocked()
	if err != nil {
		s.mu.Unlock()
		return ContextResult{}, err
	}
	always, err := s.queryAlwaysForContext(db, scopes)
	if err != nil {
		s.mu.Unlock()
		return ContextResult{}, err
	}
	searchable, err := s.querySearchableForContext(db, repoScope)
	s.mu.Unlock()
	if err != nil {
		return ContextResult{}, err
	}

	saveScope := cmp.Or(repoScope, "global")
	header := fmt.Sprintf("[Fleet Memory] (scopes: %s)", strings.Join(scopes, ", "))
	footer := fmt.Sprintf(
		"Fleet memory (`fleet-memory` skill): `fleet memory search|get|set|rm` — save standing preferences (`global`, `--tier always`) "+
			"or durable repo notes (%q, `--source fleet`). Never save ephemeral task state or code facts derivable from git; update existing keys.",
		saveScope,
	)

	var body strings.Builder
	body.WriteString(header)
	reserved := len(footer) + 64
	budget := MaxContextBytes - reserved

	truncated := false
	appendLine := func(ln string) bool {
		if body.Len()+len(ln)+1 > budget {
			truncated = true
			return false
		}
		body.WriteByte('\n')
		body.WriteString(ln)
		return true
	}

	if len(always) > 0 && appendLine("Standing preferences:") {
		for _, it := range always {
			if !appendLine(formatAlwaysLine(it)) {
				break
			}
		}
	}
	if len(searchable) > 0 && appendLine("Indexed memories:") {
		for _, it := range searchable {
			if !appendLine(formatIndexLine(it)) {
				break
			}
		}
	}
	if truncated {
		body.WriteString("\n... (truncated; use `fleet memory search`)")
	}
	body.WriteString("\n")
	body.WriteString(footer)

	rendered := body.String()
	if len(rendered) > MaxContextBytes {
		const ellipsis = "…"
		rendered = truncateUTF8Bytes(rendered, MaxContextBytes-len(ellipsis)) + ellipsis
	}
	return ContextResult{
		Scopes:          scopes,
		AlwaysCount:     len(always),
		SearchableCount: len(searchable),
		TotalCount:      len(always) + len(searchable),
		Rendered:        rendered,
	}, nil
}
