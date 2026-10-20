package cli

import (
	"testing"

	"github.com/lr00rl/ccli/internal/conv"
	"github.com/lr00rl/ccli/internal/query"
)

// log --detail reply --json carries what the reply view prints, and leaves
// the details it was given as they were.
func TestReplyOnly(t *testing.T) {
	in := []*query.TurnDetail{{
		Turn: &conv.Turn{Entries: []conv.Entry{
			{Kind: conv.KindPrompt, Text: "fix it"},
			{Kind: conv.KindText, Text: "Looking."},
			{Kind: conv.KindTool, Call: &conv.Call{Name: "Bash"}},
			{Kind: conv.KindText, Text: "Fixed."},
		}},
		Agents: []query.AgentNode{{Label: "reviewer"}},
	}, {Row: query.TurnRow{Seq: 2}}}
	out := replyOnly(in)
	if len(out) != 2 || len(out[0].Turn.Entries) != 2 || out[0].Turn.Entries[1].Text != "Fixed." || out[0].Agents != nil {
		t.Fatalf("reply: %+v", out[0])
	}
	if len(in[0].Turn.Entries) != 4 || in[0].Agents == nil {
		t.Fatal("the input was changed")
	}
	if out[1].Turn != nil || out[1].Row.Seq != 2 {
		t.Fatalf("a turn with no transcript: %+v", out[1])
	}
}
