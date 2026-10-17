package query_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/lr00rl/ccli/internal/account"
	"github.com/lr00rl/ccli/internal/index"
	"github.com/lr00rl/ccli/internal/pricing"
	"github.com/lr00rl/ccli/internal/query"
	"github.com/lr00rl/ccli/internal/store"
)

const (
	pjApp    = "e0000000-0000-4000-8000-00000000000a" // /code/app
	pjTree   = "e0000000-0000-4000-8000-00000000000b" // /code/app/.claude/worktrees/w1
	pjApp2   = "e0000000-0000-4000-8000-00000000000c" // /code/app2
	pjOther  = "e0000000-0000-4000-8000-00000000000d" // /other
	pjLinked = "e0000000-0000-4000-8000-00000000000e" // /link/app/sub, another form of /code/app/sub
	pjCodex  = "01a00000-0000-7000-b000-00000000c001" // Codex, /code/app/sub
)

// inDir moves transcript lines built by prompt and asst (which record
// /work/alpha) to another cwd.
func inDir(cwd, lines string) string {
	return strings.ReplaceAll(lines, "/work/alpha", cwd)
}

func codexAt(sec int, typ, payload string) string {
	return fmt.Sprintf(`{"timestamp":"2026-10-01T10:00:%02d.000Z","type":%q,"payload":%s}`+"\n", sec, typ, payload)
}

// codexRollout is a one-turn Codex thread started in cwd.
func codexRollout(id, cwd, text string, input int) string {
	tok := fmt.Sprintf(`{"input_tokens":%d,"cached_input_tokens":0,"output_tokens":10,"reasoning_output_tokens":0,"total_tokens":%d}`, input, input+10)
	return codexAt(0, "session_meta", fmt.Sprintf(`{"id":%q,"timestamp":"2026-10-01T10:00:00.000Z","cwd":%q,"cli_version":"0.150.1","source":"cli"}`, id, cwd)) +
		codexAt(1, "event_msg", `{"type":"task_started","turn_id":"t"}`) +
		codexAt(1, "turn_context", fmt.Sprintf(`{"model":"gpt-5.5","cwd":%q}`, cwd)) +
		codexAt(1, "event_msg", fmt.Sprintf(`{"type":"user_message","message":%q}`, text)) +
		codexAt(3, "event_msg", fmt.Sprintf(`{"type":"token_count","info":{"total_token_usage":%s,"last_token_usage":%s}}`, tok, tok)) +
		codexAt(4, "event_msg", `{"type":"task_complete","duration_ms":3000}`)
}

// setupProjects indexes five Claude Code sessions and one Codex thread
// started in different folders; /link/app stands for a symlink to /code/app.
func setupProjects(t *testing.T) *query.DB {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	claude := account.Account{Name: "default", Dir: filepath.Join(root, ".claude")}
	codex := account.Account{Name: "codex", Tool: account.Codex, Dir: filepath.Join(root, ".codex")}
	write := func(p, content string) {
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for i, s := range []struct{ id, cwd, text string }{
		{pjApp, "/code/app", "app prompt"},
		{pjTree, "/code/app/.claude/worktrees/w1", "worktree prompt"},
		{pjApp2, "/code/app2", "app2 prompt"},
		{pjOther, "/other", "other prompt"},
		{pjLinked, "/link/app/sub", "linked prompt"},
	} {
		min := float64(10 * i)
		key := strings.ReplaceAll(s.cwd, "/", "-")
		write(filepath.Join(claude.Dir, "projects", key, s.id+".jsonl"),
			inDir(s.cwd, prompt(min, s.text)+asst(min+1, "msg-"+s.id[:8]+s.id[34:], "claude-opus-5-5", 100, 10, "")))
	}
	write(filepath.Join(codex.Dir, "sessions", "2026", "10", "01", "rollout-2026-10-01T10-00-00-"+pjCodex+".jsonl"),
		codexRollout(pjCodex, "/code/app/sub", "codex prompt", 200))

	st, err := store.Open(filepath.Join(root, "ccli.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	accts := []account.Account{claude, codex}
	prices, _ := pricing.Load("")
	if _, err := index.Run(context.Background(), st, accts, prices, index.Options{Workers: 2}); err != nil {
		t.Fatal(err)
	}
	return query.New(st, accts)
}

func sessionIDs(t *testing.T, db *query.DB, f query.Filter) []string {
	t.Helper()
	page, err := db.Sessions(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, s := range page.Sessions {
		ids = append(ids, s.ID)
	}
	if page.Matched != len(ids) {
		t.Fatalf("filter %+v matched %d but listed %d", f, page.Matched, len(ids))
	}
	sort.Strings(ids)
	return ids
}

func sorted(ids ...string) []string {
	out := append([]string(nil), ids...)
	sort.Strings(out)
	return out
}

func TestSessionsDirsAndTools(t *testing.T) {
	db := setupProjects(t)
	if got := sessionIDs(t, db, query.Filter{}); len(got) != 6 {
		t.Fatalf("indexed %d sessions, want 6: %v", len(got), got)
	}
	for _, c := range []struct {
		name string
		f    query.Filter
		want []string
	}{
		{"dir", query.Filter{Dirs: []string{"/code/app"}}, sorted(pjApp, pjTree, pjCodex)},
		{"dir with a trailing slash", query.Filter{Dirs: []string{"/code/app/"}}, sorted(pjApp, pjTree, pjCodex)},
		{"sibling dir", query.Filter{Dirs: []string{"/code/app2"}}, []string{pjApp2}},
		{"two dirs", query.Filter{Dirs: []string{"/code/app2", "/other"}}, sorted(pjApp2, pjOther)},
		{"no such dir", query.Filter{Dirs: []string{"/nowhere"}}, nil},
		{"codex", query.Filter{Tools: []account.Tool{account.Codex}}, []string{pjCodex}},
		{"claude", query.Filter{Tools: []account.Tool{account.Claude}}, sorted(pjApp, pjTree, pjApp2, pjOther, pjLinked)},
		{"claude under dir", query.Filter{Tools: []account.Tool{account.Claude}, Dirs: []string{"/code/app"}}, sorted(pjApp, pjTree)},
		{"both tools", query.Filter{Tools: []account.Tool{account.Claude, account.Codex}, Dirs: []string{"/code"}}, sorted(pjApp, pjTree, pjApp2, pjCodex)},
	} {
		if got := sessionIDs(t, db, c.f); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
	page, _ := db.Sessions(context.Background(), query.Filter{Tools: []account.Tool{account.Codex}})
	if s := page.Sessions[0]; s.Tool != account.Codex || s.Account != "codex" || s.Cwd != "/code/app/sub" {
		t.Errorf("codex row = %s %s %q", s.Tool, s.Account, s.Cwd)
	}
}

func projectsBy(t *testing.T, db *query.DB, f query.Filter) (query.ProjectPage, map[string]query.ProjectRow) {
	t.Helper()
	page, err := db.Projects(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	byRel := map[string]query.ProjectRow{}
	for _, p := range page.Projects {
		if _, dup := byRel[p.Rel]; dup {
			t.Fatalf("two rows with rel %q: %+v", p.Rel, page.Projects)
		}
		byRel[p.Rel] = p
	}
	return page, byRel
}

func rels(ps []query.ProjectRow) []string {
	var out []string
	for _, p := range ps {
		out = append(out, p.Rel)
	}
	return out
}

func TestProjectsEachFolder(t *testing.T) {
	db := setupProjects(t)
	page, by := projectsBy(t, db, query.Filter{Dirs: []string{"/code"}})
	want := []string{"app", "app/.claude/worktrees/w1", "app/sub", "app2"}
	if got := sorted(rels(page.Projects)...); !reflect.DeepEqual(got, want) {
		t.Fatalf("rels = %v, want %v", got, want)
	}
	if page.Matched != 4 || page.Sessions != 4 {
		t.Errorf("matched %d projects, %d sessions; want 4 and 4", page.Matched, page.Sessions)
	}
	for rel, p := range by {
		if p.Sessions != 1 || p.Prompts != 1 || p.Path != "/code/"+rel {
			t.Errorf("%s: %+v", rel, p)
		}
	}
	if a := by["app/sub"].Accounts; !reflect.DeepEqual(a, []string{"codex"}) {
		t.Errorf("app/sub accounts = %v", a)
	}
	if a := by["app"].Accounts; !reflect.DeepEqual(a, []string{"default"}) {
		t.Errorf("app accounts = %v", a)
	}
	var sum query.Tokens
	for _, p := range page.Projects {
		sum.Input += p.Tokens.Input
		sum.Output += p.Tokens.Output
	}
	if page.Tokens.Input != sum.Input || page.Tokens.Output != sum.Output {
		t.Errorf("page tokens %+v, rows add up to %+v", page.Tokens, sum)
	}
	if by["app"].Tokens.Input != 100 || by["app/sub"].Tokens.Input != 200 {
		t.Errorf("tokens: app %+v, app/sub %+v", by["app"].Tokens, by["app/sub"].Tokens)
	}
	// /other is outside /code.
	for _, p := range page.Projects {
		if p.Path == "/other" {
			t.Errorf("/other listed under /code")
		}
	}
}

func TestProjectsDepth(t *testing.T) {
	db := setupProjects(t)
	page, by := projectsBy(t, db, query.Filter{Dirs: []string{"/code"}, Depth: 1})
	if got := sorted(rels(page.Projects)...); !reflect.DeepEqual(got, []string{"app", "app2"}) {
		t.Fatalf("rels = %v, want [app app2]", got)
	}
	app := by["app"]
	if app.Path != "/code/app" || app.Sessions != 3 || app.Prompts != 3 || !reflect.DeepEqual(app.Accounts, []string{"codex", "default"}) {
		t.Errorf("app = %+v", app)
	}
	if app.Tokens.Input != 400 {
		t.Errorf("app input = %d, want 100 + 100 + 200", app.Tokens.Input)
	}
	if !app.First.Equal(base) || !app.Last.After(app.First) {
		t.Errorf("app first %v last %v, want first %v", app.First, app.Last, base)
	}
	if a2 := by["app2"]; a2.Path != "/code/app2" || a2.Sessions != 1 {
		t.Errorf("app2 = %+v", a2)
	}
	if page.Matched != 2 || page.Sessions != 4 {
		t.Errorf("matched %d projects, %d sessions; want 2 and 4", page.Matched, page.Sessions)
	}

	// Depth 2 under /code/app keeps the worktree folder's first two levels.
	_, by = projectsBy(t, db, query.Filter{Dirs: []string{"/code/app"}, Depth: 2})
	if w := by[".claude/worktrees"]; w.Path != "/code/app/.claude/worktrees" || w.Sessions != 1 {
		t.Errorf("depth 2 rows = %+v", by)
	}
	if by["."].Sessions != 1 || by["sub"].Sessions != 1 {
		t.Errorf("depth 2 rows = %+v", by)
	}
}

func TestProjectsTwoFormsOfOneDir(t *testing.T) {
	db := setupProjects(t)
	page, by := projectsBy(t, db, query.Filter{Dirs: []string{"/link/app", "/code/app"}})
	if page.Matched != 3 {
		t.Fatalf("rels = %v, want three folders under /code/app", rels(page.Projects))
	}
	// Rows are named in the first form given, whichever form a session
	// recorded, and a folder recorded in both forms is one row.
	if p, ok := by["."]; !ok || p.Path != "/link/app" || p.Sessions != 1 {
		t.Errorf(`"." row = %+v (found %v)`, p, ok)
	}
	if p := by[".claude/worktrees/w1"]; p.Path != "/link/app/.claude/worktrees/w1" {
		t.Errorf("worktree row = %+v", p)
	}
	if p := by["sub"]; p.Path != "/link/app/sub" || p.Sessions != 2 || !reflect.DeepEqual(p.Accounts, []string{"codex", "default"}) {
		t.Errorf("sub row = %+v, want both forms' sessions in one row", p)
	}
	_, by = projectsBy(t, db, query.Filter{Dirs: []string{"/code/app", "/link/app"}})
	if p := by["sub"]; p.Path != "/code/app/sub" || p.Sessions != 2 {
		t.Errorf("forms reversed: sub row = %+v", p)
	}
}

func TestProjectsSortAndLimit(t *testing.T) {
	db := setupProjects(t)
	page, _ := projectsBy(t, db, query.Filter{Dirs: []string{"/code"}, Sort: "path"})
	var paths []string
	for _, p := range page.Projects {
		paths = append(paths, p.Path)
	}
	want := []string{"/code/app", "/code/app/.claude/worktrees/w1", "/code/app/sub", "/code/app2"}
	if !reflect.DeepEqual(paths, want) {
		t.Errorf("sort path = %v, want %v", paths, want)
	}

	// The default sort is the latest activity first: the Codex thread ran last.
	page, _ = projectsBy(t, db, query.Filter{Dirs: []string{"/code"}})
	if page.Projects[0].Rel != "app/sub" {
		t.Errorf("default sort starts with %q, want app/sub", page.Projects[0].Rel)
	}

	page, _ = projectsBy(t, db, query.Filter{Dirs: []string{"/code"}, Sort: "path", Limit: 1})
	if len(page.Projects) != 1 || page.Matched != 4 {
		t.Errorf("limit 1: %d shown, matched %d; want 1 and 4", len(page.Projects), page.Matched)
	}
	if page.Projects[0].Path != "/code/app" {
		t.Errorf("limit 1 kept %q, want the first by path", page.Projects[0].Path)
	}
}

func TestPromptsAndOverviewDirs(t *testing.T) {
	db := setupProjects(t)
	ctx := context.Background()
	pp, err := db.Prompts(ctx, query.Filter{Dirs: []string{"/other"}})
	if err != nil {
		t.Fatal(err)
	}
	if pp.Matched != 1 || len(pp.Prompts) != 1 || pp.Prompts[0].SessionID != pjOther || pp.Prompts[0].Prompt != "other prompt" {
		t.Fatalf("prompts under /other = %+v", pp.Prompts)
	}
	if pp.Prompts[0].Project != "other" {
		t.Errorf("project = %q", pp.Prompts[0].Project)
	}
	pp, err = db.Prompts(ctx, query.Filter{Tools: []account.Tool{account.Codex}})
	if err != nil {
		t.Fatal(err)
	}
	if len(pp.Prompts) != 1 || pp.Prompts[0].SessionID != pjCodex || pp.Prompts[0].Prompt != "codex prompt" {
		t.Errorf("codex prompts = %+v", pp.Prompts)
	}
	all, err := db.Prompts(ctx, query.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if all.Matched != 6 {
		t.Errorf("all prompts = %d, want 6", all.Matched)
	}

	o, err := db.Overview(ctx, query.Filter{Dirs: []string{"/code/app"}})
	if err != nil {
		t.Fatal(err)
	}
	if o.Sessions != 3 {
		t.Errorf("overview under /code/app counts %d sessions, want 3", o.Sessions)
	}
	if o.Tokens.Input != 400 {
		t.Errorf("overview input = %d, want 400", o.Tokens.Input)
	}
}
