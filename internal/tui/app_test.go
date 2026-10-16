package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/lr00rl/ccli/internal/query"
)

// fakeSrc fails every call, as a busy or vanished index would.
type fakeSrc struct{}

var errBusy = errors.New("database is busy")

func (fakeSrc) Sessions(context.Context, query.Filter) (query.SessionPage, error) {
	return query.SessionPage{}, errBusy
}
func (fakeSrc) Session(context.Context, string) (*query.SessionDetail, error) { return nil, errBusy }
func (fakeSrc) Turns(context.Context, string) (*query.TurnList, error)        { return nil, errBusy }
func (fakeSrc) TurnRange(context.Context, string, int, int, bool) ([]*query.TurnDetail, error) {
	return nil, errBusy
}
func (fakeSrc) Agent(context.Context, string, string) (*query.AgentLog, error) { return nil, errBusy }
func (fakeSrc) RunAgents(context.Context, string, string) (*query.RunInfo, []query.AgentNode, map[string]string, error) {
	return nil, nil, nil, errBusy
}

func newTestViewer(s *screen) *viewer {
	v := &viewer{ctx: context.Background(), src: fakeSrc{}, loc: time.UTC, rendered: map[string]string{}, w: 120, h: 30}
	v.stack = []*screen{s}
	v.resize()
	return v
}

// run feeds a command's message (and those it leads to) back into the viewer.
func run(v *viewer, cmd tea.Cmd) {
	for i := 0; cmd != nil && i < 10; i++ {
		msg := cmd()
		if msg == nil {
			return
		}
		_, cmd = v.Update(msg)
	}
}

func textItem(name string) item {
	return item{
		row:    func(int) string { return name },
		search: name,
		id:     name,
		load:   func() (any, error) { return name, nil },
		draw:   func(_ *viewer, data any, _ int) string { return "PREVIEW " + data.(string) },
		open:   func(any) *screen { return newScreen("opened "+name, "") },
	}
}

func key(k string) tea.KeyPressMsg {
	switch k {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	}
	return tea.KeyPressMsg{Code: rune(k[0]), Text: k}
}

// After the items are rebuilt in another order, neither a cached preview
// nor a load still in flight may show one item's content for another.
func TestRebuildDropsStalePreviews(t *testing.T) {
	order := []string{"A", "B"}
	s := newScreen("test", "")
	s.rebuild = func(*viewer) []item {
		var its []item
		for _, n := range order {
			its = append(its, textItem(n))
		}
		return its
	}
	s.list.set(s.rebuild(nil))
	v := newTestViewer(s)
	run(v, v.sync())
	if got := s.vp.GetContent(); !strings.Contains(got, "PREVIEW A") {
		t.Fatalf("preview %q, want A", got)
	}
	// A load for index 0 started before the rebuild and lands after it.
	staleGen := s.gen
	order = []string{"B", "A"}
	v.refresh(s)
	v.Update(previewMsg{scr: s, gen: staleGen, idx: 0, data: "A"})
	if _, ok := s.data[0]; ok {
		t.Fatal("a load made before the rebuild was stored")
	}
	run(v, v.sync())
	if idx := s.list.selected(); s.list.all[idx].id != "A" {
		t.Fatalf("selection moved to %q, want A kept", s.list.all[idx].id)
	}
	if got := s.vp.GetContent(); !strings.Contains(got, "PREVIEW A") {
		t.Fatalf("preview %q, want A", got)
	}
	run(v, v.key(key("k"))) // up, to B at index 0
	if got := s.vp.GetContent(); !strings.Contains(got, "PREVIEW B") {
		t.Fatalf("preview %q after moving to B", got)
	}
}

// A preview that failed to load leaves no data behind, and the keys that
// use the data cope with that instead of panicking.
func TestFailedLoadsDoNotPanic(t *testing.T) {
	v := newTestViewer(newScreen("x", ""))
	s := v.agentsScreen("acct:sess", "agents", []query.AgentNode{{FileInfo: query.FileInfo{AgentID: "a1"}, Label: "agent"}}, nil)
	v.stack = []*screen{s}
	v.resize()
	// y and o before the load returns.
	v.key(key("y"))
	if v.status != "still loading" {
		t.Fatalf("status %q, want still loading", v.status)
	}
	run(v, v.sync())
	if _, ok := s.data[0]; ok {
		t.Fatal("a failed load stored data")
	}
	if !strings.Contains(s.vp.GetContent(), errBusy.Error()) {
		t.Fatalf("preview %q does not show the error", s.vp.GetContent())
	}
	v.key(key("y"))
	if v.status != "nothing to copy" {
		t.Fatalf("status %q after y on a failed load", v.status)
	}
	v.key(key("o"))
	v.key(key("enter"))
}

// Enter on an item whose preview has not loaded opens it once it has.
func TestEnterOpensWhenLoaded(t *testing.T) {
	s := newScreen("test", "")
	s.list.set([]item{textItem("A")})
	v := newTestViewer(s)
	cmd := v.key(key("enter"))
	if len(v.stack) != 1 || s.pending != 0 {
		t.Fatalf("stack %d, pending %d before the load", len(v.stack), s.pending)
	}
	run(v, cmd)
	if len(v.stack) != 2 || v.cur().title != "opened A" {
		t.Fatalf("stack %d (top %q), want the opened screen", len(v.stack), v.cur().title)
	}
}

func TestTinyTerminal(t *testing.T) {
	s := newScreen("test", "")
	s.list.set([]item{textItem("A")})
	v := newTestViewer(s)
	for _, w := range []int{1, 2, 3} {
		v.Update(tea.WindowSizeMsg{Width: w, Height: 2})
		run(v, v.sync())
		_ = v.View()
	}
}

func headerItem(name string) item {
	return item{header: true, id: "h:" + name, row: func(int) string { return name }}
}

// Group titles are never selected: moving, jumping and clicking pass over
// them, and the filter keeps a title only while an item under it matches.
func TestHeadersAreSkipped(t *testing.T) {
	s := newScreen("test", "")
	s.list.set([]item{headerItem("P1"), textItem("a1"), textItem("a2"), headerItem("P2"), textItem("b1")})
	v := newTestViewer(s)
	sel := func() string {
		if idx := s.list.selected(); idx >= 0 {
			return s.list.all[idx].id
		}
		return ""
	}
	if got := sel(); got != "a1" {
		t.Fatalf("starts on %q, want a1", got)
	}
	v.key(key("j"))
	v.key(key("j"))
	if got := sel(); got != "b1" {
		t.Fatalf("down twice lands on %q, want b1 past the P2 title", got)
	}
	v.key(key("k"))
	if got := sel(); got != "a2" {
		t.Fatalf("up lands on %q, want a2", got)
	}
	v.key(key("g"))
	v.key(key("k"))
	if got := sel(); got != "a1" {
		t.Fatalf("up from the first item moves to %q, want a1", got)
	}
	v.key(key("G"))
	if got := sel(); got != "b1" {
		t.Fatalf("G selects %q, want b1", got)
	}
	v.click(tea.Mouse{Button: tea.MouseLeft, X: 2, Y: 1}) // the P1 title
	if got := sel(); got != "b1" {
		t.Fatalf("clicking a title selected %q", got)
	}

	s.list.filter = "b"
	s.list.apply()
	var shown []string
	for _, i := range s.list.shown {
		shown = append(shown, s.list.all[i].id)
	}
	if got := strings.Join(shown, " "); got != "h:P2 b1" {
		t.Fatalf("filtered rows %q, want the P2 title and b1", got)
	}
	if got := sel(); got != "b1" {
		t.Fatalf("after the filter %q is selected, want b1", got)
	}
}

func TestGroupByProject(t *testing.T) {
	rows := []query.SessionRow{
		{ID: "1", Cwd: "/w/ccli", Project: "ccli"},
		{ID: "2", Cwd: "/w/app", Project: "app"},
		{ID: "3", Cwd: "/w/ccli", Project: "ccli"},
		{ID: "4", Cwd: "/old/ccli", Project: "ccli"},
	}
	var got []string
	for _, g := range groupByProject(rows) {
		ids := ""
		for _, r := range g.rows {
			ids += r.ID
		}
		got = append(got, g.name+":"+ids)
	}
	if s := strings.Join(got, " "); s != "ccli:13 app:2 ccli:4" {
		t.Fatalf("groups %q", s)
	}
}
