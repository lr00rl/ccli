package render

import (
	"fmt"

	"github.com/lr00rl/ccli/internal/query"
)

// HideResults prints what `hide` or `unhide` did with each session; verb is
// what was done ("hidden", "brought back").
func (t Term) HideResults(verb string, rs []query.HideResult) {
	done := 0
	for _, r := range rs {
		id := r.Ref
		if r.ID != "" {
			id = query.ShortID(r.ID)
		}
		title := r.Title
		if title == "" && r.ID != "" {
			title = "(untitled)"
		}
		if title != "" {
			id += " " + fit(title, max(20, t.Width-30))
		}
		if r.Done {
			done++
			t.printf("%s %s %s\n", t.money("✓"), id, t.dim(verb))
			continue
		}
		t.printf("%s %s %s\n", t.warn("·"), id, t.warn(r.Reason))
	}
	if len(rs) > 1 {
		t.println(t.dim(fmt.Sprintf("%d of %d sessions %s", done, len(rs), verb)))
	}
}
