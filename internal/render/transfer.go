package render

import (
	"fmt"
	"time"

	"github.com/lr00rl/ccli/internal/query"
	"github.com/lr00rl/ccli/internal/transfer"
)

// DiffRow is one session in a comparison of two accounts.
type DiffRow struct {
	transfer.Entry
	Title   string    `json:"title"`
	Project string    `json:"project"`
	Last    time.Time `json:"last"`
	Live    bool      `json:"live"`
}

// Diff prints the comparison of two accounts.
func (t Term) Diff(a, b string, rows []DiffRow, total int) {
	label := map[transfer.State]string{
		transfer.OnlyA:    "only in " + a,
		transfer.OnlyB:    "only in " + b,
		transfer.Same:     "identical",
		transfer.AAhead:   a + " is ahead",
		transfer.BAhead:   b + " is ahead",
		transfer.Diverged: "diverged",
	}
	counts := map[transfer.State]int{}
	var cells [][]string
	for _, r := range rows {
		counts[r.State]++
		state := label[r.State]
		switch r.State {
		case transfer.Diverged:
			state = t.warn(state)
		case transfer.AAhead, transfer.BAhead:
			state = t.accent(state)
		}
		id := query.ShortID(r.SessionID)
		if r.Live {
			id = t.paint(cLive, id+"●")
		}
		title := r.Title
		if title == "" {
			title = t.dim("(untitled)")
		}
		project := r.Project
		if project == "" {
			project = r.ProjectKey
		}
		cells = append(cells, []string{id, t.Ago(r.Last), project, title, state, sizes(r.ASize, r.BSize)})
	}
	if len(cells) == 0 {
		t.printf("%s and %s hold the same %d sessions.\n", a, b, total)
		return
	}
	t.table([]column{{title: "ID"}, {title: "LAST"}, {title: "PROJECT", flex: true, min: 10}, {title: "TITLE", flex: true, min: 16},
		{title: "STATE"}, {title: "SIZE " + a + "/" + b, right: true}}, cells)
	t.println(t.dim(fmt.Sprintf("%d only in %s · %d only in %s · %d %s ahead · %d %s ahead · %d diverged",
		counts[transfer.OnlyA], a, counts[transfer.OnlyB], b, counts[transfer.AAhead], a, counts[transfer.BAhead], b, counts[transfer.Diverged])))
	// Suggest each direction that has sessions to bring over.
	for _, d := range []struct {
		from, to string
		n        int
	}{{a, b, counts[transfer.OnlyA] + counts[transfer.AAhead]}, {b, a, counts[transfer.OnlyB] + counts[transfer.BAhead]}} {
		if d.n > 0 {
			t.println(t.dim(fmt.Sprintf("To bring %s's sessions into %s: ccli copy --from %s --to %s  (prints the plan; add --apply)", d.from, d.to, d.from, d.to)))
		}
	}
}

func sizes(a, b int64) string {
	f := func(n int64) string {
		if n == 0 {
			return "-"
		}
		return humanSize(n)
	}
	return f(a) + " / " + f(b)
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1fG", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1fM", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0fK", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%dB", n)
}

// Plan prints what copy or move would do.
func (t Term) Plan(verb string, ops []transfer.Op, info map[string]query.SessionRow, archive string) {
	var cells [][]string
	ready := 0
	for _, op := range ops {
		s := info[op.From+"/"+op.SessionID]
		action := string(op.Action)
		switch op.Action {
		case transfer.Refuse:
			action = t.warn("refuse: " + op.Reason)
		case transfer.UpToDate:
			action = t.dim("up to date")
		default:
			ready++
			action = t.accent(action)
		}
		title := s.Title
		if title == "" {
			title = t.dim("(untitled)")
		}
		cells = append(cells, []string{query.ShortID(op.SessionID), op.From + " → " + op.To, s.Project, title, humanSize(op.Bytes), action})
	}
	t.table([]column{{title: "ID"}, {title: "DIRECTION"}, {title: "PROJECT", flex: true, min: 10}, {title: "TITLE", flex: true, min: 16},
		{title: "BYTES", right: true}, {title: "ACTION", flex: true, min: 20}}, cells)
	extra := ""
	if verb == "move" {
		extra = " Move puts each source in " + shortHome(archive) + " instead of deleting it."
	}
	t.println(t.dim(fmt.Sprintf("Dry run: %d of %d sessions would %s. Add --apply to do it.%s", ready, len(ops), verb, extra)))
}

// Results prints what copy or move did.
func (t Term) Results(verb string, rs []transfer.Result, info map[string]query.SessionRow) {
	done := 0
	for _, r := range rs {
		op := r.Op
		s := info[op.From+"/"+op.SessionID]
		title := fit(s.Title, 50)
		switch op.Action {
		case transfer.Refuse:
			t.printf("%s %s %s\n", t.warn("✗"), query.ShortID(op.SessionID), t.warn(op.Reason))
		default:
			done++
			msg := fmt.Sprintf("%s %s → %s, %d files written", op.Action, op.From, op.To, len(r.Written))
			if len(r.Skipped) > 0 {
				msg += fmt.Sprintf(", %d unchanged", len(r.Skipped))
			}
			if len(r.Conflicts) > 0 {
				msg += t.warn(fmt.Sprintf(", %d conflicts left alone", len(r.Conflicts)))
			}
			if r.Archive != "" {
				msg += ", source archived"
			}
			t.printf("%s %s %s %s\n", t.money("✓"), query.ShortID(op.SessionID), title, t.dim(msg))
			for _, c := range r.Conflicts {
				t.printf("    %s %s\n", t.warn("conflict"), shortHome(c))
			}
			if r.Archive != "" {
				t.printf("    %s %s\n", t.dim("archive"), shortHome(r.Archive))
			}
		}
	}
	past := map[string]string{"copy": "copied", "move": "moved"}[verb]
	t.println(t.dim(fmt.Sprintf("%d of %d sessions %s.", done, len(rs), past)))
}

// ForkPlan prints what fork would do.
func (t Term) ForkPlan(p transfer.ForkPlan, s query.SessionRow) {
	title := s.Title
	if title == "" {
		title = "(untitled)"
	}
	t.println(t.bold(title))
	if p.Reason != "" {
		t.println(t.warn("cannot fork " + query.ShortID(p.SessionID) + ": " + p.Reason))
		return
	}
	t.printf("fork %s from %s into %s as a new session %s\n", query.ShortID(p.SessionID), p.From, p.To, query.ShortID(p.NewSessionID))
	writes := "a " + humanSize(p.Bytes) + " transcript"
	if p.Folders > 0 {
		writes += " and " + plural(p.Folders, "session folder")
	}
	t.println(t.dim("  writes " + writes + "; the original is not touched"))
	t.println(t.dim("  usage already in the transcript stays with " + p.From + "; only new work counts toward " + p.To))
	t.println(t.dim("Dry run. Add --apply to do it (a new id is drawn each time)."))
}

// Forked prints what fork did and how to go on.
func (t Term) Forked(p transfer.ForkPlan, r transfer.Result) {
	t.printf("%s forked %s → %s as %s, %d files written\n", t.money("✓"), p.From, p.To, p.NewSessionID, len(r.Written))
	for _, c := range r.Conflicts {
		t.printf("    %s %s\n", t.warn("left alone"), shortHome(c))
	}
	t.println(t.dim("continue it with: ccli resume " + query.ShortID(p.NewSessionID)))
}
