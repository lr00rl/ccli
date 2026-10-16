package render

import "github.com/lr00rl/ccli/internal/query"

// Tagged prints what `tag` did.
func (t Term) Tagged(r query.TagResult) {
	id := query.ShortID(r.ID)
	title := r.Title
	if title == "" {
		title = "(untitled)"
	}
	if r.Cleared {
		t.printf("%s %s %s %s\n", t.money("✓"), id, t.dim("tag removed:"), title)
		return
	}
	t.printf("%s %s %s %s %s\n", t.money("✓"), id, t.dim("tagged"), t.tag(r.Tag), t.dim("· "+title))
}
