package query_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lr00rl/ccli/internal/account"
	"github.com/lr00rl/ccli/internal/index"
	"github.com/lr00rl/ccli/internal/pricing"
	"github.com/lr00rl/ccli/internal/query"
	"github.com/lr00rl/ccli/internal/store"
)

const (
	lead  = "aaaaaaaa-0000-4000-8000-000000000001"
	next  = "bbbbbbbb-0000-4000-8000-000000000002"
	mate  = "cccccccc-0000-4000-8000-000000000003"
	other = "dddddddd-0000-4000-8000-000000000004"
)

var base = time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)

func at(min float64) string {
	return base.Add(time.Duration(min * float64(time.Minute))).Format(time.RFC3339Nano)
}

func asst(min float64, id, model string, in, out int64, extra string) string {
	return fmt.Sprintf(`{"type":"assistant","timestamp":%q,"cwd":"/work/alpha","message":{"id":%q,"model":%q,"usage":{"input_tokens":%d,"output_tokens":%d},"content":[%s]}}`+"\n",
		at(min), id, model, in, out, extra)
}

func toolUse(id, name, input string) string {
	return fmt.Sprintf(`{"type":"tool_use","id":%q,"name":%q,"input":%s}`, id, name, input)
}

func prompt(min float64, text string) string {
	return fmt.Sprintf(`{"type":"user","timestamp":%q,"cwd":"/work/alpha","message":{"content":%q}}`+"\n", at(min), text)
}

type fixture struct {
	t     *testing.T
	accts []account.Account
	st    *store.Store
	svc   *query.DB
}

func setup(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root) // the default account is ~/.claude, as on a real machine
	mk := func(name, dir string) account.Account {
		os.MkdirAll(filepath.Join(dir, "projects"), 0o755)
		return account.Account{Name: name, Dir: dir}
	}
	fx := &fixture{t: t, accts: []account.Account{
		mk("default", filepath.Join(root, ".claude")),
		mk("work", filepath.Join(root, ".claude-work")),
	}}
	w := func(acct int, rel, content string) {
		p := filepath.Join(fx.accts[acct].Dir, "projects", rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The lead session: a title, two Opus calls (one spawning a subagent),
	// a skill, a continuation into `next`.
	w(0, "-work-alpha/"+lead+".jsonl",
		prompt(0, "build the thing")+
			`{"type":"ai-title","aiTitle":"Build the thing"}`+"\n"+
			asst(1, "m1", "claude-opus-5-5[1m]", 1_000_000, 100, toolUse("tu_spawn", "Agent", `{"description":"review"}`))+
			asst(2, "m2", "claude-opus-5-5", 0, 100, toolUse("tu_skill", "Skill", `{"skill":"humanizer"}`))+
			`{"type":"continued-in","continuedInSessionId":"`+next+`"}`+"\n")
	// Subagent spawned by the lead, which itself spawns a nested subagent.
	w(0, "-work-alpha/"+lead+"/subagents/agent-a1.jsonl", asst(1.5, "s1", "claude-sonnet-5-5", 10, 10, toolUse("tu_nested", "Agent", `{}`)))
	w(0, "-work-alpha/"+lead+"/subagents/agent-a1.meta.json", `{"agentType":"code-reviewer","description":"review","toolUseId":"tu_spawn","spawnDepth":1}`)
	w(0, "-work-alpha/"+lead+"/subagents/agent-a2.jsonl", asst(1.7, "s2", "claude-haiku-4-5-20251001", 5, 5, ""))
	w(0, "-work-alpha/"+lead+"/subagents/agent-a2.meta.json", `{"agentType":"Explore","toolUseId":"tu_nested","spawnDepth":2}`)
	// The continuation, with a custom title.
	w(0, "-work-alpha/"+next+".jsonl", prompt(60, "keep going")+`{"type":"custom-title","customTitle":"Part two"}`+"\n"+asst(61, "n1", "claude-opus-5", 10, 10, ""))
	// A teammate of the lead.
	w(0, "-work-beta/"+mate+".jsonl", strings.Replace(asst(5, "t1", "claude-opus-5-5", 10, 10, ""), `"type":"assistant"`, `"type":"assistant","teamName":"session-aaaaaaaa","agentName":"tester"`, 1))
	// A copy of the lead in work, plus work's own session. The file created
	// first owns a shared response, and birth times are kept in
	// milliseconds, so the copy must be born in a later one.
	time.Sleep(2 * time.Millisecond)
	w(1, "-work-alpha/"+lead+".jsonl", prompt(0, "build the thing")+asst(1, "m1", "claude-opus-5-5", 1_000_000, 100, ""))
	w(1, "-work-gamma/"+other+".jsonl", prompt(120, "other work")+asst(121, "o1", "claude-opus-5-5", 2_000_000, 0, ""))

	st, err := store.Open(filepath.Join(root, "ccli.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	prices, _ := pricing.Load("")
	if _, err := index.Run(context.Background(), st, fx.accts, prices, index.Options{Workers: 2}); err != nil {
		t.Fatal(err)
	}
	fx.st = st
	fx.svc = query.New(st, fx.accts)
	return fx
}

func TestSessionsFiltersAndTitles(t *testing.T) {
	fx := setup(t)
	ctx := context.Background()
	page, err := fx.svc.Sessions(ctx, query.Filter{Sort: "cost"})
	if err != nil {
		t.Fatal(err)
	}
	if page.Matched != 5 {
		t.Fatalf("matched %d sessions, want 5", page.Matched)
	}
	first := page.Sessions[0]
	if first.ID != other || first.Account != "work" {
		t.Fatalf("costliest = %s/%s", first.Account, first.ID)
	}
	byID := map[string]query.SessionRow{}
	for _, s := range page.Sessions {
		byID[s.Account+"/"+s.ID] = s
	}
	l := byID["default/"+lead]
	if l.Title != "Build the thing" || l.TitleSource != "ai" || l.Subagents != 2 || l.Prompts != 1 {
		t.Fatalf("lead row = %+v", l)
	}
	if len(l.Models) == 0 || l.Models[0] != "claude-opus-5-5" {
		t.Fatalf("models = %v (the [1m] variant should merge, costliest first)", l.Models)
	}
	if n := byID["default/"+next]; n.Title != "Part two" || n.TitleSource != "custom" {
		t.Fatalf("custom title: %+v", n)
	}
	if m := byID["default/"+mate]; m.Team != "session-aaaaaaaa" || m.AgentName != "tester" {
		t.Fatalf("teammate row = %+v", m)
	}
	// The copied history belongs to the default account.
	if c := byID["work/"+lead]; c.Tokens.Input != 0 {
		t.Fatalf("copy owns %d input tokens", c.Tokens.Input)
	}
	for _, tc := range []struct {
		f    query.Filter
		want int
	}{
		{query.Filter{Accounts: []string{"work"}}, 2},
		{query.Filter{Project: "beta"}, 1},
		{query.Filter{Search: "part"}, 1},
		{query.Filter{Search: "dddd"}, 1},
		{query.Filter{Model: "sonnet"}, 1},
		{query.Filter{Since: base.Add(90 * time.Minute)}, 1},
	} {
		p, err := fx.svc.Sessions(ctx, tc.f)
		if err != nil {
			t.Fatal(err)
		}
		if p.Matched != tc.want {
			t.Errorf("filter %+v matched %d, want %d", tc.f, p.Matched, tc.want)
		}
	}
	p, _ := fx.svc.Sessions(ctx, query.Filter{Limit: 2})
	if len(p.Sessions) != 2 || p.Matched != 5 {
		t.Fatalf("limit: %d shown of %d", len(p.Sessions), p.Matched)
	}
}

func TestSessionDetailTreeAndLinks(t *testing.T) {
	fx := setup(t)
	ctx := context.Background()
	d, err := fx.svc.Session(ctx, "default:aaaaaaaa")
	if err != nil {
		t.Fatal(err)
	}
	parents := map[string]string{}
	for _, f := range d.Files {
		if f.Kind == "subagent" {
			parents[f.AgentID] = f.Parent
		}
	}
	if parents["a1"] != "" || parents["a2"] != "a1" {
		t.Fatalf("parents = %v (a1 spawned by main, a2 by a1)", parents)
	}
	links := map[string]string{}
	for _, l := range d.Links {
		links[l.Kind+"/"+l.SessionID] = l.Account + " " + l.Detail
	}
	for _, want := range []string{"continues-in/" + next, "teammate/" + mate, "copy/" + lead} {
		if _, ok := links[want]; !ok {
			t.Errorf("missing link %s in %v", want, links)
		}
	}
	if !strings.Contains(links["teammate/"+mate], "tester") {
		t.Errorf("teammate name missing: %v", links)
	}
	if len(d.Skills) != 1 || d.Skills[0].Name != "humanizer" {
		t.Errorf("skills = %v", d.Skills)
	}
	if !d.Resume.Available || !strings.Contains(d.Resume.Command, "claude --resume "+lead) || strings.Contains(d.Resume.Command, "CLAUDE_CONFIG_DIR") {
		t.Errorf("resume = %+v", d.Resume)
	}
	// The continuation knows where it came from; the teammate knows its lead.
	n, _ := fx.svc.Session(ctx, next[:8])
	if len(n.Links) == 0 || n.Links[0].Kind != "continued-from" {
		t.Errorf("continuation links = %+v", n.Links)
	}
	m, _ := fx.svc.Session(ctx, mate[:8])
	found := false
	for _, l := range m.Links {
		if l.Kind == "team-lead" && l.SessionID == lead {
			found = true
		}
	}
	if !found {
		t.Errorf("teammate links = %+v", m.Links)
	}
	// A work session resumes with its config dir.
	o, _ := fx.svc.Session(ctx, other[:8])
	if !strings.Contains(o.Resume.Command, "CLAUDE_CONFIG_DIR=") || !strings.HasPrefix(o.Resume.Command, "cd /work/alpha") {
		t.Errorf("work resume = %q", o.Resume.Command)
	}
}

func TestResolve(t *testing.T) {
	fx := setup(t)
	ctx := context.Background()
	if _, _, err := fx.svc.Resolve(ctx, "abc"); err == nil {
		t.Error("short prefix accepted")
	}
	if _, _, err := fx.svc.Resolve(ctx, "eeeeeeee"); err == nil {
		t.Error("unknown prefix accepted")
	}
	acct, id, err := fx.svc.Resolve(ctx, "work:aaaa")
	if err != nil || acct != "work" || id != lead {
		t.Errorf("work:aaaa -> %s %s %v", acct, id, err)
	}
	// Without an account, the copy with the transcript and latest activity wins.
	if acct, _, err := fx.svc.Resolve(ctx, "aaaa"); err != nil || acct == "" {
		t.Errorf("aaaa -> %s %v", acct, err)
	}
}

func TestResolveUniqueRequiresAccountForCopies(t *testing.T) {
	fx := setup(t)
	ctx := context.Background()
	if acct, id, err := fx.svc.ResolveUnique(ctx, "work:aaaa"); err != nil || acct != "work" || id != lead {
		t.Fatalf("work:aaaa -> %s %s %v", acct, id, err)
	}
	var copies *query.AmbiguousCopies
	if _, _, err := fx.svc.ResolveUnique(ctx, "aaaa"); !errors.As(err, &copies) ||
		copies.ID != lead || !slices.Equal(copies.Accounts, []string{"default", "work"}) {
		t.Fatalf("ambiguous copy: %v", err)
	}
	if acct, id, err := fx.svc.ResolveUnique(ctx, "bbbb"); err != nil || acct != "default" || id != next {
		t.Fatalf("unique prefix -> %s %s %v", acct, id, err)
	}
}

func TestSessionCopiesUsesSessionRows(t *testing.T) {
	fx := setup(t)
	page, err := fx.svc.SessionCopies(context.Background(), lead)
	if err != nil {
		t.Fatal(err)
	}
	if page.Matched != 2 || len(page.Sessions) != 2 {
		t.Fatalf("copies: %+v", page)
	}
	byAccount := map[string]query.SessionRow{}
	for _, row := range page.Sessions {
		byAccount[row.Account] = row
		if row.ID != lead || row.Title == "" || row.Project == "" || row.Last.IsZero() {
			t.Fatalf("incomplete session row: %+v", row)
		}
	}
	if byAccount["default"].Tokens.Input == 0 || len(byAccount["default"].Models) == 0 || byAccount["work"].Tokens.Input != 0 {
		t.Fatalf("copy usage: default=%+v work=%+v", byAccount["default"].Tokens, byAccount["work"].Tokens)
	}
}

func TestOverview(t *testing.T) {
	fx := setup(t)
	o, err := fx.svc.Overview(context.Background(), query.Filter{Loc: time.UTC})
	if err != nil {
		t.Fatal(err)
	}
	if o.Sessions != 5 {
		t.Errorf("sessions = %d", o.Sessions)
	}
	if o.Tokens.Input != 3_000_035 {
		t.Errorf("input = %d", o.Tokens.Input)
	}
	models := map[string]bool{}
	for _, m := range o.ByModel {
		models[m.Name] = true
	}
	if !models["claude-opus-5-5"] || !models["claude-haiku-4-5"] || models["claude-opus-5-5[1m]"] {
		t.Errorf("models = %v", models)
	}
	if len(o.Daily) == 0 || o.Daily[0].Date != "2026-10-01" || o.Daily[0].Tokens.Input != 3_000_035 {
		t.Errorf("daily = %+v", o.Daily)
	}
	if o.ActiveMS == 0 || o.Heatmap[3][9] != 180_000 { // 2026-10-01 is a Thursday: 2 min (lead) + 1 min (work copy) at 09h
		t.Errorf("active = %d, heatmap thu 09 = %d", o.ActiveMS, o.Heatmap[3][9])
	}
	if len(o.Skills) != 1 || o.Skills[0].Uses != 1 {
		t.Errorf("skills = %+v", o.Skills)
	}
	acc, err := fx.svc.Accounts(context.Background(), query.Filter{})
	if err != nil || len(acc) != 2 || acc[1].Sessions != 2 {
		t.Fatalf("accounts = %+v %v", acc, err)
	}
}

func TestResolveIsLiteral(t *testing.T) {
	fx := setup(t)
	for _, ref := range []string{"____", "%%%%", "a_a_"} {
		if _, _, err := fx.svc.Resolve(context.Background(), ref); err == nil {
			t.Errorf("%q matched a session", ref)
		}
	}
}

func TestShortID(t *testing.T) {
	if query.ShortID("x") != "x" || query.ShortID("0123456789") != "01234567" {
		t.Fatal("ShortID")
	}
}

// A session copied between accounts has the same id in each. A process runs
// one copy, so only that account's row is live, and the copy names the
// account it came from.
func TestCopiedSessionLiveAndSource(t *testing.T) {
	fx := setup(t)
	ctx := context.Background()
	work := fx.accts[1]
	os.MkdirAll(filepath.Join(work.Dir, "sessions"), 0o755)
	reg := fmt.Sprintf(`{"sessionId":%q,"status":"busy","cwd":"/work/alpha"}`, lead)
	if err := os.WriteFile(filepath.Join(work.Dir, "sessions", fmt.Sprintf("%d.json", os.Getpid())), []byte(reg), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := fx.st.RecordTransfers(ctx, []store.Transfer{{At: base, Mode: "copy", SessionID: lead,
		FromAccount: "default", ToAccount: "work", Path: filepath.Join(work.Dir, "projects", "-work-alpha", lead+".jsonl"), To: 10}}); err != nil {
		t.Fatal(err)
	}
	page, err := query.New(fx.st, fx.accts).Sessions(ctx, query.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]query.SessionRow{}
	for _, s := range page.Sessions {
		byID[s.Account+"/"+s.ID] = s
	}
	if s := byID["work/"+lead]; !s.Live || s.CopiedFrom != "default" {
		t.Fatalf("work copy: live=%v copied_from=%q, want live and from default", s.Live, s.CopiedFrom)
	}
	if s := byID["default/"+lead]; s.Live || s.CopiedFrom != "" {
		t.Fatalf("default original: live=%v copied_from=%q, want neither", s.Live, s.CopiedFrom)
	}
}
