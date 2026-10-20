package conv

import "strings"

// ReplyEntries keeps every prompt, then the turn's last reply. The last reply
// is the assistant text after the last tool call, or the text that closed the
// turn when the tools were recorded after it. Notices after that reply stay,
// so a turn that was interrupted or failed says so. Earlier replies, earlier
// notices and every tool stay out.
func ReplyEntries(entries []Entry) []Entry {
	var prompts, group, last, notes []Entry
	closeGroup := func() {
		if len(group) > 0 {
			last = group
			group = nil
		}
	}
	for _, e := range entries {
		switch e.Kind {
		case KindPrompt, KindQueued:
			prompts = append(prompts, e)
		case KindText:
			if strings.TrimSpace(e.Text) == "" {
				continue
			}
			if len(group) == 0 {
				notes = nil // a later reply: what went before it is history
			}
			group = append(group, e)
		case KindNotice:
			closeGroup()
			notes = append(notes, e)
		default:
			closeGroup()
		}
	}
	closeGroup()
	return append(append(prompts, last...), notes...)
}
