package render

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/mattn/go-runewidth"

	"github.com/lr00rl/ccli/internal/conv"
	"github.com/lr00rl/ccli/internal/query"
)

// LogOptions choose how much of a turn to print.
type LogOptions struct {
	Detail   string // full (default) or reply. reply keeps the prompt and the turn's last reply.
	Tools    bool   // tool inputs and outputs, not just one line per call
	Thinking bool
	Context  bool // text Claude Code added: skill bodies, reminders, peer messages
	Raw      bool // prompts and replies as recorded, without markdown rendering
}

func (o LogOptions) reply() bool {
	return strings.EqualFold(strings.TrimSpace(o.Detail), "reply")
}

var kindTag = map[string]string{
	"task":  "task",
	"peer":  "peer",
	"other": "system",
	"start": "start",
}

// Turns prints a session's turn list. all includes turns started by task
// notifications, other sessions and the system.
func (t Term) Turns(l *query.TurnList, all bool) {
	s := l.Session
	ttl := s.Title
	if ttl == "" {
		ttl = "(untitled)"
	}
	t.println(t.bold(ttl))
	other := len(l.Turns) - l.Human()
	head := fmt.Sprintf("%s · %s · %s typed", s.ID[:min(8, len(s.ID))], s.Account, plural(l.Human(), "prompt"))
	if other > 0 {
		head += fmt.Sprintf(", %d from tasks and other sessions", other)
	}
	t.println(t.dim(head) + " · " + t.money(costText(s.Cost)))
	if l.Missing {
		t.println(t.warn("the transcript is gone from disk; turns can be listed but not opened"))
	}
	t.println()

	var top float64 // the cost that marks a turn as one of the costliest
	var costs []float64
	for _, tr := range l.Turns {
		if tr.Kind == "human" {
			costs = append(costs, tr.Total())
		}
	}
	if len(costs) >= 10 {
		sort.Float64s(costs)
		top = costs[len(costs)*9/10]
	}
	var rows [][]string
	hidden := 0
	day := ""
	for _, tr := range l.Turns {
		if !all && tr.Kind != "human" {
			hidden++
			continue
		}
		when := tr.At.In(t.Loc).Format("01-02 15:04")
		if d := tr.At.In(t.Loc).Format("01-02"); d == day {
			when = "      " + tr.At.In(t.Loc).Format("15:04")
		} else {
			day = d
		}
		cost := ""
		if c := tr.Total(); c > 0 {
			cost = USD(c)
			if top > 0 && c >= top {
				cost = t.warn(cost)
			} else {
				cost = t.money(cost)
			}
		}
		agents := ""
		if tr.Agents > 0 {
			agents = fmt.Sprint(tr.Agents)
		}
		tools := ""
		if tr.ToolCalls > 0 {
			tools = fmt.Sprint(tr.ToolCalls)
		}
		prompt := tr.Headline
		if prompt == "" && tr.Images > 0 {
			prompt = fmt.Sprintf("(%s)", plural(tr.Images, "image"))
		}
		if tag := kindTag[tr.Kind]; tag != "" {
			prompt = t.dim(tag+" ") + t.dim(prompt)
		}
		took := ""
		if tr.DurationMS > 0 {
			took = Dur(tr.DurationMS)
		}
		row := []string{fmt.Sprint(tr.Seq), t.dim(when), took, cost, tools, agents}
		if t.Verbose {
			row = append(row, Num(tr.Tokens.Input), Num(tr.Tokens.Output), Num(tr.Tokens.CacheRead))
		}
		row = append(row, prompt)
		rows = append(rows, row)
	}
	cols := []column{{title: " TURN", right: true}, {title: "WHEN"}, {title: "TOOK", right: true}, {title: "COST", right: true},
		{title: "TOOLS", right: true}, {title: "AGENTS", right: true}}
	if t.Verbose {
		cols = append(cols, column{title: "IN", right: true}, column{title: "OUT", right: true}, column{title: "CACHED", right: true})
	}
	cols = append(cols, column{title: "PROMPT", flex: true, min: 20})
	t.table(cols, rows)
	t.println()
	id := query.ShortID(s.ID)
	if hidden > 0 {
		t.println(t.dim(fmt.Sprintf("%d turns started by tasks, other sessions or the system are hidden (--all shows them)", hidden)))
	}
	t.println(t.dim(fmt.Sprintf("ccli log %s <turn> prints a turn in full · ccli view %s browses them", id, id)))
}

// Turn prints one turn in full.
func (t Term) Turn(d *query.TurnDetail, opt LogOptions) {
	r := d.Row
	title := fmt.Sprintf("Turn %d", r.Seq)
	if tag := kindTag[r.Kind]; tag != "" {
		title += " (" + tag + ")"
	}
	t.println(t.bold(title) + t.dim("  "+d.Session.ID[:min(8, len(d.Session.ID))]+" · "+d.Session.Account+" · "+fit(d.Session.Title, max(20, t.Width-40))))
	t.println(t.dim(joinWrap(turnFacts(t, r), " · ", max(30, t.Width))))
	t.println()
	if d.Turn != nil {
		t.Conversation(d.Turn, opt, d.Agents)
	}
	if !opt.reply() && (len(d.Agents) > 0 || len(d.Runs) > 0) {
		t.println()
		t.println(t.accent("Agents started in this turn"))
		t.agentNodes(d.Agents, "  ")
		for _, run := range d.Runs {
			t.println("  " + runLine(t, run))
		}
	}
	nav := []string{}
	id := query.ShortID(d.Session.ID)
	if d.Prev >= 0 {
		nav = append(nav, fmt.Sprintf("ccli log %s %d", id, d.Prev))
	}
	if d.Next >= 0 {
		nav = append(nav, fmt.Sprintf("ccli log %s %d", id, d.Next))
	}
	if len(nav) > 0 {
		t.println()
		t.println(t.dim("previous and next: " + strings.Join(nav, " · ")))
	}
}

func turnFacts(t Term, r query.TurnRow) []string {
	parts := []string{r.At.In(t.Loc).Format("2006-01-02 15:04")}
	if r.DurationMS > 0 {
		parts = append(parts, "took "+Dur(r.DurationMS))
	}
	if r.Total() > 0 {
		c := USD(r.Total())
		if r.AgentCost > 0 {
			c += fmt.Sprintf(" (%s here, %s in agents)", USD(r.Cost), USD(r.AgentCost))
		}
		parts = append(parts, c)
	}
	if r.Responses > 0 {
		parts = append(parts, plural(r.Responses, "model call"))
	}
	if r.ToolCalls > 0 {
		parts = append(parts, plural(r.ToolCalls, "tool call"))
	}
	if r.Chars > 0 {
		parts = append(parts, fmt.Sprintf("prompt %s chars", Num(int64(r.Chars))))
	}
	if t.Verbose && r.Tokens.All() > 0 {
		parts = append(parts, fmt.Sprintf("%s in · %s out · %s cache read · %s cache write",
			Num(r.Tokens.Input), Num(r.Tokens.Output), Num(r.Tokens.CacheRead), Num(r.Tokens.CacheWrite)))
	}
	return parts
}

// joinWrap joins parts with sep, starting a new line before a part that would
// pass w cells, so no part is split across lines.
func joinWrap(parts []string, sep string, w int) string {
	var b strings.Builder
	line := 0
	for i, p := range parts {
		pw := runewidth.StringWidth(p)
		if i > 0 {
			if line+runewidth.StringWidth(sep)+pw > w {
				b.WriteString("\n")
				line = 0
			} else {
				b.WriteString(sep)
				line += runewidth.StringWidth(sep)
			}
		}
		b.WriteString(p)
		line += pw
	}
	return b.String()
}

// Conversation prints a turn's entries. agents label Agent calls with the
// subagent they started.
func (t Term) Conversation(tr *conv.Turn, opt LogOptions, agents []query.AgentNode) {
	assistant := "Claude"
	if tr.Speaker != "" {
		assistant = tr.Speaker
	}
	byTool := map[string]query.AgentNode{}
	var walk func(ns []query.AgentNode)
	walk = func(ns []query.AgentNode) {
		for _, n := range ns {
			if n.ToolUseID != "" {
				byTool[n.ToolUseID] = n
			}
			walk(n.Children)
		}
	}
	walk(agents)
	w := max(30, t.Width-4)
	md := func(s string) string {
		if opt.Raw {
			return indentLines(wrapPlain(strings.TrimSpace(s), w), "  ")
		}
		return t.Markdown(s, "  ", w)
	}
	speaker := ""
	say := func(who string) {
		if who != speaker {
			if speaker != "" {
				t.println()
			}
			t.println(t.accent(who))
			speaker = who
		}
	}
	entries := tr.Entries
	if opt.reply() {
		entries = conv.ReplyEntries(entries)
	}
	for i := 0; i < len(entries); i++ {
		e := entries[i]
		switch e.Kind {
		case conv.KindPrompt:
			who := "You"
			switch e.Label {
			case "task":
				who = "Task report"
			case "peer":
				who = "Message from another session"
			case "other":
				who = "System prompt"
			case conv.KindAssigned:
				who = "Task given to the agent"
			}
			speaker = ""
			reminders, rest := conv.SplitReminders(e.Text)
			if len(reminders) > 0 {
				if opt.Context {
					for _, r := range reminders {
						t.println(t.dim("  [reminder from the client]"))
						t.println(t.dim(indentLines(wrapPlain(firstLines(r, 12), w-2), "    ")))
					}
				} else {
					t.println(t.dim(fmt.Sprintf("  (%s from the client hidden; --context shows them)", plural(len(reminders), "reminder"))))
				}
			}
			say(who)
			text := conv.PromptDisplay(rest)
			if e.Images > 0 {
				text = strings.TrimSpace(text + "\n\n" + fmt.Sprintf("(%s attached)", plural(e.Images, "image")))
			}
			t.println(md(text))
		case conv.KindQueued:
			switch e.Label {
			case "task", "task-notification":
				say("Task report (while it worked)")
			default:
				say("You (while it worked)")
			}
			t.println(md(conv.PromptDisplay(e.Text)))
		case conv.KindText:
			if speaker == assistant && i > 0 && entries[i-1].Kind == conv.KindTool {
				t.println() // set the reply apart from the tool calls above it
			}
			say(assistant)
			t.println(md(e.Text))
		case conv.KindThinking:
			if !opt.Thinking {
				continue
			}
			say(assistant)
			t.println(t.dim("  thinking"))
			t.println(t.dim(indentLines(wrapPlain(strings.TrimSpace(e.Text), w-2), "    ")))
		case conv.KindTool:
			say(assistant)
			t.call(e.Call, opt, byTool, w)
			// A long run of the same tool shows its first two calls and its last.
			if opt.Tools {
				continue
			}
			j := i + 1
			for j < len(entries) && entries[j].Kind == conv.KindTool && entries[j].Call.Name == e.Call.Name && byTool[entries[j].Call.ID].AgentID == "" {
				j++
			}
			if j-i > 4 {
				t.call(entries[i+1].Call, opt, byTool, w)
				t.println(t.dim(fmt.Sprintf("  ▸ … %d more %s calls", j-i-3, e.Call.Name)))
				t.call(entries[j-1].Call, opt, byTool, w)
				i = j - 1
			}
		case conv.KindContext:
			if !opt.Context {
				continue
			}
			speaker = ""
			t.println()
			t.println(t.dim("  [" + e.Label + "]"))
			t.println(t.dim(indentLines(wrapPlain(firstLines(strings.TrimSpace(e.Text), 12), w-2), "    ")))
		case conv.KindNotice:
			speaker = ""
			t.println()
			text := firstLines(strings.TrimSpace(e.Text), 6)
			t.println(t.dim("  [" + e.Label + "]"))
			if text != "" && (opt.Context || e.Label != "compact summary") {
				t.println(t.dim(indentLines(wrapPlain(text, w-2), "    ")))
			}
		}
	}
}

func (t Term) call(c *conv.Call, opt LogOptions, byTool map[string]query.AgentNode, w int) {
	name := c.Name
	sum := shortPaths(c.Summary)
	extra := ""
	if n, ok := byTool[c.ID]; ok {
		extra = t.dim(fmt.Sprintf("  → %s · %s", n.AgentID[:min(8, len(n.AgentID))], USD(n.Cost)))
	} else if strings.HasPrefix(c.Target, "wf_") {
		extra = t.dim("  → " + c.Target)
	} else if strings.HasPrefix(c.Target, "teammate:") {
		extra = t.dim("  → teammate " + strings.TrimPrefix(c.Target, "teammate:"))
	}
	mark := t.dim("▸")
	if c.IsError {
		mark = t.warn("✗")
	}
	line := fmt.Sprintf("  %s %s %s", mark, t.bold(name), fit(sum, max(10, w-width(name)-6-width(stripANSI(extra)))))
	t.println(line + extra)
	if !opt.Tools {
		return
	}
	if c.Prompt != "" {
		t.println(t.dim("    prompt"))
		t.println(t.Markdown(c.Prompt, "    ", w-4))
	} else if in := toolInput(c); in != "" {
		t.println(t.dim(indentLines(wrapPlain(strings.ReplaceAll(in, "\t", "    "), w-4), "    ")))
	}
	if c.Done && strings.TrimSpace(c.Output) != "" {
		out := firstLines(strings.TrimRight(c.Output, "\n"), 14)
		if n := strings.Count(c.Output, "\n") + 1; n > 14 {
			out += fmt.Sprintf("\n… %d more lines", n-14)
		} else if c.OutputLen > len(c.Output) {
			out += fmt.Sprintf("\n… %s more", humanSize(int64(c.OutputLen-len(c.Output))))
		}
		t.println(t.dim(indentLines(wrapPlain(strings.ReplaceAll(out, "\t", "    "), w-6), "    │ ")))
	}
}

// toolInput shows a call's input: the command of a shell call, the change of
// an edit, the arguments of anything else.
func toolInput(c *conv.Call) string {
	var in map[string]any
	if json.Unmarshal(c.Input, &in) != nil {
		// Codex passes some inputs as plain text: a patch, or code to run.
		var s string
		if json.Unmarshal(c.Input, &s) == nil {
			return firstLines(s, 16)
		}
		return ""
	}
	str := func(k string) string { s, _ := in[k].(string); return s }
	switch c.Name {
	case "Bash":
		return "$ " + firstLines(str("command"), 20)
	case "Edit":
		return markLines(firstLines(str("old_string"), 6), "- ") + "\n" + markLines(firstLines(str("new_string"), 6), "+ ")
	case "Write":
		return firstLines(str("content"), 10)
	case "Workflow":
		return firstLines(str("script"), 12)
	case "exec_command", "shell": // Codex
		if cmd := str("cmd"); cmd != "" {
			return "$ " + firstLines(cmd, 20)
		}
		if words, ok := in["command"].([]any); ok {
			parts := make([]string, 0, len(words))
			for _, w := range words {
				parts = append(parts, fmt.Sprint(w))
			}
			return "$ " + strings.Join(parts, " ")
		}
	}
	b, err := json.MarshalIndent(in, "", "  ")
	if err != nil {
		return ""
	}
	return firstLines(string(b), 16)
}

func (t Term) agentNodes(ns []query.AgentNode, indent string) {
	for i, n := range ns {
		branch, next := "├─ ", "│  "
		if i == len(ns)-1 {
			branch, next = "└─ ", "   "
		}
		bg := ""
		if n.Shape == "background" {
			bg = t.dim(" bg")
		}
		t.printf("%s%s%s%s %s\n", indent, t.dim(branch), fit(shortPaths(n.Label), max(20, t.Width-len(indent)-40)), bg,
			t.dim(fmt.Sprintf("%s · %s · %s", n.AgentID[:min(8, len(n.AgentID))], Num(n.Tokens.InOut())+" tokens", USD(n.Cost))))
		t.agentNodes(n.Children, indent+t.dim(next))
	}
}

func runLine(t Term, r query.RunInfo) string {
	name := r.Name
	if name == "" {
		name = r.ID
	}
	parts := []string{}
	if r.Status != "" {
		parts = append(parts, r.Status)
	}
	parts = append(parts, runCounts(t, r))
	if r.DurationMS > 0 {
		parts = append(parts, Dur(r.DurationMS))
	}
	parts = append(parts, Num(r.Tokens.InOut())+" tokens", USD(r.Cost))
	return "workflow " + t.bold(name) + t.dim(" "+r.ID+" · ") + strings.Join(parts, t.dim(" · "))
}

// runCounts says how a run's agents ended, adding up to the number started.
func runCounts(t Term, r query.RunInfo) string {
	n := r.Started
	if n == 0 {
		n = r.Agents
	}
	s := fmt.Sprintf("%s: %d done", plural(n, "agent"), r.Results)
	if r.Failed > 0 {
		s += ", " + t.warn(fmt.Sprintf("%d failed", r.Failed))
	}
	if u := r.Unfinished(); u > 0 {
		s += fmt.Sprintf(", %d unfinished", u)
	}
	return s
}

// AgentLog prints a subagent's transcript.
func (t Term) AgentLog(a *query.AgentLog, opt LogOptions) {
	n := a.Agent
	t.println(t.bold(n.Label))
	facts := []string{n.AgentID, a.Session.ID[:min(8, len(a.Session.ID))] + " · " + a.Session.Account}
	if a.Run != nil {
		rn := a.Run.Name
		if rn == "" {
			rn = a.Run.ID
		}
		facts = append(facts, "workflow "+rn)
		if n.Phase != "" {
			facts = append(facts, "phase "+n.Phase)
		}
	}
	if a.Status != "" {
		facts = append(facts, a.Status)
	}
	if !n.Started.IsZero() {
		facts = append(facts, n.Started.In(t.Loc).Format("2006-01-02 15:04"))
		if n.Last.After(n.Started) {
			facts = append(facts, "ran "+Dur(n.Last.Sub(n.Started).Milliseconds()))
		}
	}
	facts = append(facts, Num(n.Tokens.InOut())+" tokens", USD(n.Cost))
	t.println(t.dim(wrapPlain(strings.Join(facts, " · "), max(30, t.Width))))
	t.println()
	for i := range a.Turns {
		if i > 0 {
			t.println()
		}
		t.Conversation(&a.Turns[i], opt, n.Children)
	}
	if opt.reply() {
		return
	}
	if len(n.Children) > 0 {
		t.println()
		t.println(t.accent("Agents it started"))
		t.agentNodes(n.Children, "  ")
	}
}

// RunAgents prints a workflow run's agents by phase.
func (t Term) RunAgents(r *query.RunInfo, nodes []query.AgentNode, status map[string]string, sessionID string) {
	t.println(runLine(t, *r))
	if r.Summary != "" {
		t.println(t.dim(r.Summary))
	}
	t.println()
	var rows [][]string
	for _, n := range nodes {
		st := status[n.AgentID]
		switch st {
		case "failed":
			st = t.warn(st)
		case "done":
			st = t.dim(st)
		}
		rows = append(rows, []string{"  " + n.AgentID[:min(8, len(n.AgentID))], n.Phase, st, t.money(USD(n.Cost)), n.Label})
	}
	t.table([]column{{title: "  AGENT"}, {title: "PHASE"}, {title: "STATUS"}, {title: "COST", right: true}, {title: "LABEL", flex: true}}, rows)
	t.println()
	t.println(t.dim(fmt.Sprintf("ccli log %s --agent <id> prints one agent's transcript", query.ShortID(sessionID))))
}

// Prompts prints prompts across sessions.
func (t Term) Prompts(p query.PromptPage) {
	var rows [][]string
	for _, r := range p.Prompts {
		cost := ""
		if c := r.Total(); c > 0 {
			cost = t.money(USD(c))
		}
		took := ""
		if r.DurationMS > 0 {
			took = Dur(r.DurationMS)
		}
		sess := query.ShortID(r.SessionID) + ":" + fmt.Sprint(r.Seq)
		rows = append(rows, []string{t.dim(r.At.In(t.Loc).Format("01-02 15:04")), sess, fit(r.Project, 18), took, cost, fmt.Sprint(r.ToolCalls), r.Headline})
	}
	t.table([]column{{title: "WHEN"}, {title: "TURN"}, {title: "PROJECT"}, {title: "TOOK", right: true}, {title: "COST", right: true},
		{title: "TOOLS", right: true}, {title: "PROMPT", flex: true, min: 20}}, rows)
	t.println()
	shown := len(p.Prompts)
	summary := fmt.Sprintf("%d of %s", shown, plural(p.Matched, "prompt"))
	if p.Cost > 0 {
		summary += " · " + USD(p.Cost) + " shown"
	}
	t.println(t.dim(summary + " · ccli log <id> <turn> prints one in full"))
}

func indentLines(s, indent string) string {
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		lines[i] = indent + ln
	}
	return strings.Join(lines, "\n")
}

// markLines puts mark in front of every line of s, as a diff does.
func markLines(s, mark string) string {
	return mark + strings.ReplaceAll(s, "\n", "\n"+mark)
}

func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
		lines = append(lines, "…")
	}
	return strings.Join(lines, "\n")
}

var sessionTmp = regexp.MustCompile(`(?:/private)?/tmp/claude-\d+/[^/\s]+/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/`)

// shortPaths writes the home folder as ~ and drops the per-session temp
// folder Claude Code gives a session, leaving "scratchpad/…".
func shortPaths(s string) string {
	s = sessionTmp.ReplaceAllString(s, "")
	if h, err := os.UserHomeDir(); err == nil && len(h) > 1 {
		s = strings.ReplaceAll(s, h+"/", "~/")
	}
	return s
}
