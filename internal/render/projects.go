package render

import (
	"fmt"
	"strings"

	"github.com/lr00rl/ccli/internal/query"
)

// Projects prints the folders sessions started in, relative to the folder
// asked about.
func (t Term) Projects(p query.ProjectPage) {
	scope := "anywhere"
	if len(p.Filter.Dirs) > 0 {
		scope = ShortHome(p.Filter.Dirs[0])
	}
	when := "all time"
	if !p.Filter.Since.IsZero() {
		when = "since " + p.Filter.Since.In(p.Filter.Loc).Format("2006-01-02")
	}
	if len(p.Projects) == 0 {
		t.println(fmt.Sprintf("No sessions started in %s or below (%s).", scope, when))
		return
	}
	cols := []column{
		{title: "LAST"}, {title: "SESSIONS", right: true}, {title: "PROMPTS", right: true},
		{title: "TOKENS", right: true}, {title: "COST", right: true},
		{title: "ACCOUNTS", flex: true, min: 8}, {title: "PROJECT", flex: true, min: 12},
	}
	live := false
	var rows [][]string
	for _, pr := range p.Projects {
		n := fmt.Sprint(pr.Sessions)
		if pr.Live > 0 {
			n = t.paint(cLive, "●") + n
			live = true
		}
		path := pr.Rel
		if strings.HasPrefix(path, "/") {
			path = ShortHome(path)
		}
		rows = append(rows, []string{t.Ago(pr.Last), n, fmt.Sprint(pr.Prompts), Num(pr.Tokens.InOut()),
			t.money(costText(pr.Cost)), strings.Join(pr.Accounts, ","), path})
	}
	t.table(cols, rows)
	more := ""
	if p.Matched > len(p.Projects) {
		more = fmt.Sprintf(", showing %d (use -n)", len(p.Projects))
	}
	t.println(t.dim(fmt.Sprintf("%s under %s, %s%s · %s · %s tokens · %s list price",
		plural(p.Matched, "project"), scope, when, more, plural(p.Sessions, "session"),
		Num(p.Tokens.InOut()), costText(p.Cost))))
	if live {
		t.println(t.dim("● a session there is running now   ccli sessions --dir <folder> lists its sessions"))
	} else {
		t.println(t.dim("ccli sessions --dir <folder> lists a project's sessions"))
	}
}
