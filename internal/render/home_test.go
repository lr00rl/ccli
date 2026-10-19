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
