package render

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/lr00rl/ccli/internal/query"
)

func TestShortHome(t *testing.T) {
	t.Setenv("HOME", "/Users/al")
	for in, want := range map[string]string{
		"/Users/al":           "~",
		"/Users/al/code/app":  "~/code/app",
		"/Users/alex/code":    "/Users/alex/code", // shares the prefix, not the folder
		"/private/tmp/x":      "/private/tmp/x",
		"relative/Users/al/x": "relative/Users/al/x",
	} {
		if got := ShortHome(in); got != want {
			t.Errorf("ShortHome(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEmptySessionsNamesTheWindow(t *testing.T) {
	var b bytes.Buffer
	since := time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC)
	Term{W: &b, Width: 80, Now: since.AddDate(0, 0, 30), Loc: time.UTC}.Sessions(query.SessionPage{
		Filter: query.Filter{Since: since},
		Older:  11,
	}, false)
	if !strings.Contains(b.String(), "--days 0") || !strings.Contains(b.String(), "11 older") {
		t.Fatalf("empty list: %q", b.String())
	}
}

// The running mark stands apart from the id, so a double-click in the
// terminal selects the id alone, and a copy names the account it came from.
func TestSessionRowMarksAndCopySource(t *testing.T) {
	var b bytes.Buffer
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	id := "1087b979-d8ea-4833-8f31-8baa6290d05e"
	Term{W: &b, Width: 160, Now: now, Loc: time.UTC}.Sessions(query.SessionPage{Matched: 2, Sessions: []query.SessionRow{
		{Account: "talcum", ID: id, Title: "t", Last: now, Live: true, CopiedFrom: "default"},
		{Account: "default", ID: id, Title: "t", Last: now.Add(-time.Hour)},
	}}, true)
	out := b.String()
	for _, want := range []string{"1087b979 ●", "talcum ← default", "← copied from that account"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "1087b979●") {
		t.Fatalf("mark is glued to the id:\n%s", out)
	}
}
