package tui

import (
	"strings"
	"testing"

	"github.com/lr00rl/ccli/internal/conv"
	"github.com/lr00rl/ccli/internal/query"
)

func TestToolPaneShowsOneCall(t *testing.T) {
	d := &query.TurnDetail{Turn: &conv.Turn{Entries: []conv.Entry{
		{Kind: conv.KindTool, Call: &conv.Call{Name: "Shell", Summary: "go test", Input: []byte(`{"command":"go test"}`), Output: "ok", Done: true}},
		{Kind: conv.KindTool, Call: &conv.Call{Name: "Read", Summary: "app.go", Input: []byte(`{"path":"app.go"}`), Output: "package tui", Done: true}},
	}}}
	v := &viewer{tools: true, toolN: 1}
	got := v.toolPane(d, 80)
	if !strings.Contains(got, "tool 2 of 2") || !strings.Contains(got, "app.go") || !strings.Contains(got, "package tui") {
		t.Fatalf("pane:\n%s", got)
	}
	if strings.Contains(got, "go test") && strings.Contains(got, "ok\n") {
		t.Fatal("the other call's output is in this pane")
	}
}

// Moving to another turn starts its tool inspector at the first call, not
// at the index the previous turn was on.
func TestToolIndexStartsOverOnAnotherTurn(t *testing.T) {
	calls := func(n int) *query.TurnDetail {
		d := &query.TurnDetail{Turn: &conv.Turn{}}
		for range n {
			d.Turn.Entries = append(d.Turn.Entries, conv.Entry{Kind: conv.KindTool, Call: &conv.Call{Name: "Read", Done: true}})
		}
		return d
	}
	s := &screen{role: turnsRole, list: list{all: []item{{}, {}}, shown: []int{0, 1}}, data: map[int]any{0: calls(8), 1: calls(3)}}
	v := &viewer{tools: true, stack: []*screen{s}}
	for range 7 {
		v.stepTool(s, 1)
	}
	if v.toolN != 7 {
		t.Fatalf("toolN on the first turn = %d, want 7", v.toolN)
	}
	s.list.cursor = 1
	v.stepTool(s, 0)
	if v.toolN != 0 {
		t.Fatalf("toolN on the next turn = %d, want 0", v.toolN)
	}
}
