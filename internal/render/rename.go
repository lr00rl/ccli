package render

import "github.com/lr00rl/ccli/internal/query"

// Renamed prints what `rename` did.
func (t Term) Renamed(r query.RenameResult) {
	id := query.ShortID(r.ID)
	if r.Cleared {
		title := r.Title
		if title == "" {
			title = "(untitled)"
		}
		t.printf("%s %s %s %s\n", t.money("✓"), id, t.dim("shows its own title again:"), title)
		return
	}
	t.printf("%s %s %s %s\n", t.money("✓"), id, t.dim("renamed to"), r.Title)
	if r.ToolTitle != "" && r.ToolTitle != r.Title {
		t.println(t.dim("  its own title: " + r.ToolTitle))
	}
}
