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
