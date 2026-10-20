// Package store keeps ccli's index in SQLite. Aggregates are stored per
// transcript file so a single file can be re-read without touching others.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/lr00rl/ccli/internal/transcript"
)

// SchemaVersion changes when the tables change. A database with another
// version is set aside (renamed, never deleted) and a new one is built, because
// it may hold history of transcripts Claude Code has since deleted.
const SchemaVersion = "1"

const schema = `
CREATE TABLE IF NOT EXISTS meta(key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS files(
  id INTEGER PRIMARY KEY,
  path TEXT NOT NULL UNIQUE,
  account TEXT NOT NULL,
  session_id TEXT NOT NULL,
  project_key TEXT NOT NULL,
  kind TEXT NOT NULL,
  agent_id TEXT NOT NULL DEFAULT '',
  run_id TEXT NOT NULL DEFAULT '',
  agent_type TEXT NOT NULL DEFAULT '',
  description TEXT NOT NULL DEFAULT '',
  tool_use_id TEXT NOT NULL DEFAULT '',
  spawn_depth INTEGER NOT NULL DEFAULT 0,
  request_shape TEXT NOT NULL DEFAULT '',
  size INTEGER NOT NULL DEFAULT 0,
  mtime INTEGER NOT NULL DEFAULT 0,
  born INTEGER NOT NULL DEFAULT 0,
  offset INTEGER NOT NULL DEFAULT 0,
  head_hash TEXT NOT NULL DEFAULT '',
  parser INTEGER NOT NULL DEFAULT 0,
  prev_ts INTEGER NOT NULL DEFAULT 0,
  missing INTEGER NOT NULL DEFAULT 0,
  lines INTEGER NOT NULL DEFAULT 0,
  skipped INTEGER NOT NULL DEFAULT 0,
  typed_prompts INTEGER NOT NULL DEFAULT 0,
  other_prompts INTEGER NOT NULL DEFAULT 0,
  tool_calls INTEGER NOT NULL DEFAULT 0,
  active_ms INTEGER NOT NULL DEFAULT 0,
  first_ts INTEGER NOT NULL DEFAULT 0,
  last_ts INTEGER NOT NULL DEFAULT 0,
  cwd TEXT NOT NULL DEFAULT '',
  custom_title TEXT NOT NULL DEFAULT '',
  ai_title TEXT NOT NULL DEFAULT '',
  summary TEXT NOT NULL DEFAULT '',
  first_prompt TEXT NOT NULL DEFAULT '',
  team_name TEXT NOT NULL DEFAULT '',
  agent_name TEXT NOT NULL DEFAULT '',
  bridge_session_id TEXT NOT NULL DEFAULT '',
  entrypoint TEXT NOT NULL DEFAULT '',
  version TEXT NOT NULL DEFAULT '',
  git_branch TEXT NOT NULL DEFAULT '',
  cost_state_usd REAL NOT NULL DEFAULT 0,
  has_cost_state INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS files_session ON files(account, session_id);
CREATE INDEX IF NOT EXISTS files_sid ON files(session_id);
CREATE TABLE IF NOT EXISTS responses(
  key TEXT PRIMARY KEY,
  file_id INTEGER NOT NULL,
  account TEXT NOT NULL,
  session_id TEXT NOT NULL,
  kind TEXT NOT NULL,
  ts INTEGER NOT NULL,
  model TEXT NOT NULL,
  speed TEXT NOT NULL,
  input INTEGER NOT NULL,
  output INTEGER NOT NULL,
  cache_read INTEGER NOT NULL,
  cache_5m INTEGER NOT NULL,
  cache_1h INTEGER NOT NULL,
  web_search INTEGER NOT NULL,
  cost REAL,
  born INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS responses_ts ON responses(ts);
CREATE INDEX IF NOT EXISTS responses_session ON responses(account, session_id);
CREATE INDEX IF NOT EXISTS responses_file ON responses(file_id);
CREATE TABLE IF NOT EXISTS activity(file_id INTEGER NOT NULL, hour INTEGER NOT NULL, ms INTEGER NOT NULL, PRIMARY KEY(file_id, hour));
CREATE TABLE IF NOT EXISTS tools(file_id INTEGER NOT NULL, name TEXT NOT NULL, skill TEXT NOT NULL, n INTEGER NOT NULL, PRIMARY KEY(file_id, name, skill));
CREATE TABLE IF NOT EXISTS links(file_id INTEGER NOT NULL, kind TEXT NOT NULL, target TEXT NOT NULL, PRIMARY KEY(file_id, kind, target));
CREATE TABLE IF NOT EXISTS processes(
  file_id INTEGER NOT NULL, start_ms INTEGER NOT NULL, end_ms INTEGER NOT NULL, model TEXT NOT NULL,
  input INTEGER NOT NULL, output INTEGER NOT NULL, cache_read INTEGER NOT NULL, cache_write INTEGER NOT NULL,
  web_search INTEGER NOT NULL, cost REAL NOT NULL,
  PRIMARY KEY(file_id, start_ms, model)
);
CREATE INDEX IF NOT EXISTS processes_start ON processes(start_ms);
CREATE TABLE IF NOT EXISTS dirty(account TEXT NOT NULL, session_id TEXT NOT NULL, PRIMARY KEY(account, session_id));
CREATE TABLE IF NOT EXISTS spawns(file_id INTEGER NOT NULL, tool_use_id TEXT NOT NULL, PRIMARY KEY(file_id, tool_use_id));
CREATE INDEX IF NOT EXISTS spawns_tool ON spawns(tool_use_id);
CREATE TABLE IF NOT EXISTS runs(
  account TEXT NOT NULL, session_id TEXT NOT NULL, run_id TEXT NOT NULL,
  launched INTEGER NOT NULL DEFAULT 0, started INTEGER NOT NULL DEFAULT 0,
  results INTEGER NOT NULL DEFAULT 0, failed INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(account, session_id, run_id)
);
CREATE TABLE IF NOT EXISTS turns(
  file_id INTEGER NOT NULL, seq INTEGER NOT NULL,
  offset INTEGER NOT NULL DEFAULT 0, ts INTEGER NOT NULL DEFAULT 0, last_ts INTEGER NOT NULL DEFAULT 0,
  kind TEXT NOT NULL DEFAULT '', text TEXT NOT NULL DEFAULT '', chars INTEGER NOT NULL DEFAULT 0, images INTEGER NOT NULL DEFAULT 0,
  tool_calls INTEGER NOT NULL DEFAULT 0, agents INTEGER NOT NULL DEFAULT 0, duration_ms INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(file_id, seq)
);
CREATE TABLE IF NOT EXISTS transfers(
  id INTEGER PRIMARY KEY, at INTEGER NOT NULL, mode TEXT NOT NULL,
  session_id TEXT NOT NULL, new_session_id TEXT NOT NULL DEFAULT '',
  from_account TEXT NOT NULL, to_account TEXT NOT NULL,
  path TEXT NOT NULL, from_offset INTEGER NOT NULL, to_offset INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS transfers_path ON transfers(path);
CREATE INDEX IF NOT EXISTS transfers_session ON transfers(session_id);
CREATE TABLE IF NOT EXISTS hidden(session_id TEXT PRIMARY KEY, at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS tags(session_id TEXT PRIMARY KEY, tag TEXT NOT NULL, at INTEGER NOT NULL);
`

// columns are added to tables created by earlier versions. The schema only
// ever grows this way: a new SchemaVersion would set the database aside, and
// with it the history of transcripts that are gone from disk.
var columns = []struct{ table, name, def string }{
	{"files", "turn", "INTEGER NOT NULL DEFAULT 0"},
	{"files", "replay", "INTEGER NOT NULL DEFAULT 0"},
	{"turns", "prompt_id", "TEXT NOT NULL DEFAULT ''"},
	{"turns", "active_ms", "INTEGER NOT NULL DEFAULT 0"},
	{"files", "skill", "TEXT NOT NULL DEFAULT ''"},
	{"files", "parent_agent", "TEXT NOT NULL DEFAULT ''"},
	{"files", "phase", "TEXT NOT NULL DEFAULT ''"},
	{"files", "attribution", "TEXT NOT NULL DEFAULT ''"},
	{"files", "carry", "TEXT NOT NULL DEFAULT ''"},
	{"responses", "turn", "INTEGER NOT NULL DEFAULT 0"},
	{"responses", "imported", "INTEGER NOT NULL DEFAULT 0"},
	{"responses", "fixed_cost", "INTEGER NOT NULL DEFAULT 0"},
	{"processes", "imported", "INTEGER NOT NULL DEFAULT 0"},
	{"spawns", "turn", "INTEGER NOT NULL DEFAULT 0"},
	{"spawns", "tool", "TEXT NOT NULL DEFAULT ''"},
	{"spawns", "target", "TEXT NOT NULL DEFAULT ''"},
	{"runs", "name", "TEXT NOT NULL DEFAULT ''"},
	{"runs", "status", "TEXT NOT NULL DEFAULT ''"},
	{"runs", "summary", "TEXT NOT NULL DEFAULT ''"},
	{"runs", "start_ms", "INTEGER NOT NULL DEFAULT 0"},
	{"runs", "duration_ms", "INTEGER NOT NULL DEFAULT 0"},
	{"runs", "agent_count", "INTEGER NOT NULL DEFAULT 0"},
	{"runs", "journal_mtime", "INTEGER NOT NULL DEFAULT 0"},
	{"runs", "state_mtime", "INTEGER NOT NULL DEFAULT 0"},
}

const indexes = `
CREATE INDEX IF NOT EXISTS responses_file_turn ON responses(file_id, turn);
CREATE INDEX IF NOT EXISTS spawns_target ON spawns(target);
`

func migrate(db *sql.DB) error {
	have := map[string]map[string]bool{}
	for _, c := range columns {
		if have[c.table] == nil {
			cols, err := tableColumns(db, c.table)
			if err != nil {
				return err
			}
			have[c.table] = cols
		}
		if have[c.table][c.name] {
			continue
		}
		if _, err := db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", c.table, c.name, c.def)); err != nil {
			return err
		}
		have[c.table][c.name] = true
	}
	if err := titlesToTags(db); err != nil {
		return err
	}
	_, err := db.Exec(indexes)
	return err
}

func tableColumns(db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.Query("SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out[n] = true
	}
	return out, rows.Err()
}

// Store wraps the database.
type Store struct {
	db *sql.DB
}

// DefaultPath is $CCLI_DB, else $XDG_DATA_HOME/ccli/ccli.db, else
// ~/.local/share/ccli/ccli.db.
func DefaultPath() (string, error) {
	if p := os.Getenv("CCLI_DB"); p != "" {
		return p, nil
	}
	return filepath.Join(DataDir(), "ccli.db"), nil
}

// DataDir is where ccli keeps its own files.
func DataDir() string {
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "ccli")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "ccli")
}

// Open opens (and if needed creates or rebuilds) the database.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	s, err := openDB(path)
	if err != nil {
		return nil, err
	}
	if v, _ := s.Meta("schema"); v != "" && v != SchemaVersion {
		s.db.Close()
		kept := fmt.Sprintf("%s.schema-%s.%s", path, v, time.Now().Format("20060102-150405"))
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if err := os.Rename(path+suffix, kept+suffix); err != nil && !os.IsNotExist(err) {
				return nil, err
			}
		}
		if s, err = openDB(path); err != nil {
			return nil, err
		}
	}
	db := s.db
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.SetMeta("schema", SchemaVersion); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the handle for read queries.
func (s *Store) DB() *sql.DB { return s.db }

// Meta reads a meta value ("" when unset).
func (s *Store) Meta(key string) (string, error) {
	var v string
	err := s.db.QueryRow("SELECT value FROM meta WHERE key = ?", key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) || err != nil && isNoTable(err) {
		return "", nil
	}
	return v, err
}

func isNoTable(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such table")
}

// SetMeta writes a meta value.
func (s *Store) SetMeta(key, value string) error {
	_, err := s.db.Exec("INSERT INTO meta(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", key, value)
	return err
}

// File is the indexing state of one transcript file.
type File struct {
	ID         int64
	Path       string
	Account    string
	SessionID  string
	ProjectKey string
	Kind       string
	AgentID    string
	RunID      string
	Size       int64
	MTime      int64
	Born       int64
	Offset     int64
	HeadHash   string
	Parser     int
	PrevTS     int64
	Turn       int
	Replay     bool
	Carry      string // format-specific state the next parse continues from
	Missing    bool
}

// Meta about a subagent, from its .meta.json.
type AgentMeta struct {
	AgentType    string
	Description  string
	ToolUseID    string
	SpawnDepth   int
	RequestShape string
	Skill        string // a skill run as a subagent (meta "name")
	ParentAgent  string // the subagent that started this one
	Phase        string // workflow phase
}

// Files returns every known file by path.
func (s *Store) Files() (map[string]File, error) {
	rows, err := s.db.Query(`SELECT id, path, account, session_id, project_key, kind, agent_id, run_id, size, mtime, offset, head_hash, parser, prev_ts, turn, replay, carry, missing FROM files`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]File{}
	for rows.Next() {
		var f File
		var missing int
		if err := rows.Scan(&f.ID, &f.Path, &f.Account, &f.SessionID, &f.ProjectKey, &f.Kind, &f.AgentID, &f.RunID, &f.Size, &f.MTime, &f.Offset, &f.HeadHash, &f.Parser, &f.PrevTS, &f.Turn, &f.Replay, &f.Carry, &missing); err != nil {
			return nil, err
		}
		f.Missing = missing != 0
		out[f.Path] = f
	}
	return out, rows.Err()
}

// CostFunc prices one response; ok is false when the model is unpriced.
type CostFunc func(model, speed string, u transcript.Usage) (float64, bool)

// Update is one parse result to apply to a file.
type Update struct {
	File   File  // identity and new size, mtime, offset, head hash, parser
	Reset  bool  // drop everything recorded for this file first
	From   int64 // offset the parse started at; the update is skipped if another run moved it
	Result transcript.Result
	Agent  *AgentMeta
	// Ledger is the newest transfer id the parse knew of. A transfer
	// recorded for the file since then means the parse may have counted
	// copied bytes as the file's own: the file is then stored as needing a
	// full re-read.
	Ledger int64
}

// Apply writes a batch of updates in one transaction.
func (s *Store) Apply(ctx context.Context, ups []Update, cost CostFunc) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i := range ups {
		if err := apply(ctx, tx, &ups[i], cost); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func apply(ctx context.Context, tx *sql.Tx, u *Update, cost CostFunc) error {
	f := &u.File
	if f.ID == 0 {
		// RETURNING gives the row's id whether it was inserted or, because another
		// run got there first, updated; LastInsertId would be stale in that case.
		if err := tx.QueryRowContext(ctx, `INSERT INTO files(path, account, session_id, project_key, kind, agent_id, run_id) VALUES(?,?,?,?,?,?,?)
			ON CONFLICT(path) DO UPDATE SET account = excluded.account RETURNING id`, f.Path, f.Account, f.SessionID, f.ProjectKey, f.Kind, f.AgentID, f.RunID).Scan(&f.ID); err != nil {
			return err
		}
	}
	if !u.Reset {
		// Another ccli run may have read these bytes since this one planned:
		// the counters below add, so apply them only from the expected offset.
		res, err := tx.ExecContext(ctx, "UPDATE files SET offset = offset WHERE id = ? AND offset = ?", f.ID, u.From)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			return err
		}
	}
	if u.Reset {
		// A re-read may file the transcript elsewhere, as when Cursor's side
		// data shows late that a chat is a subagent. Every row of the file is
		// rewritten below, and the session it left is recomputed too.
		var was string
		if err := tx.QueryRowContext(ctx, "SELECT session_id FROM files WHERE id = ?", f.ID).Scan(&was); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE files SET session_id = ?, kind = ?,
			project_key = CASE WHEN ? <> '' THEN ? ELSE project_key END,
			agent_id = CASE WHEN ? <> '' THEN ? ELSE agent_id END WHERE id = ?`,
			f.SessionID, f.Kind, f.ProjectKey, f.ProjectKey, f.AgentID, f.AgentID, f.ID); err != nil {
			return err
		}
		if was != f.SessionID {
			if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO dirty(account, session_id) VALUES(?, ?)", f.Account, was); err != nil {
				return err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO dirty(account, session_id) VALUES(?, ?)", f.Account, f.SessionID); err != nil {
		return err
	}
	var newer bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM transfers WHERE path = ? AND id > ?)", f.Path, u.Ledger).Scan(&newer); err != nil {
		return err
	}
	if newer {
		f.Parser = 0
	}
	if u.Reset {
		for _, q := range []string{
			"DELETE FROM responses WHERE file_id = ?",
			"DELETE FROM activity WHERE file_id = ?",
			"DELETE FROM tools WHERE file_id = ?",
			"DELETE FROM links WHERE file_id = ?",
			"DELETE FROM processes WHERE file_id = ?",
			"DELETE FROM spawns WHERE file_id = ?",
			"DELETE FROM turns WHERE file_id = ?",
		} {
			if _, err := tx.ExecContext(ctx, q, f.ID); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE files SET lines=0, skipped=0, typed_prompts=0, other_prompts=0, tool_calls=0, active_ms=0,
			first_ts=0, last_ts=0, cwd='', custom_title='', ai_title='', summary='', first_prompt='', team_name='', agent_name='',
			bridge_session_id='', entrypoint='', version='', git_branch='', cost_state_usd=0, has_cost_state=0, attribution='' WHERE id = ?`, f.ID); err != nil {
			return err
		}
	}
	r := &u.Result
	// A file's creation time only moves earlier: a copy written over it later
	// (a fast-forward replaces the file) must not make it look newer than the
	// files it was copied to. Its responses carry the time kept here.
	if err := tx.QueryRowContext(ctx, `UPDATE files SET born = CASE WHEN ? > 0 AND (born = 0 OR ? < born) THEN ? ELSE born END
		WHERE id = ? RETURNING born`, f.Born, f.Born, f.Born, f.ID).Scan(&f.Born); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE files SET turn = ?, replay = ?, carry = ?, attribution = CASE WHEN attribution = '' THEN ? ELSE attribution END WHERE id = ?`,
		r.State.Turn, boolInt(r.State.Replay), r.State.Carry, r.Attribution, f.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE files SET size=?, mtime=?, offset=?, head_hash=?, parser=?, prev_ts=?, missing=0,
		lines = lines + ?, skipped = skipped + ?, typed_prompts = typed_prompts + ?, other_prompts = other_prompts + ?,
		tool_calls = tool_calls + ?, active_ms = active_ms + ?,
		first_ts = CASE WHEN first_ts = 0 OR (? > 0 AND ? < first_ts) THEN ? ELSE first_ts END,
		last_ts = MAX(last_ts, ?),
		cwd = CASE WHEN cwd = '' THEN ? ELSE cwd END,
		custom_title = CASE WHEN ? <> '' THEN ? ELSE custom_title END,
		ai_title = CASE WHEN ? <> '' THEN ? ELSE ai_title END,
		summary = CASE WHEN ? <> '' THEN ? ELSE summary END,
		first_prompt = CASE WHEN first_prompt = '' THEN ? ELSE first_prompt END,
		team_name = CASE WHEN ? <> '' THEN ? ELSE team_name END,
		agent_name = CASE WHEN ? <> '' THEN ? ELSE agent_name END,
		agent_id = CASE WHEN agent_id = '' THEN ? ELSE agent_id END,
		bridge_session_id = CASE WHEN ? <> '' THEN ? ELSE bridge_session_id END,
		entrypoint = CASE WHEN ? <> '' THEN ? ELSE entrypoint END,
		version = CASE WHEN ? <> '' THEN ? ELSE version END,
		git_branch = CASE WHEN ? <> '' THEN ? ELSE git_branch END,
		cost_state_usd = CASE WHEN ? THEN ? ELSE cost_state_usd END,
		has_cost_state = MAX(has_cost_state, ?)
		WHERE id = ?`,
		f.Size, f.MTime, f.Offset, f.HeadHash, f.Parser, r.State.PrevTS,
		r.Lines, r.Skipped, r.TypedPrompts, r.OtherPrompts, r.ToolCalls, r.ActiveMS,
		r.FirstTS, r.FirstTS, r.FirstTS, r.LastTS, r.Cwd,
		r.CustomTitle, r.CustomTitle, r.AITitle, r.AITitle, r.Summary, r.Summary, r.FirstPrompt,
		r.TeamName, r.TeamName, r.AgentName, r.AgentName, r.AgentID,
		r.BridgeSessionID, r.BridgeSessionID, r.Entrypoint, r.Entrypoint, r.Version, r.Version, r.GitBranch, r.GitBranch,
		r.HasCost, r.CostUSD, boolInt(r.HasCost), f.ID); err != nil {
		return err
	}
	if a := u.Agent; a != nil {
		if _, err := tx.ExecContext(ctx, `UPDATE files SET agent_type=?, description=?, tool_use_id=?, spawn_depth=?, request_shape=?, skill=?, parent_agent=?, phase=? WHERE id = ?`,
			a.AgentType, a.Description, a.ToolUseID, a.SpawnDepth, a.RequestShape, a.Skill, a.ParentAgent, a.Phase, f.ID); err != nil {
			return err
		}
	}
	for _, resp := range r.Responses {
		var c sql.NullFloat64
		if resp.HasCost {
			c = sql.NullFloat64{Float64: resp.Cost, Valid: true}
		} else if v, ok := cost(resp.Model, resp.Speed, resp.Usage); ok {
			c = sql.NullFloat64{Float64: v, Valid: true}
		}
		us := resp.Usage
		// A response in several files (a copy or fork) belongs to the file
		// that made it: a line ccli copied in from another account never owns
		// it, and among the rest the file created first does. A later line in
		// the owning file may revise its usage.
		if _, err := tx.ExecContext(ctx, `INSERT INTO responses(key, file_id, account, session_id, kind, ts, model, speed, input, output, cache_read, cache_5m, cache_1h, web_search, cost, fixed_cost, born, turn, imported)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(key) DO UPDATE SET file_id=excluded.file_id, account=excluded.account, session_id=excluded.session_id, kind=excluded.kind,
			  born=excluded.born, model=excluded.model, speed=excluded.speed, input=excluded.input, output=excluded.output,
			  cache_read=excluded.cache_read, cache_5m=excluded.cache_5m, cache_1h=excluded.cache_1h, web_search=excluded.web_search, cost=excluded.cost, fixed_cost=excluded.fixed_cost,
			  turn=CASE WHEN ? THEN responses.turn ELSE excluded.turn END, imported=excluded.imported
			WHERE responses.file_id = excluded.file_id OR excluded.imported < responses.imported
			  OR (excluded.imported = responses.imported AND excluded.born > 0 AND (responses.born = 0 OR excluded.born < responses.born))`,
			resp.Key, f.ID, f.Account, f.SessionID, f.Kind, resp.TS, resp.Model, resp.Speed,
			us.Input, us.Output, us.CacheRead, us.Cache5m, us.Cache1h, us.WebSearch, c, boolInt(resp.HasCost), f.Born, resp.Turn, boolInt(resp.Imported), resp.Replay); err != nil {
			return err
		}
	}
	if r.FillModel != "" {
		if err := fillModel(ctx, tx, f.ID, r.FillModel, cost); err != nil {
			return err
		}
	}
	if u.Reset {
		// This file now holds, as a copy, responses no other file holds: their
		// own file lost them to this one before the copy was recorded and has
		// not been read since. Read the copy's source again so it takes them
		// back (a copied response never wins against its own file).
		if _, err := tx.ExecContext(ctx, `UPDATE files SET parser = 0 WHERE id <> ? AND id IN (
				SELECT src.id FROM transfers t JOIN files src ON src.account = t.from_account AND src.session_id = t.session_id AND src.agent_id = ?
				WHERE t.path = ?)
			AND EXISTS(SELECT 1 FROM responses WHERE file_id = ? AND imported = 1)`, f.ID, f.AgentID, f.Path, f.ID); err != nil {
			return err
		}
	}
	for _, t := range r.Turns {
		if _, err := tx.ExecContext(ctx, `INSERT INTO turns(file_id, seq, offset, ts, last_ts, kind, text, chars, images, prompt_id) VALUES(?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(file_id, seq) DO UPDATE SET offset=excluded.offset, ts=excluded.ts, last_ts=MAX(last_ts, excluded.last_ts),
			  kind=excluded.kind, text=excluded.text, chars=excluded.chars, images=excluded.images, prompt_id=excluded.prompt_id`,
			f.ID, t.Seq, t.Offset, t.TS, t.TS, t.Kind, t.Text, t.Chars, t.Images, t.PromptID); err != nil {
			return err
		}
	}
	for _, seq := range sortedTurns(r.TurnStats) {
		s := r.TurnStats[seq]
		if _, err := tx.ExecContext(ctx, `INSERT INTO turns(file_id, seq, last_ts, tool_calls, agents, duration_ms, active_ms) VALUES(?,?,?,?,?,?,?)
			ON CONFLICT(file_id, seq) DO UPDATE SET last_ts=MAX(last_ts, excluded.last_ts), tool_calls=tool_calls + excluded.tool_calls,
			  agents=agents + excluded.agents, duration_ms=MAX(duration_ms, excluded.duration_ms), active_ms=active_ms + excluded.active_ms`,
			f.ID, seq, s.LastTS, s.ToolCalls, s.Agents, s.DurationMS, s.ActiveMS); err != nil {
			return err
		}
	}
	for hour, ms := range r.Activity {
		if _, err := tx.ExecContext(ctx, `INSERT INTO activity(file_id, hour, ms) VALUES(?,?,?) ON CONFLICT(file_id, hour) DO UPDATE SET ms = ms + excluded.ms`, f.ID, hour, ms); err != nil {
			return err
		}
	}
	for _, kv := range sortedCounts(r.Tools) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO tools(file_id, name, skill, n) VALUES(?,?,'',?) ON CONFLICT(file_id, name, skill) DO UPDATE SET n = n + excluded.n`, f.ID, kv.k, kv.n); err != nil {
			return err
		}
	}
	for _, kv := range sortedCounts(r.Skills) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO tools(file_id, name, skill, n) VALUES(?, 'Skill', ?, ?) ON CONFLICT(file_id, name, skill) DO UPDATE SET n = n + excluded.n`, f.ID, kv.k, kv.n); err != nil {
			return err
		}
	}
	for _, kv := range sortedCounts(r.Commands) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO tools(file_id, name, skill, n) VALUES(?, 'Command', ?, ?) ON CONFLICT(file_id, name, skill) DO UPDATE SET n = n + excluded.n`, f.ID, kv.k, kv.n); err != nil {
			return err
		}
	}
	for _, t := range r.ContinuedIn {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO links(file_id, kind, target) VALUES(?, 'continued-in', ?)`, f.ID, t); err != nil {
			return err
		}
	}
	for _, sp := range r.Spawns {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO spawns(file_id, tool_use_id, turn, tool) VALUES(?, ?, ?, ?)`, f.ID, sp.ToolUseID, sp.Turn, sp.Tool); err != nil {
			return err
		}
	}
	for _, id := range sortedKeys(r.Targets) {
		if _, err := tx.ExecContext(ctx, `UPDATE spawns SET target = ? WHERE file_id = ? AND tool_use_id = ?`, r.Targets[id], f.ID, id); err != nil {
			return err
		}
	}
	for _, p := range r.Processes {
		// A newer snapshot of the same process replaces the older one.
		if _, err := tx.ExecContext(ctx, "DELETE FROM processes WHERE file_id = ? AND start_ms = ?", f.ID, p.Start); err != nil {
			return err
		}
		for _, m := range p.Models {
			if _, err := tx.ExecContext(ctx, `INSERT INTO processes(file_id, start_ms, end_ms, model, input, output, cache_read, cache_write, web_search, cost, imported)
				VALUES(?,?,?,?,?,?,?,?,?,?,?)`, f.ID, p.Start, p.End, m.Model, m.Input, m.Output, m.CacheRead, m.CacheWrite, m.WebSearch, m.CostUSD, boolInt(p.Imported)); err != nil {
				return err
			}
		}
	}
	return nil
}

func sortedTurns(m map[int]*transcript.TurnStat) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

type kv struct {
	k string
	n int
}

func sortedCounts(m map[string]int) []kv {
	out := make([]kv, 0, len(m))
	for k, n := range m {
		out = append(out, kv{k, n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].k < out[j].k })
	return out
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// fillModel gives a file's responses stored with no model the one a later
// line named, and prices them.
func fillModel(ctx context.Context, tx *sql.Tx, file int64, model string, cost CostFunc) error {
	rows, err := tx.QueryContext(ctx, `SELECT key, speed, input, output, cache_read, cache_5m, cache_1h, web_search FROM responses
		WHERE file_id = ? AND model = '' AND kind <> 'adjustment'`, file)
	if err != nil {
		return err
	}
	type upd struct {
		key string
		c   sql.NullFloat64
	}
	var ups []upd
	for rows.Next() {
		var key, speed string
		var u transcript.Usage
		if err := rows.Scan(&key, &speed, &u.Input, &u.Output, &u.CacheRead, &u.Cache5m, &u.Cache1h, &u.WebSearch); err != nil {
			rows.Close()
			return err
		}
		var c sql.NullFloat64
		if v, ok := cost(model, speed, u); ok {
			c = sql.NullFloat64{Float64: v, Valid: true}
		}
		ups = append(ups, upd{key, c})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, u := range ups {
		if _, err := tx.ExecContext(ctx, "UPDATE responses SET model = ?, cost = ? WHERE key = ?", model, u.c, u.key); err != nil {
			return err
		}
	}
	return nil
}

// MarkMissing flags files that are gone from disk (or back again).
func (s *Store) MarkMissing(ctx context.Context, ids []int64, missing bool) error {
	if len(ids) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, "UPDATE files SET missing = ? WHERE id = ?", boolInt(missing), id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// MovePath gives a file's row the path its tool moved the file to (Codex
// moves a thread it archives), so what the index read of it stays with it.
func (s *Store) MovePath(ctx context.Context, id int64, path string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE files SET path = ? WHERE id = ?", path, id)
	return err
}

// Run is a workflow run: counts from its journal, and what its state file
// records (name, status, summary, start, duration, agent count).
type Run struct {
	Account, SessionID, RunID          string
	Launched, Started, Results, Failed int
	JournalMTime                       int64 // mtime of the journal the counts came from; 0 = none read
	Name, Status, Summary              string
	StartMS, DurationMS                int64
	AgentCount                         int
	StateMTime                         int64 // mtime of the state file; 0 = none read
}

// Runs returns the stored runs by account, session and run id.
func (s *Store) Runs() (map[[3]string]Run, error) {
	rows, err := s.db.Query(`SELECT account, session_id, run_id, launched, started, results, failed, journal_mtime,
		name, status, summary, start_ms, duration_ms, agent_count, state_mtime FROM runs`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[[3]string]Run{}
	for rows.Next() {
		var r Run
		if err := rows.Scan(&r.Account, &r.SessionID, &r.RunID, &r.Launched, &r.Started, &r.Results, &r.Failed, &r.JournalMTime,
			&r.Name, &r.Status, &r.Summary, &r.StartMS, &r.DurationMS, &r.AgentCount, &r.StateMTime); err != nil {
			return nil, err
		}
		out[[3]string{r.Account, r.SessionID, r.RunID}] = r
	}
	return out, rows.Err()
}

// PutRuns writes workflow runs. A run's journal counts and state fields are
// each written only when that file was read (its mtime is set).
func (s *Store) PutRuns(ctx context.Context, runs []Run) error {
	if len(runs) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, r := range runs {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO runs(account, session_id, run_id) VALUES(?,?,?)`, r.Account, r.SessionID, r.RunID); err != nil {
			return err
		}
		if r.JournalMTime != 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE runs SET launched=?, started=?, results=?, failed=?, journal_mtime=? WHERE account=? AND session_id=? AND run_id=?`,
				r.Launched, r.Started, r.Results, r.Failed, r.JournalMTime, r.Account, r.SessionID, r.RunID); err != nil {
				return err
			}
		}
		if r.StateMTime != 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE runs SET name=?, status=?, summary=?, start_ms=?, duration_ms=?, agent_count=?, state_mtime=? WHERE account=? AND session_id=? AND run_id=?`,
				r.Name, r.Status, r.Summary, r.StartMS, r.DurationMS, r.AgentCount, r.StateMTime, r.Account, r.SessionID, r.RunID); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// Transfer is one file ccli wrote into an account by copying it from
// another: bytes [From, To) of Path came from the other account.
type Transfer struct {
	At           time.Time `json:"at"`
	Mode         string    `json:"mode"` // copy, move, fork
	SessionID    string    `json:"session_id"`
	NewSessionID string    `json:"new_session_id,omitempty"` // fork only
	FromAccount  string    `json:"from"`
	ToAccount    string    `json:"to"`
	Path         string    `json:"path"`
	From         int64     `json:"from_offset"`
	To           int64     `json:"to_offset"`
}

// RecordTransfers adds entries to the ledger. Usage in those byte ranges
// stays with the account it came from.
func (s *Store) RecordTransfers(ctx context.Context, ts []Transfer) error {
	if len(ts) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, t := range ts {
		if _, err := recordTransfer(ctx, tx, t); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// RecordTransfer adds one entry to the ledger and returns its id (0 when the
// range is empty), for DeleteTransfer to take back if the write it records
// does not happen.
func (s *Store) RecordTransfer(ctx context.Context, t Transfer) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	id, err := recordTransfer(ctx, tx, t)
	if err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// DeleteTransfer removes a ledger entry whose bytes were never written.
func (s *Store) DeleteTransfer(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM transfers WHERE id = ?`, id)
	return err
}

func recordTransfer(ctx context.Context, tx *sql.Tx, t Transfer) (int64, error) {
	if t.To <= t.From {
		return 0, nil
	}
	var id int64
	if err := tx.QueryRowContext(ctx, `INSERT INTO transfers(at, mode, session_id, new_session_id, from_account, to_account, path, from_offset, to_offset)
		VALUES(?,?,?,?,?,?,?,?,?) RETURNING id`, t.At.UnixMilli(), t.Mode, t.SessionID, t.NewSessionID, t.FromAccount, t.ToAccount, t.Path, t.From, t.To).Scan(&id); err != nil {
		return 0, err
	}
	// If an index run read these bytes before they were recorded, read the
	// file again from the start so they count for the right account. A run
	// still in flight is caught by Update.Ledger instead.
	if _, err := tx.ExecContext(ctx, `UPDATE files SET parser = 0 WHERE path = ? AND offset > ?`, t.Path, t.From); err != nil {
		return 0, err
	}
	return id, nil
}

// Imported returns, per file path, the byte ranges ccli copied in, and the
// newest ledger id they include (see Update.Ledger).
func (s *Store) Imported() (map[string][]transcript.Range, int64, error) {
	rows, err := s.db.Query(`SELECT id, path, from_offset, to_offset FROM transfers ORDER BY path, from_offset`)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := map[string][]transcript.Range{}
	var last int64
	for rows.Next() {
		var id int64
		var p string
		var r transcript.Range
		if err := rows.Scan(&id, &p, &r.From, &r.To); err != nil {
			return nil, 0, err
		}
		out[p] = append(out[p], r)
		last = max(last, id)
	}
	return out, last, rows.Err()
}

// Transfers returns the ledger entries that involve a session, oldest first.
func (s *Store) Transfers(ctx context.Context, sessionID string) ([]Transfer, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT at, mode, session_id, new_session_id, from_account, to_account, path, from_offset, to_offset
		FROM transfers WHERE session_id = ? OR new_session_id = ? ORDER BY at, id`, sessionID, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Transfer
	for rows.Next() {
		var t Transfer
		var at int64
		if err := rows.Scan(&at, &t.Mode, &t.SessionID, &t.NewSessionID, &t.FromAccount, &t.ToAccount, &t.Path, &t.From, &t.To); err != nil {
			return nil, err
		}
		t.At = time.UnixMilli(at)
		out = append(out, t)
	}
	return out, rows.Err()
}

// Reprice recomputes every stored cost with a new price table, records the
// table's hash as meta "prices", and marks every session for reconciliation.
func (s *Store) Reprice(ctx context.Context, cost CostFunc, hash string) error {
	rows, err := s.db.QueryContext(ctx, `SELECT key, model, speed, input, output, cache_read, cache_5m, cache_1h, web_search FROM responses WHERE kind <> 'adjustment' AND fixed_cost = 0`)
	if err != nil {
		return err
	}
	type upd struct {
		key string
		c   sql.NullFloat64
	}
	var ups []upd
	for rows.Next() {
		var key, model, speed string
		var u transcript.Usage
		if err := rows.Scan(&key, &model, &speed, &u.Input, &u.Output, &u.CacheRead, &u.Cache5m, &u.Cache1h, &u.WebSearch); err != nil {
			rows.Close()
			return err
		}
		var c sql.NullFloat64
		if v, ok := cost(model, speed, u); ok {
			c = sql.NullFloat64{Float64: v, Valid: true}
		}
		ups = append(ups, upd{key, c})
	}
	rows.Close()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, "UPDATE responses SET cost = ? WHERE key = ?")
	if err != nil {
		return err
	}
	for _, u := range ups {
		if _, err := stmt.ExecContext(ctx, u.c, u.key); err != nil {
			return err
		}
	}
	// Every session's reconciliation depends on its transcript cost; record the
	// new table only together with the marks that redo them.
	if err := markAllDirty(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO meta(key, value) VALUES('prices', ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", hash); err != nil {
		return err
	}
	return tx.Commit()
}

func markAllDirty(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO dirty(account, session_id)
		SELECT DISTINCT f.account, f.session_id FROM processes p JOIN files f ON f.id = p.file_id`)
	return err
}

func openDB(path string) (*Store, error) {
	// Write transactions take the lock when they begin, so two ccli runs
	// cannot interleave a read of a file's offset with a write of it.
	db, err := sql.Open("sqlite", "file:"+path+"?_txlock=immediate&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return &Store{db: db}, nil
}

// PromptIDs returns the promptIds that started turns in a file, which an
// incremental parse needs to recognise a replayed stretch.
func (s *Store) PromptIDs(fileID int64) (map[string]bool, error) {
	rows, err := s.db.Query("SELECT prompt_id FROM turns WHERE file_id = ? AND prompt_id <> ''", fileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}
