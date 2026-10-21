package render

import (
	"fmt"
	"time"

	"github.com/lr00rl/ccli/internal/query"
)

// CleanRow is one session in a `clean` plan or result.
type CleanRow struct {
	Action  string    `json:"action"` // archive, forget or hide
	Account string    `json:"account"`
	ID      string    `json:"session_id"`
	Title   string    `json:"title,omitempty"`
	Cwd     string    `json:"cwd,omitempty"`
	Last    time.Time `json:"last"`
	Prompts int       `json:"prompts,omitempty"`
	Done    bool      `json:"done,omitempty"` // applied
	Note    string    `json:"note,omitempty"` // why it was not done, when applied
}

// CleanPlan prints the sessions `clean` found and what it does with each.
func (t Term) CleanPlan(rows []CleanRow) {
	if len(rows) == 0 {
		t.println("No empty sessions.")
		return
	}
	// Prompts and titles appear only with --unanswered; without them the
	// columns would stay empty.
	prompts, titles := false, false
	for _, r := range rows {
		prompts = prompts || r.Prompts > 0
		titles = titles || r.Title != ""
	}
	cols := []column{{title: "LAST"}, {title: "ACCOUNT"}, {title: "ID"}, {title: "DO"}}
	if prompts {
		cols = append(cols, column{title: "PROMPTS", right: true})
	}
	cols = append(cols, column{title: "PROJECT", flex: true, min: 8})
	if titles {
		cols = append(cols, column{title: "TITLE", flex: true, min: 12})
	}
	var out [][]string
	for _, r := range rows {
		row := []string{t.Ago(r.Last), r.Account, query.ShortID(r.ID), r.Action}
		if prompts {
			row = append(row, fmt.Sprint(r.Prompts))
		}
		row = append(row, ShortHome(r.Cwd))
		if titles {
			row = append(row, r.Title)
		}
		out = append(out, row)
	}
	t.table(cols, out)
}

// CleanDone prints what an applied `clean` did.
func (t Term) CleanDone(rows []CleanRow, undo string) {
	n := map[string]int{}
	for _, r := range rows {
		if r.Note != "" {
			t.printf("%s %s %s\n", t.warn("·"), query.ShortID(r.ID), t.warn(r.Note))
		}
		if r.Done {
			n[r.Action]++
		}
	}
	t.printf("%s %d archived, %d hidden, %d forgotten\n", t.money("✓"), n["archive"], n["hide"], n["forget"])
	if undo != "" {
		t.println(t.dim("Undo with: ccli clean --undo " + undo))
	}
}
