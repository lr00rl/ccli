// Command demo writes a synthetic Claude Code home with two accounts, so ccli
// can be tried, and the README screenshots rendered, without real data.
//
//	go run ./scripts/demo -home /tmp/ccli-demo
//	HOME=/tmp/ccli-demo XDG_DATA_HOME=/tmp/ccli-demo/.local/share ccli stats
//
// Everything in it (accounts, projects, titles, ids) is made up. The output is
// deterministic for a given -now.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type project struct {
	acct, dir string // dir is relative to the home
	titles    []string
	model     string
}

var projects = []project{
	{"work", "acme/checkout-service", []string{
		"Add idempotency keys to checkout retries",
		"Fix flaky cart total test around daylight saving",
		"Profile the slow order export and add an index",
		"Rotate the payment webhook signing secret",
	}, "claude-opus-5-5"},
	{"work", "acme/web-dashboard", []string{
		"Migrate dashboard charts to the v2 metrics API",
		"Upgrade the dashboard to React 20",
		"Fix tooltip overflow on narrow screens",
	}, "claude-opus-5-5"},
	{"work", "acme/data-pipeline", []string{
		"Backfill missing invoice rows from the event log",
		"Split the pipeline DAG into hourly and daily jobs",
		"Write the runbook for the queue outage",
	}, "claude-sonnet-5"},
	{"work", "acme/mobile-app", []string{
		"Review the login flow for token refresh bugs",
		"Add offline sync to the mobile cart",
	}, "claude-opus-5-5"},
	{"default", "code/blog", []string{
		"Port the blog to a static site generator",
		"Add dark mode to the blog theme",
		"Write a post on terminal UI density",
	}, "claude-sonnet-5"},
	{"default", "code/photo-sync", []string{
		"Speed up thumbnail generation",
		"Fix EXIF date parsing for old scans",
	}, "claude-opus-5-5"},
	{"default", "code/home-lab", []string{
		"Set up a Tailscale exit node on the home server",
		"Back up the home lab with restic",
	}, "claude-fable-5-1"},
	{"default", "code/dotfiles", []string{
		"Trim zsh startup time",
	}, "claude-sonnet-5"},
}

var prompts = []string{
	"Look at the failing test and find the cause before changing anything.",
	"Run the suite again and show me only the failures.",
	"Good. Now write the migration and a test for it.",
	"Review the diff and tell me what could break in production.",
	"Check how the other services call this endpoint.",
	"That works. Clean up the naming and commit it.",
	"Why is this query slow? Explain the plan.",
	"Try the simpler approach first.",
}

var subagentTypes = []struct{ kind, desc, model string }{
	{"Explore", "Find every caller of the retry helper", "claude-haiku-4-5-20251001"},
	{"code-reviewer", "Review the migration for data loss", "claude-sonnet-5"},
	{"general-purpose", "Reproduce the flaky test 50 times", "claude-sonnet-5"},
	{"Explore", "Map the config loading path", "claude-haiku-4-5-20251001"},
}

var skills = []string{"code-review", "simplify", "security-review", "frontend-design"}

type gen struct {
	home   string
	rng    *rand.Rand
	now    time.Time
	spawns int // subagents written so far, to vary their kinds
}

func main() {
	home := flag.String("home", "", "directory to write the fake home into (required)")
	nowFlag := flag.String("now", "", "anchor time, RFC 3339 (default: now)")
	livePID := flag.Int("live-pid", 0, "pid to register as a running session in the work account")
	flag.Parse()
	if *home == "" {
		fmt.Fprintln(os.Stderr, "usage: demo -home DIR [-now RFC3339] [-live-pid PID]")
		os.Exit(2)
	}
	now := time.Now()
	if *nowFlag != "" {
		t, err := time.Parse(time.RFC3339, *nowFlag)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		now = t
	}
	g := &gen{home: *home, rng: rand.New(rand.NewPCG(7, 11)), now: now}
	if err := g.run(*livePID); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func (g *gen) run(livePID int) error {
	g.must(g.writeJSON(filepath.Join(g.home, ".claude.json"), map[string]any{
		"oauthAccount": map[string]any{"emailAddress": "alex@example.com", "billingType": "stripe_subscription"},
	}))
	g.must(g.writeJSON(filepath.Join(g.dir("work"), ".claude.json"), map[string]any{
		"oauthAccount": map[string]any{"emailAddress": "alex@acme.example", "organizationName": "Acme Inc", "billingType": "stripe_subscription"},
	}))

	type made struct {
		s    *session
		path string
	}
	var all []made
	add := func(s *session) {
		all = append(all, made{s, g.write(s)})
	}

	// Ordinary sessions, newest last.
	for i := 0; i < 38; i++ {
		p := projects[g.rng.IntN(len(projects))]
		if i%5 == 0 {
			p = projects[i%2] // keep the busiest work projects busy
		}
		s := g.session(p, g.rng.IntN(29)+1)
		switch {
		case i%3 == 0:
			s.subs = 1 + g.rng.IntN(2)
		case i%7 == 0:
			s.subs = 3
			s.nested = true
		}
		if i%4 == 1 {
			s.skill = skills[g.rng.IntN(len(skills))]
		}
		if i%11 == 3 {
			s.title = "" // falls back to the first prompt
		}
		add(s)
	}

	// Today: a long work session with a team, a workflow run and calls the
	// transcript never recorded, plus the running one.
	lead := g.session(projects[0], 0)
	lead.start = g.now.Add(-7 * time.Hour)
	lead.title, lead.turns, lead.subs = "Audit payment retries across services", 46, 1
	lead.script, lead.shortfall = leadScript, 0.07
	add(lead)
	for i, name := range []string{"researcher-1", "researcher-2", "reviewer"} {
		t := g.session(projects[0], 0)
		t.start = lead.start.Add(time.Duration(10+i*4) * time.Minute)
		t.title, t.turns, t.model = "", 6+g.rng.IntN(5), "claude-sonnet-5"
		t.team, t.agent = "session-"+lead.id[:8], name
		add(t)
	}
	first := g.session(projects[2], 1)
	first.title, first.turns = "Investigate duplicate invoice events", 18
	second := g.session(projects[2], 1)
	second.start = first.start.Add(3 * time.Hour)
	second.title = "Investigate duplicate invoice events (continued)"
	first.continued = second.id
	add(first)
	add(second)
	running := g.session(projects[1], 0)
	running.start = g.now.Add(-25 * time.Minute)
	running.title, running.turns = "Fix tooltip overflow on narrow screens", 9
	add(running)

	// Sessions that also exist in the personal account, for ccli diff: one
	// identical copy, one copy the work account has since extended, and one
	// that both accounts extended differently.
	var workIdx []int
	for i, m := range all {
		if m.s.acct == "work" && m.s.team == "" && m.s != lead && m.s != running {
			workIdx = append(workIdx, i)
		}
	}
	for n, i := range workIdx[:3] {
		m := all[i]
		data, err := os.ReadFile(m.path)
		g.must(err)
		lines := strings.SplitAfter(string(data), "\n")
		switch n {
		case 1: // work kept going after the copy
			data = []byte(strings.Join(lines[:len(lines)*2/3], ""))
		case 2: // both kept going
			cut := strings.Join(lines[:len(lines)/2], "")
			extra := g.line(map[string]any{"type": "user", "timestamp": ts(m.s.start.Add(-time.Minute)), "sessionId": m.s.id,
				"cwd": m.s.cwd, "promptSource": "typed", "message": map[string]any{"role": "user", "content": "Try it on the personal laptop instead."}})
			data = []byte(cut + extra)
		}
		dst := filepath.Join(g.dir("default"), "projects", key(m.s.cwd), m.s.id+".jsonl")
		g.must(os.MkdirAll(filepath.Dir(dst), 0o755))
		g.must(os.WriteFile(dst, data, 0o644))
	}

	if livePID > 0 {
		g.must(g.writeJSON(filepath.Join(g.dir("work"), "sessions", strconv.Itoa(livePID)+".json"), map[string]any{
			"pid": livePID, "sessionId": running.id, "cwd": running.cwd, "status": "busy",
		}))
	}
	g.codex()
	fmt.Printf("wrote %d sessions under %s\n", len(all), g.home)
	return nil
}

type session struct {
	acct, cwd, id, title, model string
	start                       time.Time
	turns, subs                 int
	nested, workflow            bool
	skill, team, agent          string
	continued                   string
	shortfall                   float64
	script                      []turnScript // played before the generic turns
	seed                        uint64       // its own random source, see write
}

func (g *gen) session(p project, daysAgo int) *session {
	day := time.Date(g.now.Year(), g.now.Month(), g.now.Day(), 0, 0, 0, 0, time.Local).AddDate(0, 0, -daysAgo)
	hours := []int{9, 10, 10, 11, 11, 14, 15, 15, 16, 17, 21, 22, 23}
	start := day.Add(time.Duration(hours[g.rng.IntN(len(hours))])*time.Hour + time.Duration(g.rng.IntN(50))*time.Minute)
	if daysAgo == 0 {
		start = g.now.Add(-time.Duration(150+g.rng.IntN(60)) * time.Minute)
	}
	return &session{
		acct:  p.acct,
		cwd:   filepath.Join(g.home, p.dir),
		id:    g.uuid(),
		title: p.titles[g.rng.IntN(len(p.titles))],
		model: p.model,
		start: start,
		turns: 4 + g.rng.IntN(22),
		seed:  g.rng.Uint64(),
	}
}

// totals accumulates usage per model, for the cost-state line.
type totals map[string]*[4]int64

func (t totals) add(model string, in, out, read, write int64) {
	if t[model] == nil {
		t[model] = &[4]int64{}
	}
	v := t[model]
	v[0] += in
	v[1] += out
	v[2] += read
	v[3] += write
}

// write writes a session's main transcript and its subagents, and returns the
// main transcript's path. A session with a script plays it first; the rest
// of its turns are generic.
func (g *gen) write(s *session) string {
	// A session draws from its own source while it is written: a session from
	// today stops before now, so how much of it is written depends on the time
	// of day, and must not change the ids of the sessions made after it.
	outer := g.rng
	g.rng = rand.New(rand.NewPCG(s.seed, 1))
	defer func() { g.rng = outer }()
	base := filepath.Join(g.dir(s.acct), "projects", key(s.cwd))
	path := filepath.Join(base, s.id+".jsonl")
	w := &writer{g: g, s: s, base: base, t: s.start, sum: totals{}, ctx: int64(18_000 + g.rng.IntN(8_000))}
	if s.title != "" && s.team == "" {
		w.b.WriteString(g.line(map[string]any{"type": "ai-title", "aiTitle": s.title, "sessionId": s.id}))
	}
	spawnAt := map[int]bool{}
	for i := 0; i < s.subs; i++ {
		spawnAt[len(s.script)+1+g.rng.IntN(max(1, s.turns-len(s.script)-1))] = true
	}
	subN, wfDone := 0, false
	for turn := 0; turn < s.turns; turn++ {
		if w.t.After(g.now.Add(-3 * time.Minute)) {
			break // sessions from today stop before now
		}
		if turn < len(s.script) {
			w.scripted(s.script[turn])
			w.pause()
			continue
		}
		text := prompts[g.rng.IntN(len(prompts))]
		if turn == 0 && s.title == "" && s.team == "" {
			text = "Why does the nightly export take forty minutes now?"
		}
		origin := "human"
		if s.team != "" {
			origin = "peer"
		}
		w.prompt(text, origin)
		for r, n := 0, 1+g.rng.IntN(5); r < n; r++ {
			w.t = w.t.Add(time.Duration(4+g.rng.IntN(40)) * time.Second)
			name, input := g.tool()
			c := call{name: name, input: input, out: "ok"}
			if spawnAt[turn] && r == 0 && subN < s.subs {
				st := subagentTypes[g.spawns%len(subagentTypes)]
				g.spawns++
				c = call{name: "Agent", input: map[string]any{"description": st.desc, "subagent_type": st.kind,
					"prompt": st.desc + ". Report what you find; do not change any files."},
					agent: &agentSpec{kind: st.kind, desc: st.desc, model: st.model, nest: s.nested && subN == 0}}
				subN++
			}
			if s.skill != "" && turn == 1 && r == 0 {
				c = call{name: "Skill", input: map[string]any{"skill": s.skill}, out: "Launching skill: " + s.skill}
			}
			w.assistant("Checking.", c)
		}
		if s.workflow && !wfDone && turn == s.turns/2 {
			w.assistant("Running the checks as a workflow.", call{name: "Workflow", workflow: &workflowSpec{name: "nightly-checks",
				summary: "Run the three nightly checks in parallel", jobs: [][2]string{{"Scan", "scan:a"}, {"Scan", "scan:b"}, {"Report", "summarize"}}}})
			wfDone = true
		}
		w.pause()
	}
	if s.continued != "" {
		w.b.WriteString(g.line(map[string]any{"type": "continued-in", "continuedInSessionId": s.continued, "sessionId": s.id}))
	}
	if s.shortfall > 0 {
		usage, cost := map[string]any{}, 0.0
		for m, v := range w.sum {
			f := 1.0
			if m == s.model {
				f += s.shortfall
			}
			c := (float64(v[0])*4 + float64(v[1])*20 + float64(v[2])*0.2 + float64(v[3])*8) * f / 1e6
			usage[m] = map[string]any{"inputTokens": int64(float64(v[0]) * f), "outputTokens": int64(float64(v[1]) * f),
				"cacheReadInputTokens": int64(float64(v[2]) * f), "cacheCreationInputTokens": int64(float64(v[3]) * f),
				"webSearchRequests": 0, "costUSD": c}
			cost += c
		}
		w.b.WriteString(g.line(map[string]any{"type": "cost-state", "sessionId": s.id, "totalCostUSD": cost,
			"startTime": s.start.UnixMilli(), "modelUsage": usage}))
	}
	g.must(os.MkdirAll(base, 0o755))
	g.must(os.WriteFile(path, []byte(w.b.String()), 0o644))
	return path
}

// turnScript is one scripted turn: a prompt, the model's opening text, its
// tool calls, and its closing reply.
type turnScript struct {
	prompt, origin string
	open           string
	calls          []call
	reply          string
}

type call struct {
	name     string
	input    map[string]any
	out      string
	isErr    bool
	agent    *agentSpec
	workflow *workflowSpec
}

type agentSpec struct {
	kind, desc, model string
	nest              bool
}

type workflowSpec struct {
	name, summary string
	jobs          [][2]string // phase, label
	failed        int         // index of a job that fails, or -1 when none
}

// writer builds one main transcript.
type writer struct {
	g    *gen
	s    *session
	base string
	b    strings.Builder
	t    time.Time
	sum  totals
	ctx  int64
	n    int // prompts written, for promptIds
}

func (w *writer) common(m map[string]any) map[string]any {
	m["sessionId"], m["cwd"], m["timestamp"] = w.s.id, w.s.cwd, ts(w.t)
	if w.s.team != "" {
		m["teamName"], m["agentName"] = w.s.team, w.s.agent
	}
	return m
}

func (w *writer) prompt(text, origin string) {
	w.n++
	src := "typed"
	if origin != "human" {
		src = "system"
	}
	w.b.WriteString(w.g.line(w.common(map[string]any{"type": "user", "promptSource": src, "origin": map[string]any{"kind": origin},
		"promptId": fmt.Sprintf("%s-p%d", w.s.id[:8], w.n), "message": map[string]any{"role": "user", "content": text}})))
}

// usage makes the usage of one model call and adds it to the totals.
func (w *writer) usage() map[string]any {
	g := w.g
	write := int64(600 + g.rng.IntN(3_400))
	out := int64(150 + g.rng.IntN(2_200))
	in := int64(1 + g.rng.IntN(12))
	u := map[string]any{"input_tokens": in, "output_tokens": out, "cache_read_input_tokens": w.ctx,
		"cache_creation_input_tokens": write,
		"cache_creation":              map[string]any{"ephemeral_5m_input_tokens": 0, "ephemeral_1h_input_tokens": write}}
	if g.rng.IntN(40) == 0 {
		u["server_tool_use"] = map[string]any{"web_search_requests": 2}
	}
	w.sum.add(w.s.model, in, out, w.ctx, write)
	w.ctx += write
	if w.ctx > 380_000 {
		w.ctx = 40_000 // compacted
	}
	return u
}

// assistant writes one model response: text, then a tool call and its result.
func (w *writer) assistant(text string, c call) {
	g := w.g
	content := []any{}
	if text != "" {
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	id := g.toolID()
	input := c.input
	if c.workflow != nil {
		input = map[string]any{"script": "export const meta = {\n  name: '" + c.workflow.name + "',\n  description: '" + c.workflow.summary + "',\n}\n"}
	}
	if c.name != "" {
		content = append(content, map[string]any{"type": "tool_use", "id": id, "name": c.name, "input": input})
	}
	msg := map[string]any{"id": g.msgID(), "model": w.s.model, "role": "assistant", "usage": w.usage(), "content": content}
	w.b.WriteString(g.line(w.common(map[string]any{"type": "assistant", "requestId": "req_" + g.hex(20), "message": msg})))
	if c.name == "" {
		return
	}
	w.t = w.t.Add(time.Duration(2+g.rng.IntN(20)) * time.Second)
	res := map[string]any{"type": "tool_result", "tool_use_id": id, "content": c.out}
	if c.isErr {
		res["is_error"] = true
	}
	line := w.common(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []any{res}}})
	switch {
	case c.agent != nil:
		prompt, _ := c.input["prompt"].(string)
		aid := g.subagent(w.s, w.base, id, c.agent.kind, c.agent.desc, prompt, c.agent.model, w.t, 1, c.agent.nest, w.sum)
		line["toolUseResult"] = map[string]any{"status": "async_launched", "agentId": aid, "description": c.agent.desc}
		res["content"] = "Async agent launched successfully."
	case c.workflow != nil:
		run := g.workflow(w.s, w.base, w.t, w.sum, c.workflow)
		line["toolUseResult"] = map[string]any{"status": "async_launched", "runId": run, "workflowName": c.workflow.name, "summary": c.workflow.summary}
		res["content"] = "Workflow launched in background."
	}
	w.b.WriteString(g.line(line))
}

func (w *writer) scripted(ts turnScript) {
	origin := ts.origin
	if origin == "" {
		origin = "human"
	}
	w.prompt(ts.prompt, origin)
	w.t = w.t.Add(time.Duration(5+w.g.rng.IntN(20)) * time.Second)
	if ts.open != "" && len(ts.calls) == 0 {
		w.assistant(ts.open, call{})
	}
	for i, c := range ts.calls {
		text := ""
		if i == 0 {
			text = ts.open
		}
		if c.out == "" {
			c.out = "ok"
		}
		w.assistant(text, c)
		w.t = w.t.Add(time.Duration(6+w.g.rng.IntN(40)) * time.Second)
	}
	if ts.reply != "" {
		w.assistant(ts.reply, call{})
	}
}

// pause is the person reading and typing; now and then they leave for a while.
func (w *writer) pause() {
	gap := time.Duration(20+w.g.rng.IntN(200)) * time.Second
	if w.g.rng.IntN(9) == 0 {
		gap = time.Duration(12+w.g.rng.IntN(40)) * time.Minute
	}
	w.t = w.t.Add(gap)
}

// subagent writes one subagent transcript and its meta file, and returns its
// agent id. A nested one spawns a further agent of its own.
func (g *gen) subagent(s *session, base, toolUse, kind, desc, prompt, model string, t time.Time, depth int, nest bool, sum totals) string {
	aid := g.hex(17)
	dir := filepath.Join(base, s.id, "subagents")
	var b strings.Builder
	if prompt == "" {
		prompt = desc + "."
	}
	b.WriteString(g.line(map[string]any{"type": "user", "isSidechain": true, "agentId": aid, "sessionId": s.id, "cwd": s.cwd,
		"timestamp": ts(t), "message": map[string]any{"role": "user", "content": prompt}}))
	ctx := int64(9_000)
	for r, n := 0, 4+g.rng.IntN(9); r < n; r++ {
		t = t.Add(time.Duration(3+g.rng.IntN(15)) * time.Second)
		tool, input := g.tool()
		id := g.toolID()
		if nest && r == 1 {
			tool, input = "Agent", map[string]any{"description": "Check the retry docs", "subagent_type": "Explore", "prompt": "Read docs/retries.md and list every rule it states."}
			g.subagent(s, base, id, "Explore", "Check the retry docs", "Read docs/retries.md and list every rule it states.", "claude-haiku-4-5-20251001", t, depth+1, false, sum)
		}
		write, out := int64(400+g.rng.IntN(2_000)), int64(100+g.rng.IntN(1_200))
		sum.add(model, 2, out, ctx, write)
		content := []any{map[string]any{"type": "tool_use", "id": id, "name": tool, "input": input}}
		if r == n-1 {
			content = []any{map[string]any{"type": "text", "text": "Done: three call sites retry without a key; the other two pass the order's key through."}}
		}
		b.WriteString(g.line(map[string]any{"type": "assistant", "isSidechain": true, "agentId": aid, "sessionId": s.id, "cwd": s.cwd,
			"timestamp": ts(t), "requestId": "req_" + g.hex(20),
			"message": map[string]any{"id": g.msgID(), "model": model, "role": "assistant",
				"usage": map[string]any{"input_tokens": 2, "output_tokens": out, "cache_read_input_tokens": ctx,
					"cache_creation_input_tokens": write,
					"cache_creation":              map[string]any{"ephemeral_5m_input_tokens": write, "ephemeral_1h_input_tokens": 0}},
				"content": content}}))
		if r < n-1 {
			b.WriteString(g.line(map[string]any{"type": "user", "isSidechain": true, "agentId": aid, "sessionId": s.id, "timestamp": ts(t),
				"message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": id, "content": "ok"}}}}))
		}
		ctx += write
	}
	g.must(os.MkdirAll(dir, 0o755))
	g.must(os.WriteFile(filepath.Join(dir, "agent-"+aid+".jsonl"), []byte(b.String()), 0o644))
	g.must(g.writeJSON(filepath.Join(dir, "agent-"+aid+".meta.json"), map[string]any{
		"agentType": kind, "description": desc, "toolUseId": toolUse, "spawnDepth": depth, "requestShape": "background",
	}))
	return aid
}

// workflow writes a workflow run: its agents, journal and state file. It
// returns the run id.
func (g *gen) workflow(s *session, base string, t time.Time, sum totals, spec *workflowSpec) string {
	run := "wf_" + g.hex(8) + "-" + g.hex(3)
	dir := filepath.Join(base, s.id, "subagents", "workflows", run)
	g.must(os.MkdirAll(dir, 0o755))
	start := t
	var journal strings.Builder
	journal.WriteString(`{"type":"launched"}` + "\n")
	failed := spec.failed
	if len(spec.jobs) < 3 {
		failed = -1
	}
	for i, job := range spec.jobs {
		aid := g.hex(17)
		fmt.Fprintf(&journal, `{"type":"started","agentId":%q,"label":%q,"phase":%q}`+"\n", aid, job[1], job[0])
		var b strings.Builder
		b.WriteString(g.line(map[string]any{"type": "user", "isSidechain": true, "agentId": aid, "sessionId": s.id, "cwd": s.cwd,
			"timestamp": ts(t), "message": map[string]any{"role": "user", "content": "Workflow task " + job[1] + ": " + spec.summary + ". Return a short structured report."}}))
		ctx := int64(7_000)
		n := 5 + i
		if i == failed {
			n = 2
		}
		for r := 0; r < n; r++ {
			t = t.Add(time.Duration(5+g.rng.IntN(20)) * time.Second)
			write, out := int64(500+g.rng.IntN(1_500)), int64(200+g.rng.IntN(900))
			sum.add("claude-sonnet-5", 3, out, ctx, write)
			b.WriteString(g.line(map[string]any{"type": "assistant", "isSidechain": true, "agentId": aid, "attributionAgent": "workflow-subagent",
				"sessionId": s.id, "cwd": s.cwd, "timestamp": ts(t), "message": map[string]any{"id": g.msgID(), "model": "claude-sonnet-5", "role": "assistant",
					"usage":   map[string]any{"input_tokens": 3, "output_tokens": out, "cache_read_input_tokens": ctx, "cache_creation_input_tokens": write},
					"content": []any{map[string]any{"type": "text", "text": "Checked " + job[1] + "."}}}}))
			ctx += write
		}
		g.must(os.WriteFile(filepath.Join(dir, "agent-"+aid+".jsonl"), []byte(b.String()), 0o644))
		g.must(g.writeJSON(filepath.Join(dir, "agent-"+aid+".meta.json"), map[string]any{
			"agentType": "general-purpose", "description": job[1], "workflowPhase": job[0], "spawnDepth": 1, "requestShape": "foreground",
		}))
		if i == failed {
			fmt.Fprintf(&journal, `{"type":"failed","agentId":%q}`+"\n", aid)
		} else {
			fmt.Fprintf(&journal, `{"type":"result","agentId":%q,"result":"{}"}`+"\n", aid)
		}
	}
	g.must(os.WriteFile(filepath.Join(dir, "journal.jsonl"), []byte(journal.String()), 0o644))
	g.must(g.writeJSON(filepath.Join(base, s.id, "workflows", run+".json"), map[string]any{
		"runId": run, "workflowName": spec.name, "status": "completed", "summary": spec.summary,
		"startTime": start.UnixMilli(), "durationMs": t.Sub(start).Milliseconds(), "agentCount": len(spec.jobs),
	}))
	return run
}

func (g *gen) tool() (string, map[string]any) {
	switch n := g.rng.IntN(100); {
	case n < 34:
		return "Read", map[string]any{"file_path": "src/retry.go"}
	case n < 58:
		return "Bash", map[string]any{"command": "go test ./..."}
	case n < 78:
		return "Edit", map[string]any{"file_path": "src/retry.go"}
	case n < 90:
		return "Grep", map[string]any{"pattern": "Retry"}
	case n < 95:
		return "Glob", map[string]any{"pattern": "**/*.go"}
	default:
		return "Write", map[string]any{"file_path": "docs/notes.md"}
	}
}

func (g *gen) dir(acct string) string {
	if acct == "default" {
		return filepath.Join(g.home, ".claude")
	}
	return filepath.Join(g.home, ".claude-"+acct)
}

var nonAlnum = regexp.MustCompile(`[^A-Za-z0-9]`)

// key is Claude Code's project folder name for a working directory.
func key(cwd string) string { return nonAlnum.ReplaceAllString(cwd, "-") }

func ts(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

func (g *gen) hex(n int) string {
	const digits = "0123456789abcdef"
	b := make([]byte, n)
	for i := range b {
		b[i] = digits[g.rng.IntN(16)]
	}
	return string(b)
}

func (g *gen) uuid() string {
	h := g.hex(32)
	return h[:8] + "-" + h[8:12] + "-4" + h[13:16] + "-a" + h[17:20] + "-" + h[20:]
}

func (g *gen) msgID() string  { return "msg_" + g.hex(24) }
func (g *gen) toolID() string { return "toolu_" + g.hex(22) }

func (g *gen) line(v map[string]any) string {
	b, err := json.Marshal(v)
	g.must(err)
	return string(b) + "\n"
}

func (g *gen) writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

func (g *gen) must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// leadScript is the opening of the demo's longest session: realistic enough
// to show what ccli log and ccli view render.
var leadScript = []turnScript{
	{
		prompt: "Audit how our services retry failed payment calls. I want to know where a retry can **charge a customer twice**.\n\nStart with `checkout-service`, then look at `billing-worker` and `refund-api`.",
		open:   "I'll map every retry path first, then check each one for an idempotency key.",
		calls: []call{
			{name: "Grep", input: map[string]any{"pattern": "retry|backoff", "path": "src"}, out: "src/payments/client.go\nsrc/payments/retry.go\nsrc/orders/export.go"},
			{name: "Read", input: map[string]any{"file_path": "src/payments/client.go"}},
			{name: "Agent", input: map[string]any{"description": "Find every caller of the retry helper", "subagent_type": "Explore",
				"prompt": "Find every caller of `retry.Do` in src/ and say, for each, whether the request it repeats carries an idempotency key. Read only; report file:line."},
				agent: &agentSpec{kind: "Explore", desc: "Find every caller of the retry helper", model: "claude-haiku-4-5-20251001", nest: true}},
			{name: "Read", input: map[string]any{"file_path": "src/payments/retry.go", "offset": 40}},
		},
		reply: "## What I found\n\n- `chargeCard` retries up to **3 times** on any 5xx or timeout.\n- Each attempt builds a new request, so the idempotency key is **regenerated** on retry.\n- `billing-worker` passes the order's key through; `refund-api` never retries.\n\n| Service | Retries | Key reused on retry |\n|---|---|---|\n| checkout-service | 3 | no |\n| billing-worker | 5 | yes |\n| refund-api | 0 | n/a |\n\nA double charge happens when the first attempt times out on our side after the provider has already accepted it.",
	},
	{
		prompt: "Here is the incident log from last Tuesday:\n\n<pasted_content id=\"a1\">\n2026-10-07T14:02:11Z WARN  charge timeout after 10s order=88412\n2026-10-07T14:02:11Z INFO  retrying charge attempt=2 order=88412\n2026-10-07T14:02:13Z INFO  charge ok id=ch_9f2 order=88412\n2026-10-07T14:02:40Z ERROR provider webhook: duplicate charge ch_9e7 order=88412\n</pasted_content id=\"a1\">\n\nDoes this match what you found?",
		open:   "Checking where the key is set.",
		calls: []call{
			{name: "Bash", input: map[string]any{"command": "rg -n 'IdempotencyKey' src/", "description": "Find where the key is set"}, out: "src/payments/client.go:118:\treq.IdempotencyKey = uuid.NewString()"},
			{name: "Read", input: map[string]any{"file_path": "src/payments/client.go", "offset": 110}},
		},
		reply: "Yes. Attempt 1 (`ch_9e7`) succeeded at the provider but timed out on our side. Attempt 2 got a **new key** at `client.go:118`, so the provider treated it as a new charge (`ch_9f2`). The webhook flagged the duplicate 27 seconds later.",
	},
	{
		prompt: "Fix it: generate the key once per order and reuse it on every attempt. Add a test that simulates the timeout.",
		open:   "Moving the key out of the attempt loop.",
		calls: []call{
			{name: "Edit", input: map[string]any{"file_path": "src/payments/client.go", "old_string": "req.IdempotencyKey = uuid.NewString()", "new_string": "req.IdempotencyKey = order.IdempotencyKey()"}},
			{name: "Edit", input: map[string]any{"file_path": "src/payments/retry.go", "old_string": "for attempt := 1;", "new_string": "key := order.IdempotencyKey()\n\tfor attempt := 1;"}},
			{name: "Write", input: map[string]any{"file_path": "src/payments/retry_test.go", "content": "func TestRetryReusesKey(t *testing.T) { … }"}},
			{name: "Bash", input: map[string]any{"command": "go test ./src/payments/...", "description": "Run the payment tests"}, out: "--- FAIL: TestRetryReusesKey (0.01s)\n    retry_test.go:31: second attempt sent key \"\"", isErr: true},
			{name: "Edit", input: map[string]any{"file_path": "src/payments/retry.go", "old_string": "c.charge(ctx, req)", "new_string": "c.charge(ctx, req.WithKey(key))"}},
			{name: "Bash", input: map[string]any{"command": "go test ./src/payments/...", "description": "Run the payment tests"}, out: "ok  \tacme/checkout/src/payments\t0.412s"},
		},
		reply: "Done. The key is now created once per order:\n\n```go\nkey := order.IdempotencyKey()\nfor attempt := 1; attempt <= maxAttempts; attempt++ {\n\tres, err := c.charge(ctx, req.WithKey(key))\n\tif err == nil || !retryable(err) {\n\t\treturn res, err\n\t}\n}\n```\n\n`TestRetryReusesKey` makes the first attempt time out after the fake provider accepts it, and checks that the second attempt sends the same key.",
	},
	{
		prompt: "<command-message>code-review</command-message>\n<command-name>/code-review</command-name>\n<command-args>src/payments</command-args>",
		open:   "Starting a review of the change.",
		calls: []call{
			{name: "Agent", input: map[string]any{"description": "Review the retry fix for double charges", "subagent_type": "code-reviewer",
				"prompt": "Review the diff in src/payments for anything that could still charge twice, leak the idempotency key, or change behaviour for callers. Rank findings by severity."},
				agent: &agentSpec{kind: "code-reviewer", desc: "Review the retry fix for double charges", model: "claude-sonnet-5"}},
		},
		reply: "The reviewer is running in the background; I'll report when it finishes.",
	},
	{
		prompt: "<task-notification>\n<task-id>b7k2</task-id>\n<status>completed</status>\n<summary>Agent \"Review the retry fix for double charges\" completed</summary>\n<result>One medium finding: the key is logged at INFO level in client.go:131, which puts it in the shared log index.</result>\n</task-notification>",
		origin: "task-notification",
		open:   "The review found one medium issue: the key is logged at INFO level in `client.go:131`. Everything else checks out.",
	},
	{
		prompt: "Good catch. Lower that log to DEBUG, then run the same audit across the other two services in parallel.",
		open:   "Lowering the log level, then auditing the other services as a workflow.",
		calls: []call{
			{name: "Edit", input: map[string]any{"file_path": "src/payments/client.go", "old_string": "log.Info(\"charging\"", "new_string": "log.Debug(\"charging\""}},
			{name: "Workflow", workflow: &workflowSpec{name: "retry-audit", summary: "Audit billing-worker and refund-api for retries that can repeat a charge",
				jobs: [][2]string{{"Audit", "audit:billing-worker"}, {"Audit", "audit:refund-api"}, {"Report", "summarize"}}, failed: -1}},
		},
		reply: "The workflow is running: one agent per service, then a summary.",
	},
	{
		prompt: "<task-notification>\n<task-id>wv3x</task-id>\n<status>completed</status>\n<summary>Workflow \"retry-audit\" completed</summary>\n<result>billing-worker reuses the order key on all 5 attempts. refund-api never retries. No other double-charge path.</result>\n</task-notification>",
		origin: "task-notification",
		open:   "The audit is complete: `billing-worker` already reuses the order key, and `refund-api` never retries. The checkout fix closes the only double-charge path.",
	},
	{
		prompt: "Write the summary for the incident review. Keep it short.",
		reply:  "### Incident 88412: duplicate charge\n\n**Cause.** `checkout-service` retried a timed-out charge with a new idempotency key, so the provider accepted both attempts.\n\n**Fix.** The key is now created once per order and reused on every attempt (`src/payments/retry.go`), with a test that reproduces the timeout.\n\n**Scope.** `billing-worker` and `refund-api` were audited; neither can repeat a charge.\n\n**Follow-up.** The key is no longer logged at INFO level.",
	},
}
