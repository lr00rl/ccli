package tui

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/lr00rl/ccli/internal/query"
)

// editLine is a line of text typed in the footer. Enter submits it; Esc
// leaves things as they were.
type editLine struct {
	prompt string
	text   string
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
		e.text += msg.Text
	}
	return nil
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

// askName puts the selected session's title in the footer to edit. The name
// is kept in ccli's index only (see query.Rename); an empty one removes it.
func (v *viewer) askName(s *screen) {
	idx := s.list.selected()
	if idx < 0 {
		return
	}
	it := s.list.all[idx]
	ref, current := it.id, it.title
	src, ctx := v.src, v.ctx
	v.edit = &editLine{prompt: "rename:", text: current, submit: func(text string) tea.Cmd {
		text = strings.Join(strings.Fields(text), " ")
		if text == current {
			v.status = "left as it was"
			return nil
		}
		v.status = "renaming…"
		return v.reloadSessions(s, v.showHidden, func() (string, error) {
			r, err := src.Rename(ctx, ref, text, time.Now())
			if err != nil {
				return "", err
			}
			if r.Cleared {
				return query.ShortID(r.ID) + " shows its own title again", nil
			}
			return "renamed " + query.ShortID(r.ID) + " in ccli", nil
		})
	}}
}
