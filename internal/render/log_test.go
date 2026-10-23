package render

import (
	"strings"
	"testing"
	"time"

	"github.com/lr00rl/ccli/internal/conv"
	"github.com/lr00rl/ccli/internal/query"
)

func TestConversationReplyDropsSteps(t *testing.T) {
	var b strings.Builder
	term := Term{W: &b, Width: 80, Loc: time.UTC}
	term.Conversation(&conv.Turn{Speaker: "Cursor", Entries: []conv.Entry{
		{Kind: conv.KindPrompt, Text: "filter the drafts"},
		{Kind: conv.KindText, Text: "Looking now."},
		{Kind: conv.KindTool, Call: &conv.Call{Name: "Grep", ID: "c1"}},
		{Kind: conv.KindText, Text: "They stay out of the list."},
	}}, LogOptions{Detail: "reply"}, nil)
	got := b.String()
	if !strings.Contains(got, "filter the drafts") || !strings.Contains(got, "They stay out of the list.") {
		t.Fatalf("reply missing:\n%s", got)
	}
	if strings.Contains(got, "Looking now.") || strings.Contains(got, "Grep") {
		t.Fatalf("step leaked:\n%s", got)
	}
}

func TestConversationDropsEscapes(t *testing.T) {
	var b strings.Builder
	term := Term{W: &b, Width: 80, Loc: time.UTC}
	term.Conversation(&conv.Turn{Speaker: "Cursor", Entries: []conv.Entry{
		{Kind: conv.KindPrompt, Text: "see \x1b]0;pwned\x07this"},
		{Kind: conv.KindText, Text: "done\x1b[2J here"},
	}}, LogOptions{Raw: true}, nil)
	got := b.String()
	if strings.Contains(got, "\x1b]") || strings.Contains(got, "\x1b[2J") || !strings.Contains(got, "seethis") && !strings.Contains(got, "see this") {
		t.Fatalf("escapes reached the output: %q", got)
	}
}

// A fork's copied turns point at the session they came from; a copy's at
// the same session in the account it came from.
func TestCopiedNotes(t *testing.T) {
	l := &query.TurnList{Session: query.SessionRow{ID: "bbbbbbbb-0000-4000-8000-000000000001"}, Turns: []query.TurnRow{
		{Seq: 0, CopiedFrom: "work:aaaaaaaa-0000-4000-8000-000000000001"},
		{Seq: 1, CopiedFrom: "work:aaaaaaaa-0000-4000-8000-000000000001"},
		{Seq: 2, CopiedFrom: "work:aaaaaaaa-0000-4000-8000-000000000001"},
		{Seq: 3},
		{Seq: 4, CopiedFrom: "default"},
	}}
	got := copiedNotes(l)
	want := []string{
		"turns 1 to 2 came from aaaaaaaa in work when this session was forked from it; their usage counts there (ccli log aaaaaaaa -a work)",
		"turn 4 came with a copy from default; their usage counts in default (ccli log bbbbbbbb -a default)",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("notes:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
