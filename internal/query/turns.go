package query

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/lr00rl/ccli/internal/account"
	"github.com/lr00rl/ccli/internal/codex"
	"github.com/lr00rl/ccli/internal/conv"
)

// TurnRow is one turn of a session's main conversation: a prompt and the
// work it led to, up to the next prompt.
type TurnRow struct {
	Seq        int       `json:"seq"` // 0: what came before the first prompt
	Kind       string    `json:"kind"`
	At         time.Time `json:"at"`
	End        time.Time `json:"end"`
	DurationMS int64     `json:"duration_ms"` // Claude Code's own turn time, else active time
	Prompt     string    `json:"prompt"`      // as recorded, capped
	Headline   string    `json:"headline"`    // first line as displayed
	Chars      int       `json:"chars"`
	Images     int       `json:"images"`
	ToolCalls  int       `json:"tool_calls"`
	Agents     int       `json:"agents"` // Agent and Workflow calls
	Responses  int       `json:"responses"`
	Tokens     Tokens    `json:"tokens"`
	Cost       float64   `json:"cost"`       // the main conversation's calls in this turn
	AgentCost  float64   `json:"agent_cost"` // subagents and workflow runs it started
	Offset     int64     `json:"-"`
	Next       int64     `json:"-"` // where the next turn starts; 0 = end of file
}

// Total is the turn's cost with what its agents spent.
func (t TurnRow) Total() float64 { return t.Cost + t.AgentCost }

// TurnList is every turn of one session.
type TurnList struct {
	Session SessionRow `json:"session"`
	Path    string     `json:"path"`
	Missing bool       `json:"transcript_missing"`
	Turns   []TurnRow  `json:"turns"`
}

// Human counts the turns a person started.
func (l *TurnList) Human() int {
	n := 0
	for _, t := range l.Turns {
		if t.Kind == "human" {
			n++
		}
	}
	return n
}

// TurnDetail is one turn read from the transcript.
type TurnDetail struct {
	Session SessionRow  `json:"session"`
	Row     TurnRow     `json:"turn"`
	Turn    *conv.Turn  `json:"conversation"`
	Agents  []AgentNode `json:"agents"` // started in this turn, nested
	Runs    []RunInfo   `json:"workflow_runs"`
	Prev    int         `json:"prev"` // -1 when there is none
	Next    int         `json:"next"`
}

// AgentNode is a subagent with the subagents it started.
type AgentNode struct {
	FileInfo
	Label    string      `json:"label"`
	Children []AgentNode `json:"children,omitempty"`
}

// AgentLog is one subagent's or workflow agent's transcript.
type AgentLog struct {
	Session SessionRow  `json:"session"`
	Agent   AgentNode   `json:"agent"`
	Run     *RunInfo    `json:"workflow_run,omitempty"`
	Status  string      `json:"status,omitempty"` // workflow agents: done, failed, unfinished
	Turns   []conv.Turn `json:"conversation"`
}

// graph is a session's files and spawns, with each subagent and workflow run
// placed in the turn that started it.
type graph struct {
	det   *SessionDetail
	main  *FileInfo
	turns []TurnRow
}

func (d *DB) loadGraph(ctx context.Context, ref string) (*graph, error) {
	det, err := d.Session(ctx, ref)
	if err != nil {
		return nil, err
	}
	return det.graph, nil
}

// graphFor builds a session's graph without its list row (title, totals),
// which is all a list of prompts across sessions needs.
func (d *DB) graphFor(ctx context.Context, acct, id string) (*graph, error) {
	det := &SessionDetail{SessionRow: SessionRow{Account: acct, ID: id}}
	if err := d.fillFiles(ctx, det); err != nil {
		return nil, err
	}
	return d.buildGraph(ctx, det)
}

func (d *DB) buildGraph(ctx context.Context, det *SessionDetail) (*graph, error) {
	var err error
	g := &graph{det: det}
	for i := range det.Files {
		if det.Files[i].Kind == "main" {
			g.main = &det.Files[i]
		}
	}
	if g.main == nil {
		return g, nil
	}
	if g.turns, err = d.turnRows(ctx, g.main.ID); err != nil {
		return nil, err
	}
	spawns, err := d.spawns(ctx, det.Account, det.ID)
	if err != nil {
		return nil, err
	}
	placeAgents(det, g.main.ID, spawns, g.turns)
	for _, f := range det.Files {
		if f.Kind == "main" || f.Turn <= 0 {
			continue
		}
		if i := turnIndex(g.turns, f.Turn); i >= 0 {
			g.turns[i].AgentCost += f.Cost
		}
	}
	return g, nil
}

type spawnRow struct {
	fileID        int64
	toolUse, tool string
	target        string
	turn          int
	fileAgent     string // agent id of the file that made the call ("" = main)
}

func (d *DB) spawns(ctx context.Context, acct, id string) ([]spawnRow, error) {
	rows, err := d.sql().QueryContext(ctx, `SELECT s.file_id, s.tool_use_id, s.tool, s.target, s.turn, f.agent_id FROM spawns s
		JOIN files f ON f.id = s.file_id WHERE f.account = ? AND f.session_id = ?`, acct, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []spawnRow
	for rows.Next() {
		var s spawnRow
		if err := rows.Scan(&s.fileID, &s.toolUse, &s.tool, &s.target, &s.turn, &s.fileAgent); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// placeAgents sets the turn of every subagent file and workflow run: from the
// call that started it, else from the subagent that started it, else from
// when it began.
func placeAgents(det *SessionDetail, mainID int64, spawns []spawnRow, turns []TurnRow) {
	byTool := map[string]spawnRow{}
	byTarget := map[string]spawnRow{}
	for _, s := range spawns {
		byTool[s.toolUse] = s
		if s.target != "" {
			byTarget[s.target] = s
		}
	}
	files := map[string]*FileInfo{}
	for i := range det.Files {
		if f := &det.Files[i]; f.AgentID != "" {
			files[f.AgentID] = f
		}
	}
	byTime := func(t time.Time) int {
		if t.IsZero() || len(turns) == 0 {
			return 0
		}
		i := sort.Search(len(turns), func(i int) bool { return turns[i].At.After(t) })
		if i == 0 {
			return turns[0].Seq
		}
		return turns[i-1].Seq
	}
	var turnOf func(f *FileInfo, depth int) int
	fromSpawn := func(s spawnRow, depth int) (int, bool) {
		if s.fileID == mainID {
			return s.turn, true
		}
		if p := files[s.fileAgent]; p != nil && depth < 16 {
			return turnOf(p, depth+1), true
		}
		return 0, false
	}
	turnOf = func(f *FileInfo, depth int) int {
		if f.Turn > 0 {
			return f.Turn
		}
		t := 0
		if s, ok := byTool[f.ToolUseID]; ok && f.ToolUseID != "" {
			t, _ = fromSpawn(s, depth)
		}
		if t == 0 && f.RunID != "" {
			if s, ok := byTarget[f.RunID]; ok {
				t, _ = fromSpawn(s, depth)
			}
		}
		if t == 0 {
			if s, ok := byTarget[f.AgentID]; ok {
				t, _ = fromSpawn(s, depth)
			}
		}
		if t == 0 && f.Parent != "" && depth < 16 {
			if p := files[f.Parent]; p != nil {
				t = turnOf(p, depth+1)
			}
		}
		if t == 0 {
			t = byTime(f.Started)
		}
		f.Turn = t
		return t
	}
	for i := range det.Files {
		if f := &det.Files[i]; f.Kind != "main" {
			turnOf(f, 0)
		}
	}
	for i := range det.Runs {
		r := &det.Runs[i]
		if s, ok := byTarget[r.ID]; ok && s.fileID == mainID {
			r.Turn = s.turn
			continue
		}
		for _, f := range det.Files {
			if f.RunID == r.ID && f.Turn > 0 && (r.Turn == 0 || f.Turn < r.Turn) {
				r.Turn = f.Turn
			}
		}
		if r.Turn == 0 {
			r.Turn = byTime(r.At)
		}
	}
}

func turnIndex(turns []TurnRow, seq int) int {
	i := sort.Search(len(turns), func(i int) bool { return turns[i].Seq >= seq })
	if i < len(turns) && turns[i].Seq == seq {
		return i
	}
	return -1
}

func (d *DB) turnRows(ctx context.Context, fileID int64) ([]TurnRow, error) {
	rows, err := d.sql().QueryContext(ctx, `SELECT t.seq, t.offset, t.ts, t.last_ts, t.kind, t.text, t.chars, t.images, t.tool_calls, t.agents, CASE WHEN t.duration_ms > 0 THEN t.duration_ms ELSE t.active_ms END,
		COALESCE(r.n, 0), COALESCE(r.i, 0), COALESCE(r.o, 0), COALESCE(r.cr, 0), COALESCE(r.cw, 0), COALESCE(r.cost, 0)
		FROM turns t LEFT JOIN (SELECT turn, COUNT(*) AS n, SUM(input) AS i, SUM(output) AS o, SUM(cache_read) AS cr,
			SUM(cache_5m + cache_1h) AS cw, SUM(COALESCE(cost, 0)) AS cost
			FROM responses WHERE file_id = ? AND kind <> 'adjustment' GROUP BY turn) r ON r.turn = t.seq
		WHERE t.file_id = ? ORDER BY t.seq`, fileID, fileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TurnRow
	for rows.Next() {
		var t TurnRow
		var at, end int64
		if err := rows.Scan(&t.Seq, &t.Offset, &at, &end, &t.Kind, &t.Prompt, &t.Chars, &t.Images, &t.ToolCalls, &t.Agents, &t.DurationMS,
			&t.Responses, &t.Tokens.Input, &t.Tokens.Output, &t.Tokens.CacheRead, &t.Tokens.CacheWrite, &t.Cost); err != nil {
			return nil, err
		}
		t.At, t.End = ms(at), ms(end)
		if t.Seq == 0 {
			// Before the first prompt: session start hooks and the like. Keep it
			// only when something happened there.
			if t.Responses == 0 && t.ToolCalls == 0 {
				continue
			}
			t.Kind = "start"
		}
		t.Headline = conv.Headline(t.Prompt)
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if i+1 < len(out) {
			out[i].Next = out[i+1].Offset
		}
	}
	return out, nil
}

// Turns lists every turn of a session.
func (d *DB) Turns(ctx context.Context, ref string) (*TurnList, error) {
	g, err := d.loadGraph(ctx, ref)
	if err != nil {
		return nil, err
	}
	l := &TurnList{Session: g.det.SessionRow, Turns: g.turns}
	if g.main != nil {
		l.Path, l.Missing = g.main.Path, g.main.Missing
	}
	return l, nil
}

// ErrNoTranscript is returned when a turn's transcript is gone from disk.
var ErrNoTranscript = errors.New("the transcript is gone from disk (ccli kept only the totals and prompts)")

// Turn reads one turn from the transcript. seq -1 means the last turn.
func (d *DB) Turn(ctx context.Context, ref string, seq int) (*TurnDetail, error) {
	ds, err := d.TurnRange(ctx, ref, seq, seq, true)
	if len(ds) == 0 {
		return nil, err
	}
	return ds[0], err
}

// TurnRange reads turns from through to (from -1: only the last turn),
// loading the session once. Without all, turns nobody typed are skipped,
// except the first one asked for. When the transcript is gone the details
// come back without conversations, with ErrNoTranscript.
func (d *DB) TurnRange(ctx context.Context, ref string, from, to int, all bool) ([]*TurnDetail, error) {
	g, err := d.loadGraph(ctx, ref)
	if err != nil {
		return nil, err
	}
	if g.main == nil || len(g.turns) == 0 {
		return nil, fmt.Errorf("session %s has no turns", ShortID(g.det.ID))
	}
	first, last := len(g.turns)-1, len(g.turns)-1
	if from >= 0 {
		if first = turnIndex(g.turns, from); first < 0 {
			return nil, fmt.Errorf("session %s has no turn %d (turns run %d to %d)", ShortID(g.det.ID), from, g.turns[0].Seq, g.turns[len(g.turns)-1].Seq)
		}
		last = sort.Search(len(g.turns), func(i int) bool { return g.turns[i].Seq > to }) - 1
		if last < first {
			return nil, fmt.Errorf("turns %d to %d: the range is empty", from, to)
		}
	}
	var out []*TurnDetail
	var gone error
	for i := first; i <= last; i++ {
		row := g.turns[i]
		if i != first && !all && row.Kind != "human" {
			continue
		}
		det := &TurnDetail{Session: g.det.SessionRow, Row: row, Prev: -1, Next: -1}
		// Neighbours a person typed, unless every turn was asked for.
		for j := i - 1; j >= 0; j-- {
			if all || g.turns[j].Kind == "human" {
				det.Prev = g.turns[j].Seq
				break
			}
		}
		for j := i + 1; j < len(g.turns); j++ {
			if all || g.turns[j].Kind == "human" {
				det.Next = g.turns[j].Seq
				break
			}
		}
		det.Agents = agentTree(g.det.Files, func(f FileInfo) bool { return f.Kind == "subagent" && f.Turn == row.Seq })
		for _, r := range g.det.Runs {
			if r.Turn == row.Seq {
				det.Runs = append(det.Runs, r)
			}
		}
		out = append(out, det)
		if g.main.Missing || gone != nil {
			gone = ErrNoTranscript
			continue
		}
		start := row.Offset
		if row.Seq == 0 {
			start = 0
		}
		tr, err := d.readTurn(g.det.Account, g.main.Path, start, row.Next, row.Seq)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				gone = ErrNoTranscript
				continue
			}
			return nil, err
		}
		det.Turn = tr
	}
	return out, gone
}

// agentTree nests the matching subagents under the subagents that started
// them; a subagent whose parent is not in the set is a root.
func agentTree(files []FileInfo, keep func(FileInfo) bool) []AgentNode {
	in := map[string]bool{}
	for _, f := range files {
		if keep(f) {
			in[f.AgentID] = true
		}
	}
	kids := map[string][]FileInfo{}
	for _, f := range files {
		if !keep(f) {
			continue
		}
		parent := f.Parent
		if !in[parent] {
			parent = ""
		}
		kids[parent] = append(kids[parent], f)
	}
	var build func(parent string, depth int) []AgentNode
	build = func(parent string, depth int) []AgentNode {
		fs := kids[parent]
		sort.Slice(fs, func(i, j int) bool { return fs[i].Started.Before(fs[j].Started) })
		var out []AgentNode
		for _, f := range fs {
			n := AgentNode{FileInfo: f, Label: AgentLabel(f)}
			if depth < 16 && f.AgentID != "" {
				n.Children = build(f.AgentID, depth+1)
			}
			out = append(out, n)
		}
		return out
	}
	return build("", 0)
}

// AgentLabel names a subagent: its type and what it was asked to do. A
// workflow agent is named by its label in the workflow.
func AgentLabel(f FileInfo) string {
	typ := f.AgentType
	if typ == "" {
		typ = f.Attribution
	}
	if f.Kind == "workflow" && f.Description != "" && (typ == "" || typ == "general-purpose" || typ == "workflow-subagent") {
		return f.Description
	}
	if f.Skill != "" {
		typ = "/" + f.Skill
	}
	if typ == "" {
		typ = "agent"
	}
	desc := f.Description
	if f.Skill != "" {
		// A skill run as a subagent records its slash command as the
		// description; the arguments are what is worth showing.
		desc = strings.TrimSpace(strings.TrimPrefix(desc, "/"+f.Skill))
	}
	if desc == "" {
		desc = f.Task
	}
	if desc == "" {
		return typ
	}
	return typ + " " + desc
}

// Agent reads one subagent's or workflow agent's transcript.
func (d *DB) Agent(ctx context.Context, ref, agentID string) (*AgentLog, error) {
	g, err := d.loadGraph(ctx, ref)
	if err != nil {
		return nil, err
	}
	var f *FileInfo
	for i := range g.det.Files {
		if x := &g.det.Files[i]; x.Kind != "main" && (x.AgentID == agentID || strings.HasPrefix(x.AgentID, agentID) && len(agentID) >= 6) {
			if f != nil && f.AgentID != x.AgentID {
				return nil, fmt.Errorf("agent %q is ambiguous in session %s", agentID, ShortID(g.det.ID))
			}
			f = x
		}
	}
	if f == nil {
		return nil, fmt.Errorf("session %s has no agent %q", ShortID(g.det.ID), agentID)
	}
	log := &AgentLog{Session: g.det.SessionRow, Agent: AgentNode{FileInfo: *f, Label: AgentLabel(*f)}}
	log.Agent.Children = agentTree(g.det.Files, func(x FileInfo) bool { return x.Kind == f.Kind && x.Parent == f.AgentID && x.AgentID != f.AgentID })
	if f.RunID != "" {
		for i := range g.det.Runs {
			if g.det.Runs[i].ID == f.RunID {
				r := g.det.Runs[i]
				log.Run = &r
			}
		}
		log.Status = RunAgentStatus(filepath.Dir(f.Path))[f.AgentID]
	}
	if f.Missing {
		return log, ErrNoTranscript
	}
	turns, err := d.readAgent(g.det.Account, f.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return log, ErrNoTranscript
		}
		return nil, err
	}
	log.Turns = turns
	return log, nil
}

// RunAgents lists a workflow run's agents with their journal status.
func (d *DB) RunAgents(ctx context.Context, ref, runID string) (*RunInfo, []AgentNode, map[string]string, error) {
	g, err := d.loadGraph(ctx, ref)
	if err != nil {
		return nil, nil, nil, err
	}
	var run *RunInfo
	for i := range g.det.Runs {
		if r := &g.det.Runs[i]; r.ID == runID || strings.HasPrefix(r.ID, runID) && len(runID) >= 6 {
			run = r
		}
	}
	if run == nil {
		return nil, nil, nil, fmt.Errorf("session %s has no workflow run %q", ShortID(g.det.ID), runID)
	}
	nodes := agentTree(g.det.Files, func(f FileInfo) bool { return f.Kind == "workflow" && f.RunID == run.ID })
	status := map[string]string{}
	for _, f := range g.det.Files {
		if f.RunID == run.ID {
			status = RunAgentStatus(filepath.Dir(f.Path))
			break
		}
	}
	return run, nodes, status, nil
}

// RunAgentStatus reads a workflow run's journal from its folder: done,
// failed, or unfinished for each agent that started.
func RunAgentStatus(dir string) map[string]string {
	out := map[string]string{}
	b, err := os.ReadFile(filepath.Join(dir, "journal.jsonl"))
	if err != nil {
		return out
	}
	for _, ln := range strings.Split(string(b), "\n") {
		typ, agent := jsonField(ln, "type"), jsonField(ln, "agentId")
		if agent == "" {
			continue
		}
		switch typ {
		case "started":
			if out[agent] == "" {
				out[agent] = "unfinished"
			}
		case "result":
			out[agent] = "done"
		case "failed":
			out[agent] = "failed"
		}
	}
	return out
}

// jsonField pulls a top-level string field out of a short JSON line without
// decoding the rest (journal results can be large).
func jsonField(ln, key string) string {
	k := `"` + key + `":"`
	i := strings.Index(ln, k)
	if i < 0 {
		return ""
	}
	rest := ln[i+len(k):]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// PromptRow is one human prompt in a list across sessions.
type PromptRow struct {
	Account   string `json:"account"`
	SessionID string `json:"session_id"`
	Title     string `json:"session_title"`
	Project   string `json:"project"`
	TurnRow
}

// PromptPage is a filtered list of prompts.
type PromptPage struct {
	Filter  Filter      `json:"filter"`
	Prompts []PromptRow `json:"prompts"`
	Matched int         `json:"matched"`
	Cost    float64     `json:"cost"`
}

// Prompts lists the prompts people typed, across sessions, with what each
// turn cost (agents included). Sort: last (default), cost, tokens, tools,
// duration, length.
func (d *DB) Prompts(ctx context.Context, f Filter) (PromptPage, error) {
	page := PromptPage{Filter: f}
	sessions, err := d.loadSessions(ctx, Filter{Accounts: f.Accounts, Project: f.Project, Dirs: f.Dirs, Tools: f.Tools, Model: f.Model})
	if err != nil {
		return page, err
	}
	type skeyT = skey
	meta := map[skeyT]SessionRow{}
	for _, s := range sessions {
		if !f.Show.keeps(s.Hidden) {
			continue
		}
		meta[skey{s.Account, s.ID}] = s
	}
	q := `SELECT f.account, f.session_id, t.seq, t.offset, t.ts, t.last_ts, t.kind, t.text, t.chars, t.images, t.tool_calls, t.agents, CASE WHEN t.duration_ms > 0 THEN t.duration_ms ELSE t.active_ms END,
		COALESCE(r.n, 0), COALESCE(r.i, 0), COALESCE(r.o, 0), COALESCE(r.cr, 0), COALESCE(r.cw, 0), COALESCE(r.cost, 0)
		FROM turns t JOIN files f ON f.id = t.file_id AND f.kind = 'main'
		LEFT JOIN (SELECT file_id, turn, COUNT(*) AS n, SUM(input) AS i, SUM(output) AS o, SUM(cache_read) AS cr,
			SUM(cache_5m + cache_1h) AS cw, SUM(COALESCE(cost, 0)) AS cost
			FROM responses WHERE kind <> 'adjustment' AND file_id IN (SELECT id FROM files WHERE kind = 'main') GROUP BY file_id, turn) r ON r.file_id = t.file_id AND r.turn = t.seq
		WHERE t.kind = 'human' AND t.ts BETWEEN ? AND ?`
	args := []any{f.sinceMS(), f.untilMS()}
	if f.Search != "" {
		q += ` AND t.text LIKE ? ESCAPE '\'`
		args = append(args, "%"+likeEscape(f.Search)+"%")
	}
	rows, err := d.sql().QueryContext(ctx, q, args...)
	if err != nil {
		return page, err
	}
	var out []PromptRow
	for rows.Next() {
		var p PromptRow
		var at, end int64
		if err := rows.Scan(&p.Account, &p.SessionID, &p.Seq, &p.Offset, &at, &end, &p.Kind, &p.Prompt, &p.Chars, &p.Images, &p.ToolCalls, &p.Agents, &p.DurationMS,
			&p.Responses, &p.Tokens.Input, &p.Tokens.Output, &p.Tokens.CacheRead, &p.Tokens.CacheWrite, &p.Cost); err != nil {
			rows.Close()
			return page, err
		}
		s, ok := meta[skey{p.Account, p.SessionID}]
		if !ok {
			continue // filtered out by account, project or model
		}
		p.At, p.End = ms(at), ms(end)
		p.Title, p.Project = s.Title, s.Project
		p.Headline = conv.Headline(p.Prompt)
		out = append(out, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return page, err
	}
	// What a turn's agents cost needs each session's graph; for a list not
	// sorted by cost only the page shown needs it.
	page.Matched = len(out)
	if f.Sort == "cost" {
		if err := d.addAgentCosts(ctx, out); err != nil {
			return page, err
		}
	}
	sortPrompts(out, f.Sort)
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	if f.Sort != "cost" {
		if err := d.addAgentCosts(ctx, out); err != nil {
			return page, err
		}
	}
	for _, p := range out {
		page.Cost += p.Total()
	}
	page.Prompts = out
	return page, nil
}

// addAgentCosts fills AgentCost for prompts, placing each session's agents
// in their turns.
func (d *DB) addAgentCosts(ctx context.Context, ps []PromptRow) error {
	bySession := map[skey][]int{}
	for i, p := range ps {
		k := skey{p.Account, p.SessionID}
		bySession[k] = append(bySession[k], i)
	}
	for k, idx := range bySession {
		g, err := d.graphFor(ctx, k.account, k.id)
		if err != nil {
			return err
		}
		cost := map[int]float64{}
		for _, t := range g.turns {
			cost[t.Seq] = t.AgentCost
		}
		for _, i := range idx {
			ps[i].AgentCost = cost[ps[i].Seq]
		}
	}
	return nil
}

func sortPrompts(ps []PromptRow, by string) {
	less := func(i, j int) bool { return ps[i].At.After(ps[j].At) }
	switch by {
	case "cost":
		less = func(i, j int) bool { return ps[i].Total() > ps[j].Total() }
	case "tokens":
		less = func(i, j int) bool { return ps[i].Tokens.All() > ps[j].Tokens.All() }
	case "tools":
		less = func(i, j int) bool { return ps[i].ToolCalls > ps[j].ToolCalls }
	case "duration", "active":
		less = func(i, j int) bool { return ps[i].DurationMS > ps[j].DurationMS }
	case "length":
		less = func(i, j int) bool { return ps[i].Chars > ps[j].Chars }
	}
	sort.SliceStable(ps, less)
}

func likeEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// readTurn reads one turn of a session's main transcript, in the format of
// the account's tool.
func (d *DB) readTurn(acct, path string, from, to int64, seq int) (*conv.Turn, error) {
	if d.toolOf(acct) == account.Codex {
		return codex.ReadTurn(path, from, to, seq)
	}
	return conv.ReadTurn(path, from, to, seq)
}

// readAgent reads a subagent's whole transcript.
func (d *DB) readAgent(acct, path string) ([]conv.Turn, error) {
	if d.toolOf(acct) == account.Codex {
		return codex.ReadRange(path)
	}
	return conv.ReadRange(path, 0, 0, 1, true)
}
