package tui

import (
	"strings"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/lr00rl/ccli/internal/query"
)

// editLine is a line of text typed in the footer. Enter submits it; Esc
// leaves things as they were.
type editLine struct {
	prompt string
	text   string
	limit  int // the most characters it takes; 0 for no limit
	submit func(text string) tea.Cmd
}

// editKey types into the edit line.
func (v *viewer) editKey(e *editLine, msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "esc":
		v.edit = nil
		v.status = "left as it was"
	case "enter":
		v.edit = nil
		return e.submit(e.text)
	case "backspace":
		if r := []rune(e.text); len(r) > 0 {
			e.text = string(r[:len(r)-1])
		}
	case "ctrl+u":
		e.text = ""
	case "ctrl+c":
		return tea.Quit
	default:
		e.add(msg.Text)
	}
	return nil
}

// add types s at the end of the line, up to its limit.
func (e *editLine) add(s string) {
	if e.limit > 0 {
		if room := e.limit - utf8.RuneCountInString(e.text); room <= 0 {
			return
		} else if r := []rune(s); len(r) > room {
			s = string(r[:room])
		}
	}
	e.text += s
}

// editFooter shows the edit line, keeping the end of a long text in view.
func (v *viewer) editFooter(e *editLine) string {
	prompt, hint := accent(" "+e.prompt+" "), dim("  enter saves · esc cancels")
	text := e.text
	if room := v.w - ansi.StringWidth(prompt) - ansi.StringWidth(hint) - 1; room > 1 && ansi.StringWidth(text) > room {
		text = ansi.TruncateLeft(text, ansi.StringWidth(text)-room+1, "…")
	}
	return fitLine(prompt+text+"\x1b[7m \x1b[0m"+hint, v.w)
}

// askTag puts the selected session's tag in the footer to edit. The tag is
// kept in ccli's index only (see query.Tag); an empty one removes it.
func (v *viewer) askTag(s *screen) {
	idx := s.list.selected()
	if idx < 0 {
		return
	}
	it := s.list.all[idx]
	ref, current := it.id, it.tag
	src, ctx := v.src, v.ctx
	// A tag goes by session id, so every account's copy shows it.
	v.edit = &editLine{prompt: "tag (all accounts):", text: current, limit: query.MaxTag, submit: func(text string) tea.Cmd {
		text = strings.Join(strings.Fields(text), " ")
		if text == current {
			v.status = "left as it was"
			return nil
		}
		v.status = "tagging…"
		return v.reloadSessions(s, v.showHidden, func() (string, error) {
			r, err := src.Tag(ctx, ref, text, time.Now())
			if err != nil {
				return "", err
			}
			if r.Cleared {
				return "removed the tag of " + query.ShortID(r.ID), nil
			}
			return "tagged " + query.ShortID(r.ID) + " " + r.Tag, nil
		})
	}}
}
