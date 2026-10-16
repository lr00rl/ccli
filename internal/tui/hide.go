package tui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/lr00rl/ccli/internal/query"
	"github.com/lr00rl/ccli/internal/render"
)

// confirmation is a yes-or-no question in the footer; the next key answers
// it.
type confirmation struct {
	question string
	yes      func() tea.Cmd
}

// sessionKey handles the keys that act on sessions in the sessions list:
// marking, hiding and bringing hidden ones back. ok is false for any other
// key. Hiding only leaves sessions out of lists (see query.Hide).
func (v *viewer) sessionKey(s *screen, k string) (cmd tea.Cmd, ok bool) {
	switch k {
	case "m":
		if idx := s.list.selected(); idx >= 0 {
			s.list.toggleMark(s.list.all[idx].id)
			s.list.move(1)
		}
		return v.sync(), true
	case "x":
		if v.showHidden {
			v.status = "these are hidden already; u brings them back"
			return nil, true
		}
		v.ask(s, true)
		return nil, true
	case "u":
		if !v.showHidden {
			v.status = "H lists the hidden sessions, where u brings them back"
			return nil, true
		}
		v.ask(s, false)
		return nil, true
	case "H":
		return v.reloadSessions(s, !v.showHidden, nil), true
	case "#":
		v.askTag(s)
		return nil, true
	}
	return nil, false
}

// sessionsLoaded is the sessions list read again off the update loop.
type sessionsLoaded struct {
	scr    *screen
	hidden bool // the list holds the hidden sessions
	page   query.SessionPage
	err    error
	status string // what the work before the read did, for the footer
	done   bool   // that work is done: the marks it acted on go
	other  bool   // the list asked for is the other one: start at its top
}

// reloadSessions reads the sessions list, the hidden sessions or the others,
// off the update loop, after running work (hiding, say) there too. Reading
// the store in the update loop would freeze the screen while it is busy.
func (v *viewer) reloadSessions(s *screen, hidden bool, work func() (string, error)) tea.Cmd {
	src, ctx, f := v.src, v.ctx, v.sessionFilter(hidden)
	other := hidden != v.showHidden
	return func() tea.Msg {
		msg := sessionsLoaded{scr: s, hidden: hidden, other: other}
		if work != nil {
			if msg.status, msg.err = work(); msg.err != nil {
				return msg
			}
			msg.done = true
		}
		msg.page, msg.err = src.Sessions(ctx, f)
		return msg
	}
}

// applySessions shows a sessions list that was read off the update loop.
func (v *viewer) applySessions(msg sessionsLoaded) tea.Cmd {
	s := msg.scr
	if msg.status != "" {
		v.status = msg.status
	}
	if msg.done {
		s.list.marked = nil
	}
	if msg.err != nil {
		v.status = msg.err.Error()
		return nil
	}
	switch {
	case msg.other:
		v.showHidden = msg.hidden
		s.list.marked = nil
	case msg.hidden != v.showHidden:
		return nil // the other list is showing now
	}
	v.setSessions(s, msg.page, msg.hidden)
	if msg.other {
		s.list.top()
		if msg.hidden && len(s.list.shown) == 0 {
			v.status = "no hidden sessions"
		}
	}
	if s != v.cur() {
		return nil
	}
	return v.sync()
}

// ask puts the question of hiding (or bringing back) the marked sessions,
// or the selected one, in the footer.
func (v *viewer) ask(s *screen, hide bool) {
	refs := s.list.targets()
	if len(refs) == 0 {
		return
	}
	what := "this session"
	if len(refs) > 1 {
		what = fmt.Sprintf("%d sessions", len(refs))
	}
	if n := s.list.offScreen(); n > 0 {
		what += fmt.Sprintf(" (%d not in the current filter)", n)
	}
	q := fmt.Sprintf("Hide %s? Lists leave them out; totals still count them.", what)
	if !hide {
		q = fmt.Sprintf("Bring back %s?", what)
	}
	v.confirm = &confirmation{question: q, yes: func() tea.Cmd { return v.setHidden(s, refs, hide) }}
}

// setHidden hides the sessions refs name, or brings them back, and lists the
// sessions again; both run off the update loop.
func (v *viewer) setHidden(s *screen, refs []string, hide bool) tea.Cmd {
	src, ctx := v.src, v.ctx
	v.status = "hiding…"
	if !hide {
		v.status = "bringing back…"
	}
	return v.reloadSessions(s, v.showHidden, func() (string, error) {
		var rs []query.HideResult
		var err error
		if hide {
			rs, err = src.Hide(ctx, refs, time.Now())
		} else {
			rs, err = src.Unhide(ctx, refs)
		}
		if err != nil {
			return "", err
		}
		return hideSummary(rs, hide), nil
	})
}

// hideSummary says in one line what hiding or bringing back did.
func hideSummary(rs []query.HideResult, hide bool) string {
	done := 0
	var left []string
	for _, r := range rs {
		if r.Done {
			done++
			continue
		}
		left = append(left, query.ShortID(r.ID)+" "+r.Reason)
	}
	msg := "hid " + render.Plural(done, "session")
	if !hide {
		msg = "brought back " + render.Plural(done, "session")
	}
	if len(left) > 0 {
		msg += " · left as they were: " + strings.Join(left, ", ")
	}
	if hide && done > 0 {
		msg += " · H lists hidden sessions"
	}
	return msg
}
