package render

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/lr00rl/ccli/internal/account"
	"github.com/lr00rl/ccli/internal/cursoredits"
	"github.com/lr00rl/ccli/internal/cursoride"
	"github.com/lr00rl/ccli/internal/query"
)

func TestSessionShowsCursorEditsAndComposer(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	pct := 45.87
	var files []cursoredits.File
	for i := range 12 {
		files = append(files, cursoredits.File{Path: fmt.Sprintf("/w/app/f%02d.go", i), Models: []string{"grok-4.7"}, Last: now})
	}
	d := &query.SessionDetail{
		SessionRow: query.SessionRow{Account: "cursor-ide", Tool: account.CursorIDE, ID: "be14583f-885d-41f9-bf9c-10803fc53112", Cwd: "/w/app", Started: now, Last: now},
		Edits:      &cursoredits.Edits{Files: files, Models: []string{"grok-4.7"}},
		Composer:   &cursoride.Composer{ContextPercent: &pct},
	}
	show := func(all bool) string {
		var b bytes.Buffer
		Term{W: &b, Width: 120, Now: now, Loc: time.UTC}.Session(d, all)
		return b.String()
	}
	out := show(false)
	for _, want := range []string{"46% of the context window in use", "12 files by grok-4.7", "f00.go", "+2 more: ccli show cursor-ide:be14583f --all"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "changes") || strings.Contains(out, "/w/app/") || strings.Contains(out, "f10.go") {
		t.Fatalf("zero changes shown, path not relative to the project, or more than ten files:\n%s", out)
	}
	if out := show(true); !strings.Contains(out, "f11.go") || strings.Contains(out, "more:") {
		t.Fatalf("--all did not list every file:\n%s", out)
	}
	d.Composer.LinesAdded, d.Composer.LinesRemoved, d.Composer.FilesChanged = 95, 1, 3
	if out := show(false); !strings.Contains(out, "+95 -1 lines in 3 files") {
		t.Fatalf("changes missing:\n%s", out)
	}
}
