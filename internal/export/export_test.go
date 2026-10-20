package export

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/lr00rl/ccli/internal/account"
	"github.com/lr00rl/ccli/internal/conv"
	"github.com/lr00rl/ccli/internal/query"
)

func sample() Doc {
	at := time.Date(2026, 10, 18, 20, 58, 0, 0, time.UTC)
	return Doc{
		Session: query.SessionRow{
			Account: "cursor", Tool: account.Cursor, ID: "ee0b5fa1-83a7-4e1f-a260-bb4a1657ad54",
			Title: "Tmux Radar", Cwd: "/work/ccli", Started: at, Last: at,
		},
		Turns: []*query.TurnDetail{{
			Row: query.TurnRow{Seq: 6},
			Turn: &conv.Turn{Speaker: "Cursor", Entries: []conv.Entry{
				{Kind: conv.KindPrompt, Text: "filter the drafts"},
				{Kind: conv.KindThinking, Text: "secret plan"},
				{Kind: conv.KindContext, Label: "skill", Text: "a long skill"},
				{Kind: conv.KindText, Text: "They stay out of the list."},
				{Kind: conv.KindTool, Call: &conv.Call{
					Name: "Grep", Summary: "ideDraft", Input: []byte(`{"path":"sessions.go"}`),
					Output: "found\n", OutputLen: 10, Done: true,
				}},
			}},
			Agents: []query.AgentNode{{FileInfo: query.FileInfo{AgentID: "sub-1"}, Label: "reviewer"}},
		}},
		Agents: map[string]*query.AgentLog{
			"sub-1": {Turns: []conv.Turn{{Speaker: "Cursor", Entries: []conv.Entry{
				{Kind: conv.KindPrompt, Text: "check the filter"},
				{Kind: conv.KindText, Text: "It drops empty drafts."},
			}}}},
		},
	}
}

func TestMarkdownKeepsOrderAndSkipsAsides(t *testing.T) {
	var b strings.Builder
	if err := Write(&b, sample(), Options{}); err != nil {
		t.Fatal(err)
	}
	got := b.String()
	prompt := strings.Index(got, "filter the drafts")
	reply := strings.Index(got, "They stay out of the list.")
	tool := strings.Index(got, "### Tool: Grep")
	if prompt < 0 || reply < prompt || tool < reply {
		t.Fatalf("order:\n%s", got)
	}
	if strings.Contains(got, "secret plan") || strings.Contains(got, "a long skill") {
		t.Fatalf("aside leaked:\n%s", got)
	}
	if !strings.Contains(got, "Started agent reviewer.") || strings.Contains(got, "check the filter") {
		t.Fatalf("agent:\n%s", got)
	}
	if !strings.Contains(got, "4 more bytes were not included.") {
		t.Fatalf("truncation:\n%s", got)
	}
	if !strings.Contains(got, "not a file any tool can resume") {
		t.Fatalf("banner missing:\n%s", got)
	}
}

func TestReplyKeepsPromptAndLastText(t *testing.T) {
	doc := sample()
	doc.Turns[0].Turn.Entries = []conv.Entry{
		{Kind: conv.KindPrompt, Text: "filter the drafts"},
		{Kind: conv.KindText, Text: "Looking now."},
		{Kind: conv.KindTool, Call: &conv.Call{Name: "Grep", Input: []byte(`{"path":"sessions.go"}`), Output: "found\n", OutputLen: 6, Done: true}},
		{Kind: conv.KindText, Text: "They stay out of the list."},
		{Kind: conv.KindText, Text: "Empty drafts too."},
	}
	var b strings.Builder
	if err := Write(&b, doc, Options{Detail: "reply"}); err != nil {
		t.Fatal(err)
	}
	got := b.String()
	prompt := strings.Index(got, "filter the drafts")
	reply := strings.Index(got, "They stay out of the list.")
	tail := strings.Index(got, "Empty drafts too.")
	if prompt < 0 || reply < prompt || tail < reply {
		t.Fatalf("order:\n%s", got)
	}
	if strings.Contains(got, "Looking now.") || strings.Contains(got, "### Tool:") || strings.Contains(got, "Started agent") {
		t.Fatalf("step leaked:\n%s", got)
	}

	b.Reset()
	if err := Write(&b, doc, Options{Format: "jsonl", Detail: "reply", Agents: true, Thinking: true}); err != nil {
		t.Fatal(err)
	}
	body := b.String()
	if strings.Contains(body, `"kind":"tool"`) || strings.Contains(body, "Looking now.") || strings.Contains(body, "reviewer") {
		t.Fatalf("jsonl steps:\n%s", body)
	}
	if !strings.Contains(body, `"kind":"prompt"`) || !strings.Contains(body, "Empty drafts too.") {
		t.Fatalf("jsonl reply:\n%s", body)
	}
	if err := Write(io.Discard, doc, Options{Detail: "steps"}); err == nil {
		t.Fatal("bad detail was accepted")
	}
}

func TestAgentsInlineAndJSONL(t *testing.T) {
	var b strings.Builder
	if err := Write(&b, sample(), Options{Format: "jsonl", Agents: true, Thinking: true}); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(b.String()), "\n")
	if len(lines) < 2 || !strings.Contains(lines[0], `"type":"session"`) || !strings.Contains(lines[0], `"tool":"cursor"`) {
		t.Fatalf("head %q", lines[0])
	}
	body := strings.Join(lines[1:], "\n")
	if !strings.Contains(body, `"kind":"thinking"`) {
		t.Fatalf("thinking missing:\n%s", body)
	}
	if strings.Contains(body, "a long skill") {
		t.Fatal("context included without the flag")
	}
	agent := strings.Index(body, `"agent":"sub-1"`)
	child := strings.Index(body, "check the filter")
	if agent < 0 || child < agent {
		t.Fatalf("agent order:\n%s", body)
	}
}

// Text from the conversation cannot add turn or speaker headings to the
// document, and a code fence it leaves open does not swallow what follows.
func TestMarkdownContainsMessageStructure(t *testing.T) {
	doc := sample()
	doc.Session.Title = "Fix\n# injected"
	doc.Turns[0].Turn.Entries = []conv.Entry{
		{Kind: conv.KindPrompt, Text: "why does this happen?\n## Turn 99\n### You\nfake"},
		{Kind: conv.KindText, Text: "Agent: x\n===\n## Plan\nFirst this:\n```go\nfunc open() {"},
	}
	doc.Turns = append(doc.Turns, &query.TurnDetail{Row: query.TurnRow{Seq: 7}, Turn: &conv.Turn{Speaker: "Cursor", Entries: []conv.Entry{
		{Kind: conv.KindPrompt, Text: "next"},
	}}})
	var b strings.Builder
	if err := Write(&b, doc, Options{}); err != nil {
		t.Fatal(err)
	}
	var heads []string
	inFence := false
	for _, l := range strings.Split(b.String(), "\n") {
		if strings.HasPrefix(l, "```") {
			inFence = !inFence
			continue
		}
		if !inFence && strings.HasPrefix(l, "#") && !strings.HasPrefix(l, "####") {
			heads = append(heads, l)
		}
	}
	want := []string{"# Fix # injected", "## Turn 6", "### You", "### Cursor", "## Turn 7", "### You"}
	if strings.Join(heads, "|") != strings.Join(want, "|") {
		t.Fatalf("headings %q, want %q\n%s", heads, want, b.String())
	}
	if !strings.Contains(b.String(), "Agent: x\n\n===") {
		t.Fatalf("an underline still makes the line above it a heading:\n%s", b.String())
	}
	if !strings.Contains(b.String(), "##### Turn 99") || !strings.Contains(b.String(), "##### Plan") {
		t.Fatalf("message headings were not kept as lower headings:\n%s", b.String())
	}
}
