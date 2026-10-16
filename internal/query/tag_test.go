package query_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/lr00rl/ccli/internal/query"
)

// A tag shows next to the session's own title in lists, in the detail and
// in links, in every account the session is in; search finds the session by
// its tag or its title; and clearing the tag leaves the title as it was. The
// session's activity does not change.
func TestTag(t *testing.T) {
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

	r, err := fx.svc.Tag(ctx, "default:aaaa", "  Payments\n  audit ", base.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if r.ID != lead || r.Tag != "Payments audit" || r.Title != "Build the thing" || r.Cleared {
		t.Fatalf("result %+v", r)
	}
	after := rows(query.Filter{})
	if len(after) != 2 {
		t.Fatalf("%d copies listed, want default's and work's", len(after))
	}
	for i, s := range after {
		if s.Tag != "Payments audit" || s.Title != before[i].Title || s.TitleSource != before[i].TitleSource || !s.Last.Equal(before[i].Last) {
			t.Fatalf("%s copy: tag %q, title %q from %s, last %v (was %q from %s, %v)", s.Account, s.Tag, s.Title, s.TitleSource,
				s.Last, before[i].Title, before[i].TitleSource, before[i].Last)
		}
	}
	for _, q := range []string{"payments", "build the thing"} {
		if n := len(rows(query.Filter{Search: q, Accounts: []string{"default"}})); n != 1 {
			t.Fatalf("search %q found %d", q, n)
		}
	}
	det, err := fx.svc.Session(ctx, "default:aaaa")
	if err != nil || det.Tag != "Payments audit" || det.Title != "Build the thing" {
		t.Fatalf("detail: %v tag %q, title %q", err, det.Tag, det.Title)
	}
	cont, err := fx.svc.Session(ctx, "bbbb")
	if err != nil {
		t.Fatal(err)
	}
	linked := false
	for _, l := range cont.Links {
		if l.SessionID == lead {
			linked = true
			if l.Tag != "Payments audit" || l.Title == "Payments audit" {
				t.Fatalf("link to the tagged session: tag %q, title %q", l.Tag, l.Title)
			}
		}
	}
	if !linked {
		t.Fatal("no link to the tagged session")
	}

	if _, err := fx.svc.Tag(ctx, "default:aaaa", strings.Repeat("x", query.MaxTag+1), base); err == nil {
		t.Fatal("a tag over the limit was taken")
	}
	r, err = fx.svc.Tag(ctx, "work:aaaa", "", base.Add(25*time.Hour))
	if err != nil || !r.Cleared || r.Tag != "" || r.Title != "build the thing" {
		t.Fatalf("clearing from the copy: %v %+v", err, r)
	}
	for _, s := range rows(query.Filter{}) {
		if s.Tag != "" {
			t.Fatalf("%s copy still tagged %q", s.Account, s.Tag)
		}
	}
}
