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
		v.showHidden = !v.showHidden
		s.list.marked = nil
		if err := v.refresh(s); err != nil {
			v.showHidden = !v.showHidden
			v.status = err.Error()
			return nil, true
		}
		s.list.top()
		if v.showHidden && len(s.list.shown) == 0 {
			v.status = "no hidden sessions"
		}
		return v.sync(), true
	}
	return nil, false
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
	q := fmt.Sprintf("Hide %s? Lists leave them out; totals still count them.", what)
	if !hide {
		q = fmt.Sprintf("Bring back %s?", what)
	}
	v.confirm = &confirmation{question: q, yes: func() tea.Cmd { return v.setHidden(s, refs, hide) }}
}

func (v *viewer) setHidden(s *screen, refs []string, hide bool) tea.Cmd {
	var rs []query.HideResult
	var err error
	if hide {
		rs, err = v.src.Hide(v.ctx, refs, time.Now())
	} else {
		rs, err = v.src.Unhide(v.ctx, refs)
	}
	if err != nil {
		v.status = err.Error()
		return nil
	}
	s.list.marked = nil
	if err := v.refresh(s); err != nil {
		v.status = err.Error()
		return nil
	}
	v.status = hideSummary(rs, hide)
	return v.sync()
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
