package query_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lr00rl/ccli/internal/index"
	"github.com/lr00rl/ccli/internal/pricing"
	"github.com/lr00rl/ccli/internal/query"
)

func ids(rows []query.SessionRow) map[string]int {
	out := map[string]int{}
	for _, r := range rows {
		out[r.ID]++
	}
	return out
}

func promptsOf(t *testing.T, svc *query.DB, show query.Visibility) map[string]int {
	t.Helper()
	p, err := svc.Prompts(context.Background(), query.Filter{Show: show})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int{}
	for _, r := range p.Prompts {
		out[r.SessionID]++
	}
	return out
}

// Hiding leaves a session out of lists and nothing else: totals, the
// session's own detail and the transcripts are as they were.
func TestHideLeavesListsNotTotals(t *testing.T) {
	fx := setup(t)
	ctx := context.Background()
	before, err := fx.svc.Overview(ctx, query.Filter{Loc: time.UTC})
	if err != nil {
		t.Fatal(err)
	}
	rs, err := fx.svc.Hide(ctx, []string{"dddd", "zzzz9999"}, base.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 2 || !rs[0].Done || rs[0].ID != other || rs[1].Done || rs[1].ID != "" || !strings.Contains(rs[1].Reason, "no session matches") {
		t.Fatalf("results %+v", rs)
	}

	page, err := fx.svc.Sessions(ctx, query.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if ids(page.Sessions)[other] != 0 || page.Hidden != 1 || page.Matched != 4 {
		t.Fatalf("visible list has %v, %d hidden, %d matched", ids(page.Sessions), page.Hidden, page.Matched)
	}
	page, err = fx.svc.Sessions(ctx, query.Filter{Show: query.HiddenOnly})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Sessions) != 1 || page.Sessions[0].ID != other || !page.Sessions[0].Hidden {
		t.Fatalf("hidden list %+v", page.Sessions)
	}
	if n := promptsOf(t, fx.svc, query.Visible)[other]; n != 0 {
		t.Fatalf("prompts list %d prompts of the hidden session", n)
	}
	if n := promptsOf(t, fx.svc, query.HiddenOnly)[other]; n != 1 {
		t.Fatalf("hidden prompts list has %d of its prompts, want 1", n)
	}

	after, err := fx.svc.Overview(ctx, query.Filter{Loc: time.UTC})
	if err != nil {
		t.Fatal(err)
	}
	if after.Sessions != before.Sessions || after.Tokens != before.Tokens || after.Cost != before.Cost {
		t.Fatalf("totals changed: %d sessions %+v -> %d sessions %+v", before.Sessions, before.Tokens, after.Sessions, after.Tokens)
	}
	det, err := fx.svc.Session(ctx, "dddd")
	if err != nil || !det.Hidden {
		t.Fatalf("detail of a hidden session: %v, hidden %v", err, det != nil && det.Hidden)
	}

	if rs, _ := fx.svc.Hide(ctx, []string{"dddd"}, base.Add(25*time.Hour)); rs[0].Done || rs[0].Reason != "already hidden" {
		t.Fatalf("hiding twice: %+v", rs)
	}
	if rs, _ := fx.svc.Unhide(ctx, []string{"dddd"}); !rs[0].Done {
		t.Fatalf("unhide: %+v", rs)
	}
	if rs, _ := fx.svc.Unhide(ctx, []string{"dddd"}); rs[0].Done || rs[0].Reason != "not hidden" {
		t.Fatalf("unhide twice: %+v", rs)
	}
	if page, _ := fx.svc.Sessions(ctx, query.Filter{}); ids(page.Sessions)[other] != 1 || page.Hidden != 0 {
		t.Fatalf("after unhide the list has %v, %d hidden", ids(page.Sessions), page.Hidden)
	}
}

// Hiding marks the session, so its copies in other accounts go with it.
func TestHideCoversCopies(t *testing.T) {
	fx := setup(t)
	ctx := context.Background()
	if _, err := fx.svc.Hide(ctx, []string{"work:aaaa"}, base.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	page, err := fx.svc.Sessions(ctx, query.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if n := ids(page.Sessions)[lead]; n != 0 || page.Hidden != 2 {
		t.Fatalf("%d copies of the hidden session listed, %d hidden", n, page.Hidden)
	}
}

// A hidden session that is used again shows again, without unhide.
func TestHiddenSessionReturnsWhenUsed(t *testing.T) {
	fx := setup(t)
	ctx := context.Background()
	if _, err := fx.svc.Hide(ctx, []string{"dddd"}, base.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(fx.accts[1].Dir, "projects", "-work-gamma", other+".jsonl")
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprint(f, prompt(240, "back to it")+asst(241, "o2", "claude-opus-5-5", 10, 10, ""))
	f.Close()
	prices, _ := pricing.Load("")
	if _, err := index.Run(ctx, fx.st, fx.accts, prices, index.Options{Workers: 1}); err != nil {
		t.Fatal(err)
	}
	page, err := fx.svc.Sessions(ctx, query.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if ids(page.Sessions)[other] != 1 || page.Hidden != 0 {
		t.Fatalf("used again after hiding, the list has %v and %d hidden", ids(page.Sessions), page.Hidden)
	}
}

// A session a process is running is left alone.
func TestHideRefusesRunningSession(t *testing.T) {
	fx := setup(t)
	dir := filepath.Join(fx.accts[0].Dir, "sessions")
	os.MkdirAll(dir, 0o755)
	pidFile := filepath.Join(dir, fmt.Sprintf("%d.json", os.Getpid()))
	if err := os.WriteFile(pidFile, []byte(`{"sessionId":"`+next+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	svc := query.New(fx.st, fx.accts)
	rs, err := svc.Hide(context.Background(), []string{"bbbb"}, base.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if rs[0].Done || rs[0].Reason == "" {
		t.Fatalf("hid a running session: %+v", rs)
	}
	if page, _ := svc.Sessions(context.Background(), query.Filter{}); ids(page.Sessions)[next] != 1 {
		t.Fatal("the running session left the list")
	}
}

// Review fixes: a transcript dated after the clock that hides it, a session
// named twice, and a bad reference among good ones.
func TestHideEdgeCases(t *testing.T) {
	fx := setup(t)
	ctx := context.Background()
	// The clock reads 09:00, before the session's last line at 11:01.
	rs, err := fx.svc.Hide(ctx, []string{"dddd", "dddd", "ab"}, base)
	if err != nil {
		t.Fatal(err)
	}
	if !rs[0].Done {
		t.Fatalf("first: %+v", rs[0])
	}
	if rs[1].Done || rs[1].Reason != "named more than once" {
		t.Fatalf("repeat: %+v", rs[1])
	}
	if rs[2].Done || rs[2].ID != "" || !strings.Contains(rs[2].Reason, "too short") {
		t.Fatalf("short reference: %+v", rs[2])
	}
	page, err := fx.svc.Sessions(ctx, query.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if ids(page.Sessions)[other] != 0 {
		t.Fatal("a session dated after the clock stayed listed after hiding")
	}
}
