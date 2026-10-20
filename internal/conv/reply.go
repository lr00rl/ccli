package conv

import "strings"

// ReplyEntries keeps every prompt, then the turn's last reply. The last reply
// is the assistant text after the last tool call, or the text that closed the
// turn when the tools were recorded after it. Earlier replies and every tool
// stay out.
func ReplyEntries(entries []Entry) []Entry {
	var prompts, group, last []Entry
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
			group = append(group, e)
		default:
			closeGroup()
		}
	}
	closeGroup()
	return append(prompts, last...)
}
