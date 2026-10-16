package render

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/lr00rl/ccli/internal/query"
)

// Accounts prints the account table.
func (t Term) Accounts(rows []query.AccountSummary, days int) {
	span := "all time"
	if days > 0 {
		span = fmt.Sprintf("last %d days", days)
	}
	var cells [][]string
	for _, a := range rows {
		email := a.Email
		if email == "" {
			email = t.dim("(not signed in)")
		}
		live := ""
		if a.Live > 0 {
			live = t.paint(cLive, fmt.Sprintf("● %d", a.Live))
		}
		cells = append(cells, []string{
			t.bold(a.Name), email, shortHome(a.Dir),
			fmt.Sprintf("%d", a.Sessions), fmt.Sprintf("%d", a.SessionsInRange),
			Num(a.Tokens.InOut()), Num(a.Tokens.CacheRead), t.money(USD(a.Cost.USD)),
			t.Ago(a.LastActive), live,
		})
	}
	t.table([]column{
		{title: "ACCOUNT"}, {title: "EMAIL", flex: true, min: 14}, {title: "DIR", flex: true, min: 10},
		{title: "SESSIONS", right: true}, {title: "ACTIVE", right: true},
		{title: "TOKENS", right: true}, {title: "CACHE READ", right: true}, {title: "COST", right: true},
		{title: "LAST"}, {title: "LIVE"},
	}, cells)
	t.println(t.dim(fmt.Sprintf("ACTIVE, TOKENS, CACHE READ and COST cover the %s. Cost is list price; subscriptions are not billed per token.", span)))
}

// Sessions prints a session list.
func (t Term) Sessions(p query.SessionPage, showAccount bool) {
	if len(p.Sessions) == 0 {
		t.println("No sessions match.")
		return
	}
	cols := []column{{title: "LAST"}}
	if showAccount {
		cols = append(cols, column{title: "ACCOUNT"})
	}
	cols = append(cols,
		column{title: "ID"}, column{title: "PROJECT", flex: true, min: 8}, column{title: "TITLE", flex: true, min: 16},
		column{title: "MODEL", flex: true, min: 8}, column{title: "ACTIVE", right: true},
		column{title: "TOKENS", right: true}, column{title: "CACHED", right: true}, column{title: "COST", right: true},
	)
	var rows [][]string
	for _, s := range p.Sessions {
		r := []string{t.Ago(s.Last)}
		if showAccount {
			r = append(r, s.Account)
		}
		id := query.ShortID(s.ID)
		switch {
		case s.Live:
			id = t.paint(cLive, id+"●")
		case s.Missing:
			id = t.dim(id + "×")
		}
		ttl := s.Title
		if ttl == "" {
			ttl = t.dim("(untitled)")
		}
		if s.AgentName != "" {
			ttl = t.dim("["+s.AgentName+"] ") + ttl
		}
		if n := s.Subagents; n > 0 {
			ttl += t.dim(fmt.Sprintf(" +%d agents", n))
		}
		r = append(r, id, s.Project, ttl, shortModels(s.Models), Dur(s.ActiveMS),
			Num(s.Tokens.InOut()), Num(s.Tokens.CacheRead), t.money(costText(s.Cost)))
		rows = append(rows, r)
	}
	t.table(cols, rows)
	more := ""
	if p.Matched > len(p.Sessions) {
		more = fmt.Sprintf(", showing %d (use -n)", len(p.Sessions))
	}
	count := plural(p.Matched, "session")
	if p.Filter.Show == query.HiddenOnly {
		count = plural(p.Matched, "hidden session")
	}
	t.println(t.dim(fmt.Sprintf("%s%s · %s tokens · %s cached · %s list price", count, more,
		Num(p.Tokens.InOut()), Num(p.Tokens.CacheRead), costText(p.Cost))))
	if p.Hidden > 0 {
		t.println(t.dim(fmt.Sprintf("%s left out: ccli sessions --hidden lists them", plural(p.Hidden, "hidden session"))))
	}
	t.println(t.dim("● running   × transcript deleted (history kept)   + unpriced models left out"))
}

func costText(c query.Cost) string {
	s := USD(c.USD)
	if c.Unpriced > 0 {
		s += "+"
	}
	return s
}

func shortModels(ms []string) string {
	var out []string
	for _, m := range ms {
		m = strings.TrimPrefix(m, "claude-")
		if i := strings.IndexByte(m, '['); i > 0 {
			m = m[:i]
		}
		out = append(out, m)
	}
	out = dedup(out)
	if len(out) > 2 {
		return out[0] + " +" + fmt.Sprint(len(out)-1)
	}
	return strings.Join(out, ",")
}

func dedup(ss []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func shortHome(p string) string {
	if h, err := os.UserHomeDir(); err == nil && h != "" && strings.HasPrefix(p, h) {
		return "~" + strings.TrimPrefix(p, h)
	}
	return p
}

// Session prints one session's detail.
func (t Term) Session(d *query.SessionDetail, all bool) {
	ttl := d.Title
	if ttl == "" {
		ttl = "(untitled)"
	}
	t.println(t.bold(ttl))
	state := ""
	switch {
	case d.Live:
		state = t.paint(cLive, " ● running")
	case d.Missing:
		state = t.warn(" transcript deleted, history kept")
	}
	if d.Hidden {
		state += t.dim(" · hidden from lists (ccli unhide brings it back)")
	}
	t.println(t.dim(d.ID+" · "+d.Account) + state)
	t.println()
	kv := func(k, v string) { t.printf("  %-11s %s\n", t.dim(k), v) }
	kv("project", shortHome(d.Cwd))
	switch d.GitBranch {
	case "":
	case "HEAD":
		kv("branch", "detached HEAD")
	default:
		kv("branch", d.GitBranch)
	}
	end := d.Last.In(t.Loc).Format("01-02 15:04")
	if d.Last.In(t.Loc).Year() != d.Started.In(t.Loc).Year() {
		end = d.Last.In(t.Loc).Format("2006-01-02 15:04")
	}
	kv("time", fmt.Sprintf("%s → %s  (active %s)", d.Started.In(t.Loc).Format("2006-01-02 15:04"), end, Dur(d.ActiveMS)))
	prompts := fmt.Sprintf("%d typed", d.Prompts)
	if d.OtherPrompts > 0 {
		prompts += fmt.Sprintf(", %d from tasks and other sessions", d.OtherPrompts)
	}
	kv("prompts", prompts+fmt.Sprintf(" · %d replies · %d tool calls", d.Replies, d.ToolCalls))
	kv("tokens", fmt.Sprintf("%s in · %s out · %s cache read · %s cache write", Num(d.Tokens.Input), Num(d.Tokens.Output), Num(d.Tokens.CacheRead), Num(d.Tokens.CacheWrite)))
	cost := t.money(costText(d.Cost)) + t.dim(" list price")
	if d.Cost.Unlogged > 0 {
		cost += t.dim(fmt.Sprintf(" (includes %s of calls Claude Code billed but did not write to the transcript)", USD(d.Cost.Unlogged)))
	}
	kv("cost", cost)
	if d.Team != "" {
		kv("team", d.Team+agentSuffix(d.AgentName))
	}
	if d.Version != "" {
		kv("client", strings.TrimSpace(d.Entrypoint+" "+d.Version))
	}

	if len(d.ByModel) > 0 {
		t.println()
		t.println(t.accent("Models"))
		var rows [][]string
		for _, m := range d.ByModel {
			name := m.Name
			if strings.HasPrefix(name, "adj|") {
				continue
			}
			rows = append(rows, []string{"  " + name, fmt.Sprint(m.Sessions), Num(m.Tokens.InOut()), Num(m.Tokens.CacheRead), t.money(USD(m.Cost))})
		}
		t.table([]column{{title: "  MODEL"}, {title: "CALLS", right: true}, {title: "TOKENS", right: true}, {title: "CACHED", right: true}, {title: "COST", right: true}}, rows)
	}

	if len(d.Files) > 1 || len(d.Runs) > 0 {
		t.println()
		t.println(t.accent("Agents"))
		t.tree(d, all)
	}

	if len(d.Links) > 0 {
		t.println()
		t.println(t.accent("Related sessions"))
		for _, l := range d.Links {
			label := map[string]string{
				"continues-in": "continues in", "continued-from": "continued from",
				"team-lead": "team lead", "teammate": "teammate", "copy": "copy in",
			}[l.Kind]
			extra := ""
			if l.Detail != "" && l.Kind == "teammate" {
				extra = t.dim(" [" + l.Detail + "]")
			}
			t.printf("  %-15s %s %s%s %s\n", t.dim(label), l.SessionID[:min(8, len(l.SessionID))], t.dim(l.Account), extra, fit(l.Title, max(20, t.Width-40)))
		}
	}

	if len(d.Moves) > 0 {
		t.println()
		t.println(t.accent("Copies between accounts"))
		for _, m := range d.Moves {
			line := fmt.Sprintf("  %s  %s %s → %s", t.dim(m.At.In(t.Loc).Format("2006-01-02 15:04")), m.Mode, m.From, m.To)
			if m.NewSessionID != "" {
				line += " as " + query.ShortID(m.NewSessionID)
			}
			t.println(line + t.dim(fmt.Sprintf("  %s in %s", humanSize(m.Bytes), plural(m.Files, "file"))))
		}
	}

	if len(d.Skills) > 0 || len(d.Tools) > 0 || len(d.Commands) > 0 {
		t.println()
		t.println(t.accent("Tools"))
		if len(d.Tools) > 0 {
			t.printf("  %s\n", counts(d.Tools, 8))
		}
		if len(d.Skills) > 0 {
			t.printf("  %s %s\n", t.dim("skills the model called"), counts(d.Skills, 8))
		}
		if len(d.Commands) > 0 {
			t.printf("  %s %s\n", t.dim("commands you ran"), counts(d.Commands, 8))
		}
	}

	t.println()
	t.println(t.accent("Turns"))
	t.printf("  %s\n", t.dim("ccli log "+query.ShortID(d.ID)+" lists every prompt with what it cost · ccli view "+query.ShortID(d.ID)+" browses them"))
	t.println()

	t.println(t.accent("Resume"))
	if d.Resume.Available {
		t.printf("  %s\n", d.Resume.Command)
		t.printf("  %s\n", t.dim("or: ccli resume "+query.ShortID(d.ID)))
	} else {
		t.printf("  %s\n", t.warn(d.Resume.Reason))
	}
}

func agentSuffix(name string) string {
	if name == "" {
		return ""
	}
	return " as " + name
}

func counts(cs []query.Count, n int) string {
	var parts []string
	for i, c := range cs {
		if i == n {
			parts = append(parts, fmt.Sprintf("+%d more", len(cs)-n))
			break
		}
		parts = append(parts, fmt.Sprintf("%s %d", c.Name, c.N))
	}
	return strings.Join(parts, " · ")
}

// tree prints the main transcript, subagents nested by who spawned them, and
// workflow runs, with a line that adds the parts up to the session's cost.
func (t Term) tree(d *query.SessionDetail, all bool) {
	children := map[string][]query.FileInfo{} // parent agent id ("" = main) -> subagents
	known := map[string]bool{}
	var main *query.FileInfo
	var subCost float64
	subs := 0
	for i := range d.Files {
		f := d.Files[i]
		switch f.Kind {
		case "main":
			main = &d.Files[i]
		case "subagent":
			known[f.AgentID] = true
			subs++
			subCost += f.Cost
		}
	}
	for _, f := range d.Files {
		if f.Kind != "subagent" {
			continue
		}
		parent := f.Parent
		if !known[parent] {
			parent = "" // its parent's transcript is gone: show it at the top
		}
		children[parent] = append(children[parent], f)
	}
	var runCost float64
	for _, r := range d.Runs {
		runCost += r.Cost
	}
	parts := []string{}
	if main != nil {
		parts = append(parts, "main "+USD(main.Cost))
	}
	if subs > 0 {
		parts = append(parts, fmt.Sprintf("%s %s", plural(subs, "subagent"), USD(subCost)))
	}
	if len(d.Runs) > 0 {
		parts = append(parts, fmt.Sprintf("%s %s", plural(len(d.Runs), "workflow run"), USD(runCost)))
	}
	if d.Cost.Unlogged > 0 {
		parts = append(parts, "unlogged "+USD(d.Cost.Unlogged))
	}
	t.printf("  %s\n", t.dim(strings.Join(parts, " · ")))

	limit, shown, hidden := 30, 0, 0
	if all {
		limit = 1 << 30
	}
	var walk func(parent, indent string, depth int)
	walk = func(parent, indent string, depth int) {
		kids := children[parent]
		sort.Slice(kids, func(i, j int) bool { return kids[i].Started.Before(kids[j].Started) })
		for i, k := range kids {
			if shown >= limit {
				hidden++
				walk(k.AgentID, indent, depth+1)
				continue
			}
			shown++
			branch, next := "├─ ", "│  "
			if i == len(kids)-1 {
				branch, next = "└─ ", "   "
			}
			extra := ""
			if k.Shape == "background" {
				extra += " bg"
			}
			if k.Turn > 0 {
				extra += fmt.Sprintf(" · turn %d", k.Turn)
			}
			extra += fmt.Sprintf(" · %s tokens · %s", Num(k.Tokens.InOut()), USD(k.Cost))
			if k.Missing {
				extra += " ×"
			}
			label := fit(shortPaths(query.AgentLabel(k)), max(24, t.Width-len(indent)-width(extra)-6))
			t.printf("%s%s%s%s\n", indent, t.dim(branch), label, t.dim(extra))
			if depth < 16 {
				walk(k.AgentID, indent+t.dim(next), depth+1)
			}
		}
	}
	walk("", "  ", 0)
	if hidden > 0 {
		t.printf("  %s\n", t.dim(fmt.Sprintf("… %d more agents (ccli show --all %s)", hidden, query.ShortID(d.ID))))
	}
	shownRuns := 0
	for _, r := range d.Runs {
		if shownRuns >= limit {
			break
		}
		shownRuns++
		when := ""
		if !r.At.IsZero() {
			when = r.At.In(t.Loc).Format("01-02 15:04") + " "
		}
		turn := ""
		if r.Turn > 0 {
			turn = fmt.Sprintf(" · turn %d", r.Turn)
		}
		t.printf("  %s%s\n", t.dim(when), runLine(t, r)+t.dim(turn))
	}
	if len(d.Runs) > shownRuns {
		t.printf("  %s\n", t.dim(fmt.Sprintf("… %d more workflow runs (ccli show --all %s)", len(d.Runs)-shownRuns, query.ShortID(d.ID))))
	}
}

// Overview prints the dashboard.
func (t Term) Overview(o *query.Overview, days int) {
	span := "all time"
	if days > 0 {
		span = fmt.Sprintf("last %d days", days)
	}
	accts := "all accounts"
	if len(o.Filter.Accounts) > 0 {
		accts = strings.Join(o.Filter.Accounts, ", ")
	}
	t.println(t.bold("Claude Code usage") + t.dim(" · "+accts+" · "+span))
	t.println()
	tiles := [][2]string{
		{"SESSIONS", fmt.Sprintf("%d", o.Sessions)},
		{"TOKENS", Num(o.Tokens.InOut())},
		{"COST", costText(o.Cost)},
		{"CACHE READ", Num(o.Tokens.CacheRead)},
		{"ACTIVE", Dur(o.ActiveMS)},
		{"PROJECTS", fmt.Sprintf("%d", o.Projects)},
	}
	subs := []string{
		fmt.Sprintf("median %s · p90 %s", Num(o.MedianTokens), Num(o.P90Tokens)),
		fmt.Sprintf("in %s · out %s", Num(o.Tokens.Input), Num(o.Tokens.Output)),
		"list price",
		fmt.Sprintf("hit rate %.0f%%", o.CacheHitRate*100),
		fmt.Sprintf("on %d days", o.ActiveDays),
		"",
	}
	t.tiles(tiles, subs)
	if o.Cost.Unlogged > 0 {
		t.println(t.dim(fmt.Sprintf("Cost includes %s that Claude Code billed for calls it does not write to transcripts.", USD(o.Cost.Unlogged))))
	}
	t.println()
	t.daily(o.Daily)
	t.println()
	half := (t.Width - 4) / 2
	if half < 34 {
		t.shares("Projects", o.ByProject, t.Width-2)
		t.println()
		t.shares("Models", o.ByModel, t.Width-2)
	} else {
		left := t.sharesLines("Projects", o.ByProject, half)
		right := t.sharesLines("Models", o.ByModel, half)
		t.sideBySide(left, right, half)
	}
	if len(o.ByAccount) > 1 {
		t.println()
		t.shares("Accounts", o.ByAccount, min(t.Width-2, 90))
	}
	t.println()
	t.heatmap(o.Heatmap)
	t.println()
	t.shape(o.Shape)
	if len(o.Skills) > 0 {
		t.println()
		t.println(t.accent("Skills"))
		top := o.Skills
		if len(top) > 8 {
			top = top[:8]
		}
		maxUses := top[0].Uses
		for _, s := range top {
			t.printf("  %s %s %s\n", pad(fit(s.Name, 26), 26, false), pad(t.accent(bar(float64(s.Uses)/float64(maxUses), 20)), 20, false),
				t.dim(plural(s.Uses, "use")+" · "+plural(s.Sessions, "session")))
		}
	}
}

func (t Term) tiles(tiles [][2]string, subs []string) {
	per := len(tiles)
	cell := (t.Width - 2) / per
	if cell < 24 {
		per = 3
		cell = (t.Width - 2) / per
	}
	for start := 0; start < len(tiles); start += per {
		end := min(start+per, len(tiles))
		var l1, l2, l3 strings.Builder
		for i := start; i < end; i++ {
			l1.WriteString(pad("  "+t.dim(tiles[i][0]), cell, false))
			val := tiles[i][1]
			if tiles[i][0] == "COST" {
				val = t.money(val)
			}
			l2.WriteString(pad("  "+t.bold(val), cell, false))
			l3.WriteString(pad("  "+t.dim(fit(subs[i], cell-3)), cell, false))
		}
		t.println(strings.TrimRight(l1.String(), " "))
		t.println(strings.TrimRight(l2.String(), " "))
		t.println(strings.TrimRight(l3.String(), " "))
	}
}

// daily draws a column chart of tokens (in+out) per day, 6 rows tall.
func (t Term) daily(days []query.Day) {
	if len(days) == 0 {
		return
	}
	t.println(t.accent("By day") + t.dim("  tokens in+out, with cost"))
	avail := t.Width - 10
	vals := make([]float64, len(days))
	for i, d := range days {
		vals[i] = float64(d.Tokens.InOut())
	}
	// Group days into columns when there are more days than room.
	group := 1
	for len(days)/group > avail {
		group++
	}
	var cols []float64
	var labels []string
	for i := 0; i < len(days); i += group {
		sum := 0.0
		for j := i; j < min(i+group, len(days)); j++ {
			sum += vals[j]
		}
		cols = append(cols, sum)
		labels = append(labels, days[i].Date)
	}
	maxV := 0.0
	for _, v := range cols {
		if v > maxV {
			maxV = v
		}
	}
	colW := 1
	if len(cols)*2 <= avail {
		colW = 2
	}
	if len(cols)*3 <= avail {
		colW = 3
	}
	const height = 6
	levels := []rune(" ▁▂▃▄▅▆▇█")
	for row := height; row >= 1; row-- {
		var b strings.Builder
		if row == height {
			b.WriteString(pad(Num(int64(maxV)), 7, true) + " ")
		} else {
			b.WriteString(strings.Repeat(" ", 8))
		}
		for _, v := range cols {
			h := 0.0
			if maxV > 0 {
				h = v / maxV * height
			}
			var r rune
			switch {
			case h >= float64(row):
				r = '█'
			case h > float64(row-1):
				r = levels[int((h-float64(row-1))*8)]
			default:
				r = ' '
			}
			cell := strings.Repeat(string(r), colW-1)
			if colW == 1 {
				cell = string(r)
			} else {
				cell += " "
			}
			b.WriteString(t.accent(cell))
		}
		t.println(strings.TrimRight(b.String(), " "))
	}
	// Date axis: first, middle and last.
	axis := []rune(strings.Repeat(" ", 8+len(cols)*colW))
	put := func(i int, s string) {
		pos := 8 + i*colW
		for k, r := range s {
			if pos+k < len(axis) {
				axis[pos+k] = r
			}
		}
	}
	short := func(d string) string { return d[5:] }
	put(0, short(labels[0]))
	if len(labels) > 12 {
		put(len(labels)/2-2, short(labels[len(labels)/2]))
	}
	last := short(labels[len(labels)-1])
	put(max(0, len(labels)-len(last)/colW-1), last)
	t.println(t.dim(strings.TrimRight(string(axis), " ")))
	var cost float64
	peak := query.Day{}
	for _, d := range days {
		cost += d.Cost
		if d.Tokens.InOut() > peak.Tokens.InOut() {
			peak = d
		}
	}
	if peak.Date != "" {
		t.println(t.dim(fmt.Sprintf("        busiest %s: %s tokens, %s", peak.Date, Num(peak.Tokens.InOut()), USD(peak.Cost))))
	}
}

func (t Term) sharesLines(title string, ss []query.Share, w int) []string {
	out := []string{t.accent(title)}
	if len(ss) == 0 {
		return append(out, t.dim("  none"))
	}
	top := ss
	if len(top) > 6 {
		top = top[:6]
	}
	maxC := top[0].Cost
	if maxC == 0 {
		maxC = 1
	}
	nameW := min(18, w/3)
	barW := w - nameW - 20
	if barW < 4 {
		barW = 4
	}
	for _, s := range top {
		name := strings.TrimPrefix(s.Name, "claude-")
		out = append(out, fmt.Sprintf("  %s %s %s %s",
			pad(fit(name, nameW), nameW, false),
			pad(t.accent(bar(s.Cost/maxC, barW)), barW, false),
			pad(Num(s.Tokens.InOut()), 6, true),
			pad(t.money(USD(s.Cost)), 8, true)))
	}
	if len(ss) > len(top) {
		out = append(out, t.dim(fmt.Sprintf("  +%d more", len(ss)-len(top))))
	}
	return out
}

func (t Term) shares(title string, ss []query.Share, w int) {
	for _, l := range t.sharesLines(title, ss, w) {
		t.println(l)
	}
}

func (t Term) sideBySide(left, right []string, w int) {
	n := max(len(left), len(right))
	for i := 0; i < n; i++ {
		l, r := "", ""
		if i < len(left) {
			l = left[i]
		}
		if i < len(right) {
			r = right[i]
		}
		t.println(strings.TrimRight(pad(l, w+2, false)+r, " "))
	}
}

// heatmap prints active time by weekday and hour.
func (t Term) heatmap(h [7][24]int64) {
	var maxV int64
	for _, row := range h {
		for _, v := range row {
			if v > maxV {
				maxV = v
			}
		}
	}
	t.println(t.accent("When you work") + t.dim("  active time by weekday and hour, "+zoneName(t)))
	cellW := 2
	if t.Width < 60 {
		cellW = 1
	}
	shades := []string{"·", "░", "▒", "▓", "█"}
	names := []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}
	for d, row := range h {
		var b strings.Builder
		b.WriteString("  " + t.dim(names[d]) + " ")
		for _, v := range row {
			i := 0
			if maxV > 0 && v > 0 {
				i = 1 + int(float64(v)/float64(maxV)*3.999)
			}
			cell := strings.Repeat(shades[i], cellW)
			if i == 0 {
				cell = t.dim(cell)
			} else {
				cell = t.accent(cell)
			}
			b.WriteString(cell)
		}
		t.println(b.String())
	}
	axis := "      "
	for hr := 0; hr < 24; hr += 6 {
		axis += pad(fmt.Sprint(hr), 6*cellW, false)
	}
	t.println(t.dim(strings.TrimRight(axis, " ")))
	if maxV > 0 {
		bd, bh := 0, 0
		for d := range h {
			for hr := range h[d] {
				if h[d][hr] > h[bd][bh] {
					bd, bh = d, hr
				}
			}
		}
		t.println(t.dim(fmt.Sprintf("  busiest hour: %s %02d:00", names[bd], bh)))
	}
}

func (t Term) shape(s query.Shape) {
	t.println(t.accent("Session shape"))
	row := func(name string, bs []query.Bucket) {
		total := 0
		for _, b := range bs {
			total += b.Count
		}
		var parts []string
		for _, b := range bs {
			parts = append(parts, fmt.Sprintf("%s %s", t.dim(b.Label), pctBar(t, b.Count, total)))
		}
		t.printf("  %s %s\n", pad(t.dim(name), 20, false), strings.Join(parts, "  "))
	}
	row("prompts", s.Prompts)
	row("active minutes", s.ActiveMinutes)
	row("tool calls/prompt", s.ToolsPerPrompt)
}

func pctBar(t Term, n, total int) string {
	if total == 0 || n == 0 {
		return t.dim("0")
	}
	return t.accent(fmt.Sprint(n))
}

// Since returns a short "since" description for a range start.
func Since(t time.Time) string {
	if t.IsZero() {
		return "all time"
	}
	return "since " + t.Format("2006-01-02")
}

func zoneName(t Term) string {
	name, off := t.Now.In(t.Loc).Zone()
	return fmt.Sprintf("%s (UTC%+d)", name, off/3600)
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// Plural writes a count with its noun: "1 agent", "3 agents".
func Plural(n int, word string) string { return plural(n, word) }
