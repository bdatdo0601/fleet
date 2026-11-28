package assistant

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestParseSkillSource(t *testing.T) {
	cases := []struct {
		name      string
		source    string
		skill     string
		ref       string
		wantKind  string
		wantOwner string
		wantRepo  string
		wantSkill string
		wantSub   string
		wantRef   string
		wantURL   string
		wantErr   bool
	}{
		{
			name:      "shorthand with @skill",
			source:    "anthropics/skills@pdf",
			wantKind:  "github",
			wantOwner: "anthropics",
			wantRepo:  "skills",
			wantSkill: "pdf",
		},
		{
			name:      "shorthand repo with explicit skill",
			source:    "vercel-labs/agent-skills",
			skill:     "react-best-practices",
			wantKind:  "github",
			wantOwner: "vercel-labs",
			wantRepo:  "agent-skills",
			wantSkill: "react-best-practices",
		},
		{
			name:      "shorthand with subpath",
			source:    "owner/repo/skills/my-skill",
			wantKind:  "github",
			wantOwner: "owner",
			wantRepo:  "repo",
			wantSub:   "skills/my-skill",
		},
		{
			name:      "skills.sh URL",
			source:    "https://skills.sh/anthropics/skills/pdf",
			wantKind:  "github",
			wantOwner: "anthropics",
			wantRepo:  "skills",
			wantSkill: "pdf",
		},
		{
			name:      "github tree URL",
			source:    "https://github.com/anthropics/skills/tree/main/skills/pdf",
			wantKind:  "github",
			wantOwner: "anthropics",
			wantRepo:  "skills",
			wantSub:   "skills/pdf",
			wantRef:   "main",
		},
		{
			name:      "github blob SKILL.md URL",
			source:    "https://github.com/anthropics/skills/blob/v1.0/skills/pdf/SKILL.md",
			wantKind:  "github",
			wantOwner: "anthropics",
			wantRepo:  "skills",
			wantSub:   "skills/pdf",
			wantRef:   "v1.0",
		},
		{
			name:     "direct HTTPS SKILL.md",
			source:   "https://example.com/.well-known/skills/pdf/SKILL.md",
			wantKind: "url",
			wantURL:  "https://example.com/.well-known/skills/pdf/SKILL.md",
		},
		{
			name:    "reject empty",
			source:  "   ",
			wantErr: true,
		},
		{
			name:    "reject file scheme",
			source:  "file:///etc/passwd",
			wantErr: true,
		},
		{
			name:    "reject traversal",
			source:  "owner/repo/../../secret",
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseSkillSource(tc.source, tc.skill, tc.ref)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got %+v", tc.source, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.source, err)
			}
			if got.Kind != tc.wantKind || got.Owner != tc.wantOwner || got.Repo != tc.wantRepo ||
				got.SkillID != tc.wantSkill || got.Subpath != tc.wantSub || got.Ref != tc.wantRef || got.DirectURL != tc.wantURL {
				t.Fatalf("unexpected parsed result for %q: %+v", tc.source, got)
			}
		})
	}
}

func TestSanitizeExternalSkillContent(t *testing.T) {
	withSync := "---\nname: pdf\ndescription: Read PDFs\nsync_to_fleet: true\ntags: [docs]\n---\n# PDF\nSteps here.\n"
	sanitized, stripped := SanitizeExternalSkillContent(withSync)
	if !stripped {
		t.Fatalf("expected sync_to_fleet: true to be stripped")
	}
	if strings.Contains(sanitized, "sync_to_fleet") {
		t.Fatalf("expected sync_to_fleet removed, got:\n%s", sanitized)
	}
	if !strings.Contains(sanitized, "# PDF\nSteps here.") {
		t.Fatalf("expected markdown body preserved, got:\n%s", sanitized)
	}
	sk := ParseSkillFrontmatter("pdf", sanitized)
	if sk.SyncToFleet || sk.Description != "Read PDFs" || len(sk.Tags) != 1 || sk.Tags[0] != "docs" {
		t.Fatalf("unexpected parsed frontmatter after sanitize: %+v", sk)
	}

	clean := "---\nname: clean\ndescription: Clean skill\n---\nBody.\n"
	out, stripped2 := SanitizeExternalSkillContent(clean)
	if stripped2 || out != clean {
		t.Fatalf("expected clean skill unchanged, got stripped=%v out=%q", stripped2, out)
	}
}

func TestSkillRegistrySSRFBlock(t *testing.T) {
	localSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("---\nname: x\n---\n"))
	}))
	defer localSrv.Close()

	reg := NewSkillRegistry() // AllowPrivateURLs = false
	if _, err := reg.Fetch(t.Context(), localSrv.URL+"/SKILL.md", "", "", ""); err == nil {
		t.Fatalf("expected default SkillRegistry to reject loopback/http URL")
	}

	reg.RegistryBaseURL = localSrv.URL
	if _, err := reg.Search(t.Context(), "pdf", 5); err == nil {
		t.Fatalf("expected default SkillRegistry Search to reject loopback/http URL")
	}
}

func TestSkillToolsSearchFetchInstallRemove(t *testing.T) {
	const commitSHA = "0123456789abcdef0123456789abcdef01234567"
	var treeStatus atomic.Int32
	treeStatus.Store(http.StatusOK)

	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/search":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"skills": []map[string]any{
					{
						"id":       "anthropics/skills/pdf",
						"source":   "anthropics/skills",
						"skillId":  "pdf",
						"name":     "pdf",
						"installs": 12345,
					},
				},
			})
		case "/repos/anthropics/skills/commits/HEAD":
			_, _ = w.Write([]byte(commitSHA))
		case "/repos/anthropics/skills/git/trees/" + commitSHA:
			if st := int(treeStatus.Load()); st != http.StatusOK {
				http.Error(w, "rate limited", st)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tree": []map[string]any{
					{"path": "skills/pdf/SKILL.md", "mode": "100644", "type": "blob", "size": 120},
					{"path": "skills/pdf/REFERENCE.md", "mode": "100644", "type": "blob", "size": 40},
					{"path": "skills/pdf/scripts/helper.py", "mode": "100755", "type": "blob", "size": 30},
					{"path": "skills/pdf/assets/icon.png", "mode": "100644", "type": "blob", "size": 16},
					{"path": "skills/pdf/.hidden", "mode": "100644", "type": "blob", "size": 10},
					{"path": "skills/docx/SKILL.md", "mode": "100644", "type": "blob", "size": 80},
				},
			})
		case "/anthropics/skills/" + commitSHA + "/skills/pdf/SKILL.md":
			_, _ = w.Write([]byte("---\nname: pdf\ndescription: Extract text from PDFs\nsync_to_fleet: true\ntags: [pdf, docs]\n---\n# PDF Guide\nUse sandbox_exec.\n"))
		case "/anthropics/skills/" + commitSHA + "/skills/pdf/REFERENCE.md":
			_, _ = w.Write([]byte("# Reference\nDetailed PDF notes.\n"))
		case "/anthropics/skills/" + commitSHA + "/skills/pdf/scripts/helper.py":
			_, _ = w.Write([]byte("print('hello pdf')\n"))
		case "/anthropics/skills/" + commitSHA + "/skills/pdf/assets/icon.png":
			_, _ = w.Write([]byte{0x89, 'P', 'N', 'G', 0x00, 0x01})
		default:
			http.NotFound(w, r)
		}
	}))
	defer mockSrv.Close()

	tmp := t.TempDir()
	eng, err := Open(filepath.Join(tmp, "assistant.db"), filepath.Join(tmp, "assistant.json"), filepath.Join(tmp, "vault"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = eng.Close() }()

	eng.SetSkillRegistry(&SkillRegistry{
		RegistryBaseURL:  mockSrv.URL,
		GitHubAPIBaseURL: mockSrv.URL,
		GitHubRawBaseURL: mockSrv.URL,
		AllowPrivateURLs: true,
	})

	tctxChat := &toolExecContext{ChannelID: "general", Source: "chat", Config: eng.Config()}
	tctxHeartbeat := &toolExecContext{ChannelID: "briefing", Source: "heartbeat", Config: eng.Config()}
	tctxApproved := &toolExecContext{ChannelID: "general", Source: "approval", Config: eng.Config(), BypassHITL: true}

	// 1. skill_search returns parsed results.
	searchOut, err := eng.executeTool(t.Context(), ToolCall{
		ID:       "s1",
		Type:     "function",
		Function: ToolCallFunction{Name: "skill_search", Arguments: `{"query":"pdf"}`},
	}, tctxChat)
	if err != nil || !strings.Contains(searchOut, "anthropics/skills@pdf") {
		t.Fatalf("skill_search failed: %v (%s)", err, searchOut)
	}

	// 2. skill_fetch on multi-skill repo without @skill disambiguates cleanly.
	if _, err := eng.executeTool(t.Context(), ToolCall{
		ID:       "s2",
		Type:     "function",
		Function: ToolCallFunction{Name: "skill_fetch", Arguments: `{"source":"anthropics/skills"}`},
	}, tctxChat); err == nil || !strings.Contains(err.Error(), "multiple skills") {
		t.Fatalf("expected multi-skill disambiguation error, got %v", err)
	}

	// 3. skill_fetch previews bundle, detects scripts, strips sync_to_fleet, skips binary/hidden files.
	fetchOut, err := eng.executeTool(t.Context(), ToolCall{
		ID:       "s3",
		Type:     "function",
		Function: ToolCallFunction{Name: "skill_fetch", Arguments: `{"source":"anthropics/skills@pdf"}`},
	}, tctxChat)
	if err != nil {
		t.Fatalf("skill_fetch failed: %v", err)
	}
	if !strings.Contains(fetchOut, `"has_scripts":true`) || !strings.Contains(fetchOut, `"sync_to_fleet_stripped":true`) || !strings.Contains(fetchOut, "REFERENCE.md") {
		t.Fatalf("unexpected skill_fetch output: %s", fetchOut)
	}

	// 4. skill_fetch falls back to raw paths when GitHub tree API returns 403 rate limit.
	treeStatus.Store(http.StatusForbidden)
	fbOut, err := eng.executeTool(t.Context(), ToolCall{
		ID:       "s4",
		Type:     "function",
		Function: ToolCallFunction{Name: "skill_fetch", Arguments: `{"source":"anthropics/skills@pdf"}`},
	}, tctxChat)
	if err != nil || !strings.Contains(fbOut, "PDF Guide") {
		t.Fatalf("expected raw fallback to succeed on 403, got %v (%s)", err, fbOut)
	}
	treeStatus.Store(http.StatusOK)

	// 5. skill_install is blocked in heartbeat turns.
	if _, err := eng.executeTool(t.Context(), ToolCall{
		ID:       "s5",
		Type:     "function",
		Function: ToolCallFunction{Name: "skill_install", Arguments: `{"source":"anthropics/skills@pdf"}`},
	}, tctxHeartbeat); err == nil || !strings.Contains(err.Error(), "direct user chat") {
		t.Fatalf("expected heartbeat skill_install to be forbidden, got %v", err)
	}

	// 6. skill_install in chat pauses for HITL approval with pinned commit SHA, without writing yet.
	_, err = eng.executeTool(t.Context(), ToolCall{
		ID:       "s6",
		Type:     "function",
		Function: ToolCallFunction{Name: "skill_install", Arguments: `{"source":"anthropics/skills@pdf"}`},
	}, tctxChat)
	aprErr, ok := errors.AsType[*approvalRequiredError](err)
	if !ok || aprErr.RiskLevel != "skill_install" {
		t.Fatalf("expected approvalRequiredError(skill_install), got %v", err)
	}
	if !strings.Contains(aprErr.ArgsJSON, commitSHA) {
		t.Fatalf("expected commit SHA pinned in approval ArgsJSON, got %s", aprErr.ArgsJSON)
	}
	v, _ := eng.Vault()
	if _, gErr := v.GetSkill("pdf"); gErr == nil {
		t.Fatalf("skill should not be installed before approval")
	}

	// 7. Approved skill_install writes SKILL.md (sanitized), companion files (0644), and .fleet-skill.json.
	instOut, err := eng.executeTool(t.Context(), ToolCall{
		ID:       "s7",
		Type:     "function",
		Function: ToolCallFunction{Name: "skill_install", Arguments: aprErr.ArgsJSON},
	}, tctxApproved)
	if err != nil || !strings.Contains(instOut, `"status":"installed"`) {
		t.Fatalf("approved skill_install failed: %v (%s)", err, instOut)
	}
	sk, err := v.GetSkill("pdf")
	if err != nil {
		t.Fatalf("expected installed pdf skill: %v", err)
	}
	if sk.SyncToFleet || len(sk.Files) != 2 || sk.Source == nil || sk.Source.CommitSHA != commitSHA {
		t.Fatalf("unexpected installed skill metadata: %+v", sk)
	}
	scriptInfo, err := os.Stat(filepath.Join(v.Root, "_skills", "pdf", "scripts", "helper.py"))
	if err != nil || scriptInfo.Mode().Perm() != 0o644 {
		t.Fatalf("expected bundled script written non-executable 0644, got mode=%v err=%v", scriptInfo.Mode().Perm(), err)
	}

	// 8. Re-installing without overwrite returns 409 Conflict.
	if _, err := eng.executeTool(t.Context(), ToolCall{
		ID:       "s8",
		Type:     "function",
		Function: ToolCallFunction{Name: "skill_install", Arguments: `{"source":"anthropics/skills@pdf"}`},
	}, tctxChat); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected conflict when overwrite=false, got %v", err)
	}

	// 9. skill_read reads companion files and rejects traversal.
	readOut, err := eng.executeTool(t.Context(), ToolCall{
		ID:       "s9",
		Type:     "function",
		Function: ToolCallFunction{Name: "skill_read", Arguments: `{"name":"pdf","file":"REFERENCE.md"}`},
	}, tctxChat)
	if err != nil || !strings.Contains(readOut, "Detailed PDF notes.") {
		t.Fatalf("skill_read companion file failed: %v (%s)", err, readOut)
	}
	if _, err := eng.executeTool(t.Context(), ToolCall{
		ID:       "s10",
		Type:     "function",
		Function: ToolCallFunction{Name: "skill_read", Arguments: `{"name":"pdf","file":"../../etc/passwd"}`},
	}, tctxChat); err == nil {
		t.Fatalf("expected path traversal rejection in skill_read")
	}

	// 10. skill_remove requires direct chat + HITL approval and deletes entire bundle dir.
	if _, err := eng.executeTool(t.Context(), ToolCall{
		ID:       "s11",
		Type:     "function",
		Function: ToolCallFunction{Name: "skill_remove", Arguments: `{"name":"pdf"}`},
	}, tctxHeartbeat); err == nil {
		t.Fatalf("expected heartbeat skill_remove to be forbidden")
	}
	_, err = eng.executeTool(t.Context(), ToolCall{
		ID:       "s12",
		Type:     "function",
		Function: ToolCallFunction{Name: "skill_remove", Arguments: `{"name":"pdf"}`},
	}, tctxChat)
	rmApr, ok := errors.AsType[*approvalRequiredError](err)
	if !ok || rmApr.RiskLevel != "skill_remove" {
		t.Fatalf("expected approvalRequiredError(skill_remove), got %v", err)
	}
	rmOut, err := eng.executeTool(t.Context(), ToolCall{
		ID:       "s13",
		Type:     "function",
		Function: ToolCallFunction{Name: "skill_remove", Arguments: rmApr.ArgsJSON},
	}, tctxApproved)
	if err != nil || !strings.Contains(rmOut, `"status":"removed"`) {
		t.Fatalf("approved skill_remove failed: %v (%s)", err, rmOut)
	}
	if _, statErr := os.Stat(filepath.Join(v.Root, "_skills", "pdf")); !os.IsNotExist(statErr) {
		t.Fatalf("expected _skills/pdf directory completely removed, got err=%v", statErr)
	}
}
