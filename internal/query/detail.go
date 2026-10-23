package query

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/lr00rl/ccli/internal/account"
	"github.com/lr00rl/ccli/internal/cursoredits"
	"github.com/lr00rl/ccli/internal/cursoride"
	"github.com/lr00rl/ccli/internal/transcript"
)

// SessionDetail is everything ccli knows about one session.
type SessionDetail struct {
	SessionRow
	ByModel   []Share    `json:"by_model"`
	Files     []FileInfo `json:"files"`
	Runs      []RunInfo  `json:"workflow_runs"`
	Tools     []Count    `json:"tools"`
	Skills    []Count    `json:"skills"`
	Commands  []Count    `json:"commands"` // slash commands typed in the session
	Moves     []Move     `json:"moves"`    // copies, moves and forks ccli made of it
	Links     []Link     `json:"links"`
	Processes int        `json:"processes_with_own_totals"`
	Resume    Resume     `json:"resume"`
	// Edits are the files Cursor's agents wrote in the conversation, from
	// Cursor's AI code tracking; nil outside Cursor or when none are known.
	Edits *cursoredits.Edits `json:"cursor_edits,omitempty"`
	// Composer is what a Cursor IDE chat's header says now.
	Composer *cursoride.Composer `json:"cursor_composer,omitempty"`

	graph *graph // turns, with each subagent and run placed in one
}

// FileInfo is one transcript file of the session.
type FileInfo struct {
	ID          int64     `json:"-"`
	Path        string    `json:"path"`
	Kind        string    `json:"kind"` // main, subagent, workflow
	AgentID     string    `json:"agent_id,omitempty"`
	AgentType   string    `json:"agent_type,omitempty"`
	Description string    `json:"description,omitempty"`
	Shape       string    `json:"request_shape,omitempty"`
	Depth       int       `json:"spawn_depth,omitempty"`
	ToolUseID   string    `json:"tool_use_id,omitempty"`
	Parent      string    `json:"parent,omitempty"` // agent id of the spawning subagent, "" for the main transcript
	RunID       string    `json:"run_id,omitempty"`
	Skill       string    `json:"skill,omitempty"`       // a skill run as a subagent
	Phase       string    `json:"phase,omitempty"`       // workflow phase
	Attribution string    `json:"attribution,omitempty"` // agent type from the transcript lines
	Task        string    `json:"task,omitempty"`        // first line of the prompt a subagent was given
	Turn        int       `json:"turn"`                  // turn of the main conversation that started it, 0 if unknown
	Size        int64     `json:"size"`
	Missing     bool      `json:"missing"`
	Responses   int       `json:"responses"`
	Tokens      Tokens    `json:"tokens"`
	Cost        float64   `json:"cost"`
	Started     time.Time `json:"started,omitempty"`
	Last        time.Time `json:"last,omitempty"`
}

// RunInfo summarizes a workflow run.
type RunInfo struct {
	ID         string    `json:"id"`
	Name       string    `json:"name,omitempty"`
	Status     string    `json:"status,omitempty"`
	Summary    string    `json:"summary,omitempty"`
	At         time.Time `json:"started_at,omitempty"`
	DurationMS int64     `json:"duration_ms,omitempty"`
	Launched   int       `json:"launched"`
	Started    int       `json:"started"`
	Results    int       `json:"results"`
	Failed     int       `json:"failed"`
	Agents     int       `json:"agents"` // transcripts on disk
	Turn       int       `json:"turn"`
	Tokens     Tokens    `json:"tokens"`
	Cost       float64   `json:"cost"`
}

// Unfinished is how many agents started and neither returned nor failed:
// stopped with the run, or still running.
func (r RunInfo) Unfinished() int { return max(0, r.Started-r.Results-r.Failed) }

// Count is a name with a number.
type Count struct {
	Name string `json:"name"`
	N    int    `json:"n"`
}

// Link relates the session to another one.
type Link struct {
	Kind      string `json:"kind"` // continues-in, continued-from, team-lead, teammate, copy
	Account   string `json:"account"`
	SessionID string `json:"session_id"`
	Title     string `json:"title,omitempty"`
	Tag       string `json:"tag,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

// Resume is how to reopen the session in Claude Code.
type Resume struct {
	Account   string   `json:"account"`
	ConfigDir string   `json:"config_dir"`
	Cwd       string   `json:"cwd"`
	Command   string   `json:"command"`
	Program   string   `json:"program"` // claude or codex
	Args      []string `json:"args"`
	// Env names the variable that selects the account (CLAUDE_CONFIG_DIR,
	// CODEX_HOME); EnvValue is what to set it to, empty for the default dir.
	Env      string `json:"env"`
	EnvValue string `json:"env_value,omitempty"`
	// ForkArg resumes into a new session id; empty when the tool has none.
	ForkArg   string `json:"fork_arg,omitempty"`
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
}

// ErrNotFound is returned when no session matches a reference.
var ErrNotFound = errors.New("no session matches")

// CopiesOf lists the accounts that hold a session, by name.
func (d *DB) CopiesOf(ctx context.Context, id string) ([]string, error) {
	rows, err := d.sql().QueryContext(ctx, `SELECT DISTINCT account FROM files WHERE session_id = ? AND kind = 'main' ORDER BY account`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Resolve finds the session for a reference: a full id or a unique prefix,
// optionally "account:" in front.
func (d *DB) Resolve(ctx context.Context, ref string) (account, id string, err error) {
	acct := ""
	if i := strings.IndexByte(ref, ':'); i > 0 {
		acct, ref = ref[:i], ref[i+1:]
	}
	if len(ref) < 4 {
		return "", "", fmt.Errorf("session reference %q is too short (use at least 4 characters)", ref)
	}
	// Compare the prefix literally: "%" and "_" in a reference are not wildcards.
	rows, err := d.sql().QueryContext(ctx, `SELECT account, session_id, MAX(last_ts), MIN(missing) FROM files
		WHERE substr(session_id, 1, ?) = ? GROUP BY account, session_id ORDER BY MIN(missing), MAX(last_ts) DESC`, len(ref), ref)
	if err != nil {
		return "", "", err
	}
	defer rows.Close()
	type hit struct{ account, id string }
	var hits []hit
	ids := map[string]bool{}
	for rows.Next() {
		var h hit
		var last int64
		var missing int
		if err := rows.Scan(&h.account, &h.id, &last, &missing); err != nil {
			return "", "", err
		}
		if acct != "" && h.account != acct {
			continue
		}
		hits = append(hits, h)
		ids[h.id] = true
	}
	if len(hits) == 0 {
		return "", "", fmt.Errorf("%w %q", ErrNotFound, ref)
	}
	if len(ids) > 1 {
		var names []string
		for id := range ids {
			names = append(names, id[:min(13, len(id))])
		}
		sort.Strings(names)
		return "", "", fmt.Errorf("%q matches %d sessions (%s); use a longer prefix", ref, len(ids), strings.Join(names, ", "))
	}
	// The same session copied into several accounts: the copy still on disk
	// with the latest activity is the one in use. Callers list the others
	// (as "copy" links) so the choice is visible.
	return hits[0].account, hits[0].id, nil
}

// Session returns the detail of one session.
func (d *DB) Session(ctx context.Context, ref string) (*SessionDetail, error) {
	if d.cache == nil {
		return d.session(ctx, ref)
	}
	d.cacheMu.Lock()
	det, gen := d.cache[ref], d.cacheGen
	d.cacheMu.Unlock()
	if det != nil {
		return det, nil
	}
	det, err := d.session(ctx, ref)
	if err == nil {
		// A detail loaded while the cache was cleared may already be stale.
		d.cacheMu.Lock()
		if d.cacheGen == gen {
			d.cache[ref] = det
		}
		d.cacheMu.Unlock()
	}
	return det, err
}

func (d *DB) session(ctx context.Context, ref string) (*SessionDetail, error) {
	acct, id, err := d.Resolve(ctx, ref)
	if err != nil {
		return nil, err
	}
	all, err := d.querySessions(ctx, Filter{Accounts: []string{acct}}, &skey{acct, id})
	if err != nil {
		return nil, err
	}
	var det *SessionDetail
	for _, s := range all {
		if s.ID == id {
			det = &SessionDetail{SessionRow: s}
		}
	}
	if det == nil {
		return nil, fmt.Errorf("%w %q", ErrNotFound, ref)
	}
	if err := d.fillFiles(ctx, det); err != nil {
		return nil, err
	}
	if err := d.fillUsage(ctx, det); err != nil {
		return nil, err
	}
	if err := d.fillLinks(ctx, det); err != nil {
		return nil, err
	}
	if err := d.fillMoves(ctx, det); err != nil {
		return nil, err
	}
	d.fillCursor(ctx, det)
	if det.graph, err = d.buildGraph(ctx, det); err != nil {
		return nil, err
	}
	det.Resume = d.resume(det)
	return det, nil
}

func (d *DB) fillFiles(ctx context.Context, det *SessionDetail) error {
	rows, err := d.sql().QueryContext(ctx, `SELECT f.id, f.path, f.kind, f.agent_id, f.agent_type, f.description, f.request_shape, f.spawn_depth,
		f.tool_use_id, f.run_id, f.size, f.missing, f.first_ts, f.last_ts, f.skill, f.phase, f.attribution, f.first_prompt,
		COALESCE(NULLIF(f.parent_agent, ''), (SELECT sf.agent_id FROM spawns s JOIN files sf ON sf.id = s.file_id WHERE s.tool_use_id = f.tool_use_id AND f.tool_use_id <> ''
			AND sf.account = f.account AND sf.session_id = f.session_id ORDER BY sf.id LIMIT 1), ''),
		COUNT(r.key), COALESCE(SUM(r.input), 0), COALESCE(SUM(r.output), 0), COALESCE(SUM(r.cache_read), 0),
		COALESCE(SUM(r.cache_5m + r.cache_1h), 0), COALESCE(SUM(r.cost), 0)
		FROM files f LEFT JOIN responses r ON r.file_id = f.id AND r.kind <> 'adjustment'
		WHERE f.account = ? AND f.session_id = ? GROUP BY f.id ORDER BY f.kind = 'main' DESC, f.first_ts`, det.Account, det.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var fi FileInfo
		var missing int
		var first, last int64
		if err := rows.Scan(&fi.ID, &fi.Path, &fi.Kind, &fi.AgentID, &fi.AgentType, &fi.Description, &fi.Shape, &fi.Depth,
			&fi.ToolUseID, &fi.RunID, &fi.Size, &missing, &first, &last, &fi.Skill, &fi.Phase, &fi.Attribution, &fi.Task, &fi.Parent,
			&fi.Responses, &fi.Tokens.Input, &fi.Tokens.Output, &fi.Tokens.CacheRead, &fi.Tokens.CacheWrite, &fi.Cost); err != nil {
			return err
		}
		if fi.Kind == "main" {
			fi.Task = ""
		}
		fi.Missing = missing != 0
		fi.Started, fi.Last = ms(first), ms(last)
		det.Files = append(det.Files, fi)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rrows, err := d.sql().QueryContext(ctx, `SELECT run_id, launched, started, results, failed, name, status, summary, start_ms, duration_ms
		FROM runs WHERE account = ? AND session_id = ? ORDER BY run_id`, det.Account, det.ID)
	if err != nil {
		return err
	}
	defer rrows.Close()
	type agg struct {
		n     int
		tok   Tokens
		cost  float64
		first time.Time
	}
	byRun := map[string]*agg{}
	for _, f := range det.Files {
		if f.RunID == "" {
			continue
		}
		a := byRun[f.RunID]
		if a == nil {
			a = &agg{}
			byRun[f.RunID] = a
		}
		a.n++
		a.tok.add(f.Tokens)
		a.cost += f.Cost
		if !f.Started.IsZero() && (a.first.IsZero() || f.Started.Before(a.first)) {
			a.first = f.Started
		}
	}
	seen := map[string]bool{}
	for rrows.Next() {
		var r RunInfo
		var start int64
		if err := rrows.Scan(&r.ID, &r.Launched, &r.Started, &r.Results, &r.Failed, &r.Name, &r.Status, &r.Summary, &start, &r.DurationMS); err != nil {
			return err
		}
		r.At = ms(start)
		if a := byRun[r.ID]; a != nil {
			r.Agents, r.Tokens, r.Cost = a.n, a.tok, a.cost
			if r.At.IsZero() {
				r.At = a.first
			}
		}
		seen[r.ID] = true
		det.Runs = append(det.Runs, r)
	}
	if err := rrows.Err(); err != nil {
		return err
	}
	// Runs whose journal and state file are gone still have transcripts.
	for id, a := range byRun {
		if !seen[id] {
			det.Runs = append(det.Runs, RunInfo{ID: id, Agents: a.n, Tokens: a.tok, Cost: a.cost, At: a.first})
		}
	}
	sort.Slice(det.Runs, func(i, j int) bool { return det.Runs[i].At.Before(det.Runs[j].At) })
	return nil
}

func (d *DB) fillUsage(ctx context.Context, det *SessionDetail) error {
	rows, err := d.sql().QueryContext(ctx, `SELECT model, SUM(kind <> 'model'), SUM(input), SUM(output), SUM(cache_read), SUM(cache_5m + cache_1h), SUM(COALESCE(cost, 0))
		FROM responses WHERE account = ? AND session_id = ? GROUP BY model ORDER BY 7 DESC`, det.Account, det.ID)
	if err != nil {
		return err
	}
	byName := map[string]int{}
	for rows.Next() {
		var s Share
		if err := rows.Scan(&s.Name, &s.Sessions, &s.Tokens.Input, &s.Tokens.Output, &s.Tokens.CacheRead, &s.Tokens.CacheWrite, &s.Cost); err != nil {
			rows.Close()
			return err
		}
		if s.Name == "" && s.Tokens.All() == 0 && s.Cost == 0 {
			continue // Cursor replies whose model is not known
		}
		s.Name = modelName(s.Name)
		if i, ok := byName[s.Name]; ok {
			m := &det.ByModel[i]
			m.Sessions += s.Sessions
			m.Tokens.add(s.Tokens)
			m.Cost += s.Cost
			continue
		}
		byName[s.Name] = len(det.ByModel)
		det.ByModel = append(det.ByModel, s)
	}
	rows.Close()
	sort.SliceStable(det.ByModel, func(i, j int) bool { return det.ByModel[i].Cost > det.ByModel[j].Cost })
	trows, err := d.sql().QueryContext(ctx, `SELECT t.name, t.skill, SUM(t.n) FROM tools t JOIN files f ON f.id = t.file_id
		WHERE f.account = ? AND f.session_id = ? GROUP BY t.name, t.skill ORDER BY 3 DESC`, det.Account, det.ID)
	if err != nil {
		return err
	}
	for trows.Next() {
		var name, skill string
		var n int
		if err := trows.Scan(&name, &skill, &n); err != nil {
			trows.Close()
			return err
		}
		switch {
		case name == "Command":
			if cmd, ok := transcript.CommandName(skill); ok {
				det.Commands = append(det.Commands, Count{"/" + cmd, n})
			}
		case skill != "":
			det.Skills = append(det.Skills, Count{skill, n})
		default:
			det.Tools = append(det.Tools, Count{name, n})
		}
	}
	trows.Close()
	d.applyCursorDetail(ctx, det)
	return d.sql().QueryRowContext(ctx, `SELECT COUNT(DISTINCT p.start_ms) FROM snapshots p JOIN files f ON f.id = p.file_id
		WHERE f.account = ? AND f.session_id = ? AND p.imported = 0`, det.Account, det.ID).Scan(&det.Processes)
}

func (d *DB) fillLinks(ctx context.Context, det *SessionDetail) error {
	add := func(kind, acct, id, detail string) {
		det.Links = append(det.Links, Link{Kind: kind, Account: acct, SessionID: id, Detail: detail})
	}
	q := func(query string, args ...any) error {
		rows, err := d.sql().QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var kind, acct, id, detail string
			if err := rows.Scan(&kind, &acct, &id, &detail); err != nil {
				return err
			}
			add(kind, acct, id, detail)
		}
		return rows.Err()
	}
	// Continuations, both ways.
	if err := q(`SELECT DISTINCT 'continues-in', COALESCE(t.account, f.account), l.target, '' FROM links l JOIN files f ON f.id = l.file_id
		LEFT JOIN files t ON t.session_id = l.target AND t.kind = 'main'
		WHERE l.kind = 'continued-in' AND f.account = ? AND f.session_id = ?`, det.Account, det.ID); err != nil {
		return err
	}
	if err := q(`SELECT DISTINCT 'continued-from', f.account, f.session_id, '' FROM links l JOIN files f ON f.id = l.file_id
		WHERE l.kind = 'continued-in' AND l.target = ?`, det.ID); err != nil {
		return err
	}
	// Teams: a team is named session-<first 8 characters of the lead's id>.
	// User-named teams carry no lead id, so only that exact form is followed.
	if lead, ok := strings.CutPrefix(det.Team, "session-"); ok && len(lead) == 8 && !strings.HasPrefix(det.ID, lead) {
		if err := q(`SELECT DISTINCT 'team-lead', account, session_id, ? FROM files WHERE kind = 'main' AND substr(session_id, 1, 8) = ? AND session_id <> ?`,
			det.Team, lead, det.ID); err != nil {
			return err
		}
	}
	if len(det.ID) >= 8 {
		if err := q(`SELECT DISTINCT 'teammate', account, session_id, MAX(agent_name) FROM files WHERE team_name = ? AND session_id <> ? GROUP BY account, session_id`,
			"session-"+ShortID(det.ID), det.ID); err != nil {
			return err
		}
	}
	// The same session in another account (a copy).
	if err := q(`SELECT DISTINCT 'copy', account, session_id, '' FROM files WHERE session_id = ? AND account <> ? AND kind = 'main'`, det.ID, det.Account); err != nil {
		return err
	}
	// Titles and tags for the linked sessions.
	tags, err := d.st.Tags(ctx)
	if err != nil {
		return err
	}
	for i := range det.Links {
		l := &det.Links[i]
		l.Tag = tags[l.SessionID]
		var custom, ai, summary, prompt string
		err := d.sql().QueryRowContext(ctx, `SELECT custom_title, ai_title, summary, first_prompt FROM files WHERE account = ? AND session_id = ? AND kind = 'main' LIMIT 1`,
			l.Account, l.SessionID).Scan(&custom, &ai, &summary, &prompt)
		if err == nil {
			l.Title, _ = title(custom, ai, summary, prompt)
		}
	}
	return nil
}

func (d *DB) resume(det *SessionDetail) Resume {
	r := Resume{Account: det.Account, Cwd: det.Cwd}
	acct := account.Account{Name: det.Account, Tool: d.toolOf(det.Account)}
	for _, a := range d.accts {
		if a.Name == det.Account {
			acct = a
		}
	}
	r.ConfigDir = acct.Dir
	if acct.Dir != "" {
		r.Env, r.EnvValue = acct.SelectEnv()
	}
	switch acct.Kind() {
	case account.Codex:
		r.Program, r.Args = "codex", []string{"resume", det.ID}
	case account.Pi:
		r.Program, r.Args = "pi", []string{"--session", det.ID}
	case account.Cursor:
		r.Program, r.Args = "agent", []string{"--resume", det.ID}
	case account.CursorIDE:
		r.Program = "Cursor IDE"
		r.Reason = "this session is in the Cursor IDE; open it from the Agents window in " + det.Cwd
		return r
	default:
		r.Program, r.Args, r.ForkArg = "claude", []string{"--resume", det.ID}, "--fork-session"
	}
	line := func(args ...string) string {
		cmd := r.Program + " " + strings.Join(args, " ")
		if r.EnvValue != "" {
			cmd = r.Env + "=" + shellQuote(r.EnvValue) + " " + cmd
		}
		return cmd
	}
	r.Command = line(r.Args...)
	// A Cursor transcript directory is where the log lives, not the project
	// the session worked in. Resuming from there opens the wrong folder.
	if det.Cwd != "" && !cursorStoreDir(det.Cwd) {
		r.Command = "cd " + shellQuote(det.Cwd) + " && " + r.Command
	}
	switch {
	case r.ConfigDir == "":
		r.Reason = "account " + det.Account + " was not found on this machine"
	case det.Missing:
		r.Reason = "the transcript is gone from disk"
	case r.Program == "codex" && archived(det, r.ConfigDir):
		// codex resume looks only among the threads that are not archived.
		r.Reason = "Codex has archived this thread; to resume it, first run: " + line("unarchive", det.ID)
	case det.Live:
		r.Reason = "a " + r.Program + " process is running this session"
	default:
		r.Available = true
	}
	return r
}

// archived reports whether a Codex session's own rollout is in the home's
// archived_sessions folder.
func archived(det *SessionDetail, home string) bool {
	dir := filepath.Join(home, "archived_sessions") + string(filepath.Separator)
	for _, f := range det.Files {
		if f.Kind == "main" && !f.Missing {
			return strings.HasPrefix(f.Path, dir)
		}
	}
	return false
}

// cursorStoreDir is where the CLI keeps transcripts, not a project folder.
func cursorStoreDir(p string) bool {
	return strings.Contains(p, "/.cursor/projects/") || strings.Contains(p, "/.cursor/chats/")
}

func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r == '/' || r == '-' || r == '_' || r == '.' || r == '~' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z')
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Move is one copy, move or fork of a session that ccli carried out.
type Move struct {
	At           time.Time `json:"at"`
	Mode         string    `json:"mode"`
	From         string    `json:"from"`
	To           string    `json:"to"`
	NewSessionID string    `json:"new_session_id,omitempty"`
	Bytes        int64     `json:"bytes"`
	Files        int       `json:"files"`
}

func (d *DB) fillMoves(ctx context.Context, det *SessionDetail) error {
	ts, err := d.st.Transfers(ctx, det.ID)
	if err != nil {
		return err
	}
	for _, t := range ts {
		n := len(det.Moves)
		if n > 0 {
			m := &det.Moves[n-1]
			if m.At.Equal(t.At) && m.Mode == t.Mode && m.From == t.FromAccount && m.To == t.ToAccount && m.NewSessionID == t.NewSessionID {
				m.Bytes += t.To - t.From
				m.Files++
				continue
			}
		}
		det.Moves = append(det.Moves, Move{At: t.At, Mode: t.Mode, From: t.FromAccount, To: t.ToAccount,
			NewSessionID: t.NewSessionID, Bytes: t.To - t.From, Files: 1})
	}
	return nil
}
