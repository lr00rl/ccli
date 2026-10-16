package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

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
func (fakeSrc) Hide(context.Context, []string, time.Time) ([]query.HideResult, error) {
	return nil, errBusy
}
func (fakeSrc) Unhide(context.Context, []string) ([]query.HideResult, error) { return nil, errBusy }
func (fakeSrc) Tag(context.Context, string, string, time.Time) (query.TagResult, error) {
	return query.TagResult{}, errBusy
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
		open:   func(any) *screen { return newScreen("opened "+name, agentsRole) },
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
	s := newScreen("test", agentsRole)
	s.rebuild = func(*viewer) ([]item, error) {
		var its []item
		for _, n := range order {
			its = append(its, textItem(n))
		}
		return its, nil
	}
	items, _ := s.rebuild(nil)
	s.list.set(items)
	v := newTestViewer(s)
	run(v, v.sync())
	if got := s.vp.GetContent(); !strings.Contains(got, "PREVIEW A") {
		t.Fatalf("preview %q, want A", got)
	}
	// A load for index 0 started before the rebuild and lands after it.
	staleGen := s.gen
	order = []string{"B", "A"}
	v.refresh(s)
	_, cmd := v.Update(previewMsg{scr: s, gen: staleGen, idx: 0, data: "A"})
	if _, ok := s.data[0]; ok {
		t.Fatal("a load made before the rebuild was stored")
	}
	run(v, cmd)
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
	v := newTestViewer(newScreen("x", agentsRole))
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
	s := newScreen("test", agentsRole)
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
	s := newScreen("test", agentsRole)
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
	s := newScreen("test", agentsRole)
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

// sessionSrc serves a fixed set of sessions and keeps what was hidden and
// what was tagged.
type sessionSrc struct {
	fakeSrc
	rows   []query.SessionRow
	hidden map[string]bool
	tags   map[string]string
}

func (s *sessionSrc) Tag(_ context.Context, ref, tag string, _ time.Time) (query.TagResult, error) {
	id := ref[strings.IndexByte(ref, ':')+1:]
	if s.tags == nil {
		s.tags = map[string]string{}
	}
	if tag == "" {
		delete(s.tags, id)
	} else {
		s.tags[id] = tag
	}
	return query.TagResult{ID: id, Tag: tag, Cleared: tag == ""}, nil
}

func (s *sessionSrc) Sessions(_ context.Context, f query.Filter) (query.SessionPage, error) {
	var p query.SessionPage
	for _, r := range s.rows {
		r.Hidden = s.hidden[r.ID]
		r.Tag = s.tags[r.ID]
		switch {
		case (f.Show == query.HiddenOnly) == r.Hidden:
			p.Sessions = append(p.Sessions, r)
		case r.Hidden:
			p.Hidden++
		}
	}
	return p, nil
}

func (s *sessionSrc) set(refs []string, hide bool) []query.HideResult {
	var out []query.HideResult
	for _, ref := range refs {
		id := ref[strings.IndexByte(ref, ':')+1:]
		out = append(out, query.HideResult{Ref: ref, ID: id, Done: s.hidden[id] != hide})
		s.hidden[id] = hide
	}
	return out
}

func (s *sessionSrc) Hide(_ context.Context, refs []string, _ time.Time) ([]query.HideResult, error) {
	return s.set(refs, true), nil
}

func (s *sessionSrc) Unhide(_ context.Context, refs []string) ([]query.HideResult, error) {
	return s.set(refs, false), nil
}

// Marking two sessions and hiding them asks first, then leaves them out of
// the list; H lists them, and u brings one back.
func TestHideFromTheSessionsList(t *testing.T) {
	src := &sessionSrc{hidden: map[string]bool{}, rows: []query.SessionRow{
		{Account: "default", ID: "a1", Cwd: "/w/a", Project: "a", Title: "first"},
		{Account: "default", ID: "a2", Cwd: "/w/a", Project: "a", Title: "second"},
		{Account: "work", ID: "b1", Cwd: "/w/b", Project: "b", Title: "third"},
	}}
	v := &viewer{ctx: context.Background(), src: src, loc: time.UTC, rendered: map[string]string{}, w: 120, h: 30}
	s, err := v.sessionsScreen()
	if err != nil {
		t.Fatal(err)
	}
	v.stack = []*screen{s}
	v.resize()
	listed := func() string {
		var out []string
		for _, i := range s.list.shown {
			if it := s.list.all[i]; !it.header {
				out = append(out, it.id)
			}
		}
		return strings.Join(out, " ")
	}

	v.key(key("m"))
	v.key(key("m"))
	if got := strings.Join(s.list.targets(), " "); got != "default:a1 default:a2" {
		t.Fatalf("marked %q", got)
	}
	v.key(key("x"))
	if v.confirm == nil || !strings.Contains(v.confirm.question, "2 sessions") {
		t.Fatalf("x asked %+v", v.confirm)
	}
	v.key(key("n"))
	if v.confirm != nil || len(src.hidden) != 0 {
		t.Fatal("answering no hid something")
	}
	v.key(key("x"))
	cmd := v.key(key("y"))
	if len(src.hidden) != 0 || v.status != "hiding…" {
		t.Fatalf("the key hid %v itself, status %q; the store belongs off the update loop", src.hidden, v.status)
	}
	run(v, cmd)
	if !src.hidden["a1"] || !src.hidden["a2"] || src.hidden["b1"] {
		t.Fatalf("hidden %v", src.hidden)
	}
	if got := listed(); got != "work:b1" {
		t.Fatalf("after hiding the list shows %q", got)
	}
	if len(s.list.marked) != 0 || !strings.HasPrefix(v.status, "hid 2 sessions") || !strings.Contains(s.hint, "2 hidden") {
		t.Fatalf("marks %v, status %q, hint %q", s.list.marked, v.status, s.hint)
	}

	run(v, v.key(key("H")))
	if got := listed(); got != "default:a1 default:a2" || s.title != "hidden sessions" {
		t.Fatalf("hidden list %q titled %q", got, s.title)
	}
	v.key(key("x"))
	if v.confirm != nil {
		t.Fatal("x in the hidden list asked to hide again")
	}
	v.key(key("u"))
	if v.confirm == nil || !strings.Contains(v.confirm.question, "this session") {
		t.Fatalf("u asked %+v", v.confirm)
	}
	run(v, v.key(key("enter")))
	if src.hidden["a1"] || !src.hidden["a2"] {
		t.Fatalf("after bringing a1 back: %v", src.hidden)
	}
	run(v, v.key(key("H")))
	if got := listed(); got != "default:a1 work:b1" {
		t.Fatalf("back in the full list %q", got)
	}
}

// Moving through many rows starts one preview load, not one per row: the
// store has one connection, so loads for rows already left would queue ahead
// of everything else. When the load lands, the row selected then loads.
func TestOneLoadAtATime(t *testing.T) {
	s := newScreen("test", agentsRole)
	var loads []string
	var items []item
	for _, n := range []string{"A", "B", "C", "D", "E"} {
		it := textItem(n)
		it.load = func() (any, error) { loads = append(loads, n); return n, nil }
		items = append(items, it)
	}
	s.list.set(items)
	v := newTestViewer(s)
	first := v.sync()
	var later []tea.Cmd
	for range 4 {
		if cmd := v.key(key("j")); cmd != nil {
			later = append(later, cmd)
		}
	}
	if len(later) != 0 {
		t.Fatalf("moving started %d more loads while one ran", len(later))
	}
	run(v, first)
	if strings.Join(loads, " ") != "A E" {
		t.Fatalf("loaded %q, want A, then only E where the cursor rests", loads)
	}
	if got := s.vp.GetContent(); !strings.Contains(got, "PREVIEW E") {
		t.Fatalf("preview %q, want E", got)
	}
}

// # puts the selected session's tag in the footer; typing edits it, Enter
// saves it off the update loop, Esc leaves it, and an empty tag removes it.
// The row shows the tag in its own column and keeps the session's title.
func TestTagFromTheSessionsList(t *testing.T) {
	src := &sessionSrc{hidden: map[string]bool{}, rows: []query.SessionRow{
		{Account: "default", ID: "a1", Cwd: "/w/a", Project: "a", Title: "first"},
		{Account: "work", ID: "b2", Cwd: "/w/a", Project: "a", Title: "second"},
	}}
	v := &viewer{ctx: context.Background(), src: src, loc: time.UTC, rendered: map[string]string{}, w: 120, h: 30}
	s, err := v.sessionsScreen()
	if err != nil {
		t.Fatal(err)
	}
	v.stack = []*screen{s}
	v.resize()
	sel := func() item { return s.list.all[s.list.selected()] }
	row := func(i int) string { return ansi.Strip(s.list.all[i].row(120)) }
	col := func(line, sub string) int { return ansi.StringWidth(line[:max(0, strings.Index(line, sub))]) }

	v.key(key("#"))
	if v.edit == nil || v.edit.text != "" || !strings.Contains(ansi.Strip(v.footer()), "tag:") {
		t.Fatalf("# opened %+v, footer %q", v.edit, v.footer())
	}
	v.key(key("x")) // typed, not a hide
	if v.confirm != nil || v.edit.text != "x" {
		t.Fatalf("typing x gave %+v, confirm %+v", v.edit, v.confirm)
	}
	v.key(tea.KeyPressMsg{Code: tea.KeyEscape})
	if v.edit != nil || len(src.tags) != 0 {
		t.Fatalf("esc saved %v", src.tags)
	}

	v.key(key("#"))
	v.Update(tea.PasteMsg{Content: "payments\naudit"})
	cmd := v.key(key("enter"))
	if len(src.tags) != 0 || v.status != "tagging…" {
		t.Fatalf("enter tagged %v itself, status %q", src.tags, v.status)
	}
	run(v, cmd)
	if src.tags["a1"] != "payments audit" || sel().tag != "payments audit" || !strings.HasPrefix(v.status, "tagged a1") {
		t.Fatalf("tags %v, tag %q, status %q", src.tags, sel().tag, v.status)
	}
	// The tag sits in a column of its own, cut to maxTagCells, before the
	// unchanged title, and the untagged row keeps the titles lined up.
	a, b := row(s.list.selected()), ""
	for i, it := range s.list.all {
		if it.id == "work:b2" {
			b = row(i)
		}
	}
	if !strings.Contains(a, "payments au… first") || col(a, "first") != col(b, "second") {
		t.Fatalf("rows not aligned:\n%q\n%q", a, b)
	}

	v.key(key("#"))
	if v.edit.text != "payments audit" {
		t.Fatalf("# offered %q to edit", v.edit.text)
	}
	v.key(tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl})
	run(v, v.key(key("enter")))
	if len(src.tags) != 0 || sel().tag != "" || strings.Contains(row(s.list.selected()), "payments") {
		t.Fatalf("an empty tag left %v, row %q", src.tags, row(s.list.selected()))
	}
}

// In a narrow list pane the title keeps its room: the tag column shrinks
// first and goes when too little is left for it, while the preview still
// shows the tag.
func TestTagGivesWayToTheTitle(t *testing.T) {
	rows := []query.SessionRow{
		{Account: "default", ID: "a1", Title: "Fix tooltip overflow on narrow screens", Tag: "payments-audit"},
		{Account: "work", ID: "b2", Title: "Second session"},
	}
	cols := measureSessions(rows)
	v := &viewer{loc: time.UTC}
	row := func(w int) string { return ansi.Strip(v.sessionItem(rows[0], cols).row(w)) }
	if r := row(44); strings.Contains(r, "payments") || !strings.Contains(r, "Fix tooltip") {
		t.Fatalf("44 cells: %q", r)
	}
	if r := row(60); !strings.Contains(r, "payments") || !strings.Contains(r, "Fix tooltip") {
		t.Fatalf("60 cells: %q", r)
	}
	if r := row(120); !strings.Contains(r, "payments-au… Fix tooltip overflow") {
		t.Fatalf("120 cells: %q", r)
	}
}

// Columns are measured in cells, so wide characters line up and a tag is
// cut to fit with an ellipsis.
func TestPadCells(t *testing.T) {
	for _, c := range []struct {
		in   string
		w    int
		want string
	}{
		{"ab", 4, "ab  "},
		{"支付审计", 8, "支付审计"},
		{"支付审计", 6, "支付… "},
		{"payments-audit", 12, "payments-au…"},
	} {
		if got := padCells(c.in, c.w); got != c.want || ansi.StringWidth(got) != c.w {
			t.Errorf("padCells(%q, %d) = %q", c.in, c.w, got)
		}
	}
	if c := measureSessions([]query.SessionRow{{Account: "work", Tag: "支付审计"}, {Account: "codex-work"}}); c.account != 10 || c.tag != 8 {
		t.Errorf("measured %+v", c)
	}
}

// The tag editor stops at MaxTag characters, typed or pasted, so a save
// never fails on length after the line has closed.
func TestTagEditorStopsAtTheLimit(t *testing.T) {
	src := &sessionSrc{hidden: map[string]bool{}, rows: []query.SessionRow{{Account: "default", ID: "a1", Cwd: "/w/a", Title: "first"}}}
	v := &viewer{ctx: context.Background(), src: src, loc: time.UTC, rendered: map[string]string{}, w: 120, h: 30}
	s, err := v.sessionsScreen()
	if err != nil {
		t.Fatal(err)
	}
	v.stack = []*screen{s}
	v.resize()
	v.key(key("#"))
	v.Update(tea.PasteMsg{Content: strings.Repeat("长", query.MaxTag+5)})
	v.key(key("x"))
	if n := len([]rune(v.edit.text)); n != query.MaxTag {
		t.Fatalf("editor holds %d characters", n)
	}
	run(v, v.key(key("enter")))
	if src.tags["a1"] != strings.Repeat("长", query.MaxTag) {
		t.Fatalf("saved %q, status %q", src.tags["a1"], v.status)
	}
}
