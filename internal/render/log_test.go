package render

import (
	"strings"
	"testing"
	"time"

	"github.com/lr00rl/ccli/internal/conv"
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
