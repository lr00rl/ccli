// Package tui is ccli's interactive browser: sessions, then a session's
// turns, then the agents a turn started, each with a live preview rendered
// by the same code as `ccli show` and `ccli log`.
package tui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/lr00rl/ccli/internal/query"
	"github.com/lr00rl/ccli/internal/render"
)

// Source is what the browser reads; *query.DB implements it.
type Source interface {
	Sessions(ctx context.Context, f query.Filter) (query.SessionPage, error)
	Session(ctx context.Context, ref string) (*query.SessionDetail, error)
	Turns(ctx context.Context, ref string) (*query.TurnList, error)
	TurnRange(ctx context.Context, ref string, from, to int, all bool) ([]*query.TurnDetail, error)
	Agent(ctx context.Context, ref, agentID string) (*query.AgentLog, error)
	RunAgents(ctx context.Context, ref, runID string) (*query.RunInfo, []query.AgentNode, map[string]string, error)
}

// screen is one level of the browser.
type screen struct {
	title, kind string
	hint        string
	list        list
	data        map[int]any
	errs        map[int]error
	loading     map[int]bool
	vp          viewport.Model
	shown       string // cache key of what the preview holds
	focus       bool   // the preview has the keyboard
	full        bool   // narrow terminals: the preview fills the screen
	rebuild     func(v *viewer) []item
	// gen changes whenever the items are rebuilt, so the index a load or a
	// cached preview was made for can no longer point at another item.
	gen int
	// pending is the item Enter was pressed on before its preview loaded;
	// it opens when the load arrives. -1 when there is none.
	pending int
}

func newScreen(title, kind string) *screen {
	return &screen{title: title, kind: kind, data: map[int]any{}, errs: map[int]error{}, loading: map[int]bool{}, vp: viewport.New(), pending: -1}
}

type viewer struct {
	ctx      context.Context
	src      Source
	loc      *time.Location
	stack    []*screen
	w, h     int
	opt      render.LogOptions
	allTurns bool
	sortCost bool
	filter   bool   // the filter line has the keyboard
	query    string // what is typed in it
	help     bool
	status   string
	rendered map[string]string
}

type previewMsg struct {
	scr  *screen
	gen  int
	idx  int
	data any
	err  error
}

// maxRendered bounds the cache of rendered previews.
const maxRendered = 64

type statusMsg string

// Run starts the browser at the sessions list, or at a session's turns when
// ref names one.
func Run(ctx context.Context, src Source, ref string) error {
	v := &viewer{ctx: ctx, src: src, loc: time.Local, rendered: map[string]string{}}
	var first *screen
	var err error
	if ref != "" {
		first, err = v.turnsScreen(ref)
	} else {
		first, err = v.sessionsScreen()
	}
	if err != nil {
		return err
	}
	v.stack = []*screen{first}
	_, err = tea.NewProgram(v, tea.WithContext(ctx)).Run()
	if errors.Is(err, tea.ErrProgramKilled) && ctx.Err() != nil {
		return nil
	}
	return err
}

func (v *viewer) cur() *screen { return v.stack[len(v.stack)-1] }

func (v *viewer) wide() bool { return v.w >= 100 }

// listWidth is the left pane's width in a wide layout.
func (v *viewer) listWidth() int { return min(max(v.w*2/5, 44), 80) }

func (v *viewer) bodyHeight() int { return max(1, v.h-2) }

// previewWidth is the width the preview is rendered for.
func (v *viewer) previewWidth(s *screen) int {
	if v.wide() && !s.full {
		return v.w - v.listWidth() - 3
	}
	return max(1, v.w-2)
}

func (v *viewer) Init() tea.Cmd { return nil }

func (v *viewer) term(w int) render.Term {
	return render.Term{Width: w, Color: true, Now: time.Now(), Loc: v.loc}
}

// capture renders into a string for a w-cell column.
func (v *viewer) capture(w int, draw func(render.Term)) string {
	var b bytes.Buffer
	t := v.term(w)
	t.W = &b
	draw(t)
	return strings.TrimRight(b.String(), "\n")
}

// sync loads or shows the selected item's preview.
func (v *viewer) sync() tea.Cmd {
	s := v.cur()
	idx := s.list.selected()
	if idx < 0 {
		s.vp.SetContent("")
		s.shown = ""
		return nil
	}
	it := s.list.all[idx]
	if it.load == nil {
		return nil
	}
	w := v.previewWidth(s)
	itemKey := fmt.Sprintf("%p/%d/%d/", s, s.gen, idx)
	key := fmt.Sprintf("%s%d/%v", itemKey, w, v.opt)
	if s.shown == key {
		return nil
	}
	if data, ok := s.data[idx]; ok {
		content, ok := v.rendered[key]
		if !ok {
			content = clamp(it.draw(v, data, w), w)
			if err := s.errs[idx]; err != nil {
				content += "\n\n" + warn(err.Error())
			}
			if len(v.rendered) >= maxRendered {
				clear(v.rendered)
			}
			v.rendered[key] = content
		}
		top := s.shown == "" || !strings.HasPrefix(s.shown, itemKey)
		s.vp.SetContent(content)
		if top {
			s.vp.GotoTop()
		}
		s.shown = key
		return nil
	}
	if err := s.errs[idx]; err != nil {
		s.vp.SetContent(warn(err.Error()))
		s.shown = key
		return nil
	}
	s.vp.SetContent(dim("loading…"))
	s.shown = ""
	if s.loading[idx] {
		return nil
	}
	s.loading[idx] = true
	gen := s.gen
	return func() tea.Msg {
		data, err := it.load()
		return previewMsg{scr: s, gen: gen, idx: idx, data: data, err: err}
	}
}

func (v *viewer) resize() {
	for _, s := range v.stack {
		s.vp.SetWidth(v.previewWidth(s))
		s.vp.SetHeight(v.bodyHeight())
	}
}

func (v *viewer) push(s *screen) tea.Cmd {
	if s == nil {
		return nil
	}
	v.stack = append(v.stack, s)
	v.resize()
	return v.sync()
}

func (v *viewer) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		v.w, v.h = msg.Width, msg.Height
		v.resize()
		return v, v.sync()
	case previewMsg:
		s := msg.scr
		if msg.gen != s.gen {
			return v, nil // loaded for items that have since been rebuilt
		}
		s.loading[msg.idx] = false
		if msg.data != nil {
			s.data[msg.idx] = msg.data
		}
		if msg.err != nil {
			s.errs[msg.idx] = msg.err
		}
		if s != v.cur() {
			return v, nil
		}
		if s.pending == msg.idx && s.list.selected() == msg.idx {
			s.pending = -1
			if data, ok := s.data[msg.idx]; ok {
				if next := s.list.all[msg.idx].open(data); next != nil {
					return v, v.push(next)
				}
			}
		}
		return v, v.sync()
	case tea.PasteMsg:
		if v.filter {
			v.query += strings.Join(strings.Fields(msg.Content), " ")
			s := v.cur()
			s.list.filter = v.query
			s.list.apply()
			s.list.top()
			return v, v.sync()
		}
		return v, nil
	case statusMsg:
		v.status = string(msg)
		return v, nil
	case tea.MouseWheelMsg:
		return v, v.wheel(msg.Mouse())
	case tea.MouseClickMsg:
		return v, v.click(msg.Mouse())
	case tea.KeyPressMsg:
		return v, v.key(msg)
	}
	return v, nil
}

func (v *viewer) key(msg tea.KeyPressMsg) tea.Cmd {
	s := v.cur()
	k := msg.String()
	v.status = ""
	s.pending = -1
	if v.filter {
		switch k {
		case "esc":
			v.filter, v.query = false, ""
		case "enter", "down", "up", "tab":
			v.filter = false
			return v.sync()
		case "backspace":
			if r := []rune(v.query); len(r) > 0 {
				v.query = string(r[:len(r)-1])
			}
		case "ctrl+u":
			v.query = ""
		case "ctrl+c":
			return tea.Quit
		default:
			if msg.Text == "" {
				return nil
			}
			v.query += msg.Text
		}
		s.list.filter = v.query
		s.list.apply()
		s.list.top()
		return v.sync()
	}
	if v.help {
		v.help = false
		if k == "ctrl+c" {
			return tea.Quit
		}
		return nil
	}
	page := v.bodyHeight() - 1
	previewKeys := s.focus || s.full
	switch k {
	case "ctrl+c", "q":
		return tea.Quit
	case "?":
		v.help = true
	case "/":
		v.filter = true
		s.focus, s.full = false, false
		v.query = s.list.filter
		return nil
	case "esc", "backspace", "left", "h":
		switch {
		case s.full:
			s.full = false
			v.resize()
			return v.sync()
		case s.focus:
			s.focus = false
		case s.list.filter != "":
			s.list.filter = ""
			s.list.apply()
			return v.sync()
		case len(v.stack) > 1:
			v.stack = v.stack[:len(v.stack)-1]
			v.resize()
			return v.sync()
		}
	case "tab":
		if v.wide() {
			s.focus = !s.focus
		}
	case "right", "l":
		if v.wide() {
			s.focus = true
		} else {
			s.full = true
			v.resize()
			return v.sync()
		}
	case "enter":
		idx := s.list.selected()
		if idx < 0 {
			return nil
		}
		it := s.list.all[idx]
		if it.open != nil {
			if data, ok := s.data[idx]; ok || it.load == nil {
				return v.push(it.open(data))
			}
			if err := s.errs[idx]; err != nil {
				v.status = err.Error()
				return nil
			}
			// Open it as soon as it has loaded.
			s.pending = idx
			return v.sync()
		}
		if v.wide() {
			s.focus = true
		} else {
			s.full = true
			v.resize()
			return v.sync()
		}
	case "o":
		idx := s.list.selected()
		if idx < 0 || s.list.all[idx].children == nil {
			return nil
		}
		data, ok := s.data[idx]
		if !ok {
			v.status = "still loading"
			return nil
		}
		if c := s.list.all[idx].children(data); c != nil {
			return v.push(c)
		}
		v.status = "no agents here"
	case "y":
		idx := s.list.selected()
		if idx < 0 || s.list.all[idx].copy == nil {
			return nil
		}
		data, ok := s.data[idx]
		if !ok && s.errs[idx] == nil {
			v.status = "still loading"
			return nil
		}
		text := s.list.all[idx].copy(data)
		if text == "" {
			v.status = "nothing to copy"
			return nil
		}
		return copyText(text)
	case "t":
		v.opt.Tools = !v.opt.Tools
		return v.sync()
	case "T":
		v.opt.Thinking = !v.opt.Thinking
		return v.sync()
	case "c":
		v.opt.Context = !v.opt.Context
		return v.sync()
	case "r":
		v.opt.Raw = !v.opt.Raw
		return v.sync()
	case "s", "a":
		if s.rebuild == nil {
			return nil
		}
		if k == "s" {
			v.sortCost = !v.sortCost
		} else {
			v.allTurns = !v.allTurns
		}
		v.refresh(s)
		return v.sync()
	case "down", "j":
		if previewKeys {
			s.vp.ScrollDown(1)
		} else {
			s.list.move(1)
		}
	case "up", "k":
		if previewKeys {
			s.vp.ScrollUp(1)
		} else {
			s.list.move(-1)
		}
	case "pgdown", "ctrl+d", "space", "f":
		if previewKeys || k == "space" {
			s.vp.ScrollDown(page)
		} else {
			s.list.move(page)
		}
	case "pgup", "ctrl+u", "b":
		if previewKeys {
			s.vp.ScrollUp(page)
		} else {
			s.list.move(-page)
		}
	case "home", "g":
		if previewKeys {
			s.vp.GotoTop()
		} else {
			s.list.top()
		}
	case "end", "G":
		if previewKeys {
			s.vp.GotoBottom()
		} else {
			s.list.bottom()
		}
	case "J":
		s.vp.ScrollDown(3)
	case "K":
		s.vp.ScrollUp(3)
	}
	return v.sync()
}

// refresh rebuilds a screen's items (after a sort or filter toggle),
// keeping the selected item when it is still there.
func (v *viewer) refresh(s *screen) {
	keep := ""
	if idx := s.list.selected(); idx >= 0 {
		keep = s.list.all[idx].id
	}
	s.list.set(s.rebuild(v))
	s.data, s.errs, s.loading, s.shown = map[int]any{}, map[int]error{}, map[int]bool{}, ""
	s.gen++
	s.pending = -1
	if keep == "" {
		return
	}
	for i, it := range s.list.all {
		if it.id == keep {
			s.list.selectIndex(i)
			break
		}
	}
}

func (v *viewer) wheel(m tea.Mouse) tea.Cmd {
	s := v.cur()
	inPreview := s.full || (v.wide() && m.X > v.listWidth())
	d := 3
	if m.Button == tea.MouseWheelUp {
		d = -3
	} else if m.Button != tea.MouseWheelDown {
		return nil
	}
	if inPreview {
		if d > 0 {
			s.vp.ScrollDown(d)
		} else {
			s.vp.ScrollUp(-d)
		}
		return nil
	}
	s.list.move(d / 3)
	return v.sync()
}

func (v *viewer) click(m tea.Mouse) tea.Cmd {
	s := v.cur()
	if v.help {
		v.help = false
		return nil
	}
	s.pending = -1
	if v.filter || m.Button != tea.MouseLeft || s.full || m.Y < 1 || m.Y > v.bodyHeight() {
		return nil
	}
	if v.wide() && m.X > v.listWidth() {
		s.focus = true
		return nil
	}
	row := s.list.offset + m.Y - 1
	if row < len(s.list.shown) && !s.list.isHeader(row) {
		s.list.cursor = row
		s.focus = false
	}
	return v.sync()
}

// copyText puts text on the clipboard: through pbcopy, wl-copy or xclip
// when one is installed, else with the terminal's OSC 52 sequence.
func copyText(text string) tea.Cmd {
	for _, c := range [][]string{{"pbcopy"}, {"wl-copy"}, {"xclip", "-selection", "clipboard"}} {
		if _, err := exec.LookPath(c[0]); err == nil {
			return func() tea.Msg {
				cmd := exec.Command(c[0], c[1:]...)
				cmd.Stdin = strings.NewReader(text)
				if err := cmd.Run(); err != nil {
					return statusMsg("copy failed: " + err.Error())
				}
				return statusMsg(fmt.Sprintf("copied the prompt (%d characters)", len([]rune(text))))
			}
		}
	}
	return tea.Batch(tea.SetClipboard(text), func() tea.Msg { return statusMsg("copied the prompt") })
}

func (v *viewer) View() tea.View {
	view := tea.NewView("")
	view.AltScreen = true
	view.MouseMode = tea.MouseModeCellMotion
	view.WindowTitle = "ccli"
	if v.w == 0 || v.h == 0 {
		return view
	}
	s := v.cur()
	lines := []string{v.header()}
	body := v.bodyHeight()
	switch {
	case v.help:
		lines = append(lines, v.helpLines(body)...)
	case s.full:
		for _, ln := range strings.Split(s.vp.View(), "\n")[:min(body, len(strings.Split(s.vp.View(), "\n")))] {
			lines = append(lines, fitLine(" "+ln, v.w))
		}
	case v.wide() && s.list.previews():
		lw := v.listWidth()
		left := s.list.view(lw, body, !s.focus)
		right := strings.Split(s.vp.View(), "\n")
		sep := dim("│")
		if s.focus {
			sep = accent("│")
		}
		for i := 0; i < body; i++ {
			r := ""
			if i < len(right) {
				r = right[i]
			}
			lines = append(lines, left[i]+sep+" "+fitLine(r, v.w-lw-2))
		}
	default:
		lines = append(lines, s.list.view(v.w, body, true)...)
	}
	for len(lines) < v.h-1 {
		lines = append(lines, "")
	}
	lines = append(lines[:v.h-1], v.footer())
	view.SetContent(strings.Join(lines, "\n"))
	return view
}

func (v *viewer) header() string {
	var crumbs []string
	for _, s := range v.stack {
		t := s.title
		if s.kind != "" && s.kind != "turns" {
			t = s.kind + " " + t
		}
		crumbs = append(crumbs, t)
	}
	left := accent("ccli") + dim(" › ") + strings.Join(crumbs, dim(" › "))
	s := v.cur()
	right := s.hint
	if s.rebuild != nil {
		if v.sortCost {
			right += " · by cost"
		}
		if v.allTurns {
			right += " · all turns"
		}
	}
	if f := s.list.filter; f != "" {
		right += fmt.Sprintf(" · /%s %d", f, len(s.list.shown))
	}
	right = dim(right)
	room := v.w - ansi.StringWidth(right) - 1
	return fitLine(ansi.Truncate(left, max(10, room), "…"), max(10, room)) + " " + right
}

func (v *viewer) footer() string {
	if v.filter {
		return fitLine(accent(" /")+v.query+"\x1b[7m \x1b[0m"+dim("  enter keeps it · esc clears"), v.w)
	}
	if v.status != "" {
		return fitLine(warn(v.status), v.w)
	}
	s := v.cur()
	var keys []string
	switch {
	case s.focus || s.full:
		keys = []string{"↑↓ scroll", "space page", "esc back", "t tools", "T thinking", "c context", "o agents", "y copy prompt"}
	default:
		keys = []string{"↑↓ move", "enter open", "/ filter"}
		if s.rebuild != nil {
			keys = append(keys, "s sort", "a all turns", "o agents", "y copy prompt", "t tools")
		} else if s.kind != "" || len(v.stack) > 1 {
			keys = append(keys, "o agents")
		}
		if len(v.stack) > 1 {
			keys = append(keys, "esc back")
		}
	}
	keys = append(keys, "? help", "q quit")
	return fitLine(dim(" "+strings.Join(keys, " · ")), v.w)
}

func (v *viewer) helpLines(n int) []string {
	help := []string{
		"",
		bold("  Moving"),
		"  ↑ ↓ j k        move in the list, or scroll the preview when it has focus",
		"  pgup pgdn      a page; g G the top and bottom; J K scroll the preview from the list",
		"  enter          open: a session's turns, a workflow run's agents; on a turn, focus its preview",
		"  tab → l        focus the preview; esc ← h goes back a level",
		"  mouse          the wheel scrolls the pane under the pointer; a click selects a row",
		"",
		bold("  Turns"),
		"  /              filter the list by what the prompt says",
		"  s              sort by cost (the turn's own calls plus the agents it started) or by order",
		"  a              include turns started by tasks, other sessions and the system",
		"  o              list the subagents and workflow runs the turn started",
		"  y              copy the prompt to the clipboard",
		"",
		bold("  Preview"),
		"  t              tool inputs and outputs     T  the model's thinking",
		"  c              text Claude Code added: skills, reminders, peer messages",
		"  r              prompts and replies as recorded, without markdown",
		"",
		dim("  any key closes this"),
	}
	out := make([]string, n)
	for i := range out {
		if i < len(help) {
			out[i] = fitLine(help[i], v.w)
		}
	}
	return out
}

// clamp cuts every line of s to at most w cells: a line wider than the
// preview would make the viewport pad all lines to it.
func clamp(s string, w int) string {
	// A tab measures as no width here but takes several cells on screen.
	lines := strings.Split(strings.ReplaceAll(s, "\t", "    "), "\n")
	for i, ln := range lines {
		if ansi.StringWidth(ln) > w {
			lines[i] = ansi.Truncate(ln, w, "…")
		}
	}
	return strings.Join(lines, "\n")
}
