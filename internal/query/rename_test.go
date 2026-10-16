package query_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/lr00rl/ccli/internal/query"
)

// A name given in ccli shows in lists, in the detail and in links, in every
// account the session is in; search finds the session by either title; and
// clearing the name brings the tool's own title back. The session's activity
// does not change.
func TestRename(t *testing.T) {
	fx := setup(t)
	ctx := context.Background()
	rows := func(f query.Filter) []query.SessionRow {
		t.Helper()
		page, err := fx.svc.Sessions(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		var out []query.SessionRow
		for _, s := range page.Sessions {
			if s.ID == lead {
				out = append(out, s)
			}
		}
		return out
	}
	before := rows(query.Filter{})

	r, err := fx.svc.Rename(ctx, "default:aaaa", "  Payments\n  audit ", base.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if r.ID != lead || r.Title != "Payments audit" || r.ToolTitle != "Build the thing" || r.Cleared {
		t.Fatalf("result %+v", r)
	}
	after := rows(query.Filter{})
	if len(after) != 2 {
		t.Fatalf("%d copies listed, want default's and work's", len(after))
	}
	for i, s := range after {
		if s.Title != "Payments audit" || s.TitleSource != "ccli" || !s.Last.Equal(before[i].Last) {
			t.Fatalf("%s copy: %q from %s, last %v (was %v)", s.Account, s.Title, s.TitleSource, s.Last, before[i].Last)
		}
	}
	for _, q := range []string{"payments", "build the thing"} {
		if n := len(rows(query.Filter{Search: q, Accounts: []string{"default"}})); n != 1 {
			t.Fatalf("search %q found %d", q, n)
		}
	}
	det, err := fx.svc.Session(ctx, "default:aaaa")
	if err != nil || det.Title != "Payments audit" || det.ToolTitle != "Build the thing" {
		t.Fatalf("detail: %v %q, own %q", err, det.Title, det.ToolTitle)
	}
	cont, err := fx.svc.Session(ctx, "bbbb")
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range cont.Links {
		if l.SessionID == lead && l.Title != "Payments audit" {
			t.Fatalf("link to the renamed session titled %q", l.Title)
		}
	}

	if _, err := fx.svc.Rename(ctx, "default:aaaa", strings.Repeat("x", query.MaxTitle+1), base); err == nil {
		t.Fatal("a title over the limit was taken")
	}
	r, err = fx.svc.Rename(ctx, "work:aaaa", "", base.Add(25*time.Hour))
	if err != nil || !r.Cleared || r.Title != "build the thing" || r.ToolTitle != "" {
		t.Fatalf("clearing from the copy: %v %+v", err, r)
	}
	if got := rows(query.Filter{Accounts: []string{"default"}}); got[0].Title != "Build the thing" || got[0].TitleSource == "ccli" {
		t.Fatalf("after clearing: %q from %s", got[0].Title, got[0].TitleSource)
	}
}
