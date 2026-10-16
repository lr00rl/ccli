// Package query answers questions about the index. Every ccli view calls a
// Service; the terminal renderer formats its results, and a web server can
// serve the same calls as JSON.
package query

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lr00rl/ccli/internal/account"
	"github.com/lr00rl/ccli/internal/store"
)

// Service is the read interface shared by the terminal and a future web UI.
type Service interface {
	Accounts(ctx context.Context, f Filter) ([]AccountSummary, error)
	Sessions(ctx context.Context, f Filter) (SessionPage, error)
	Session(ctx context.Context, ref string) (*SessionDetail, error)
	Overview(ctx context.Context, f Filter) (*Overview, error)
}

// Filter narrows a query. Zero values mean "no limit".
type Filter struct {
	Accounts []string       `json:"accounts,omitempty"`
	Since    time.Time      `json:"since,omitempty"`
	Until    time.Time      `json:"until,omitempty"`
	Project  string         `json:"project,omitempty"`
	Model    string         `json:"model,omitempty"`
	Search   string         `json:"search,omitempty"`
	Sort     string         `json:"sort,omitempty"` // last, start, cost, tokens, active
	Limit    int            `json:"limit,omitempty"`
	Show     Visibility     `json:"show"` // lists only: hidden sessions are left out unless asked for
	Loc      *time.Location `json:"-"`
}

func (f Filter) loc() *time.Location {
	if f.Loc != nil {
		return f.Loc
	}
	return time.Local
}

func (f Filter) wantsAccount(a string) bool {
	if len(f.Accounts) == 0 {
		return true
	}
	for _, x := range f.Accounts {
		if x == a {
			return true
		}
	}
	return false
}

func (f Filter) sinceMS() int64 {
	if f.Since.IsZero() {
		return 0
	}
	return f.Since.UnixMilli()
}

func (f Filter) untilMS() int64 {
	if f.Until.IsZero() {
		return 1<<62 - 1
	}
	return f.Until.UnixMilli()
}

// Tokens counts tokens by kind.
type Tokens struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
	WebSearch  int64 `json:"web_search,omitempty"`
}

// InOut is input plus output, the "tokens" figure shown in tables.
func (t Tokens) InOut() int64 { return t.Input + t.Output }

// All is every token kind.
func (t Tokens) All() int64 { return t.Input + t.Output + t.CacheRead + t.CacheWrite }

func (t *Tokens) add(o Tokens) {
	t.Input += o.Input
	t.Output += o.Output
	t.CacheRead += o.CacheRead
	t.CacheWrite += o.CacheWrite
	t.WebSearch += o.WebSearch
}

// Cost is a list-price estimate in USD.
type Cost struct {
	USD      float64 `json:"usd"`
	Unlogged float64 `json:"unlogged_usd"` // part from Claude Code's own totals beyond the transcript
	Unpriced int64   `json:"unpriced"`     // responses of models without a price, left out
}

func (c *Cost) add(o Cost) {
	c.USD += o.USD
	c.Unlogged += o.Unlogged
	c.Unpriced += o.Unpriced
}

// AccountSummary describes one account.
type AccountSummary struct {
	account.Account
	Sessions        int       `json:"sessions"`
	SessionsInRange int       `json:"sessions_in_range"`
	Transcripts     int       `json:"transcripts_on_disk"`
	LastActive      time.Time `json:"last_active,omitempty"`
	Tokens          Tokens    `json:"tokens"`
	Cost            Cost      `json:"cost"`
	Live            int       `json:"live"`
}

// SessionRow is one session in a list.
type SessionRow struct {
	Account      string    `json:"account"`
	ID           string    `json:"id"`
	Title        string    `json:"title"`
	TitleSource  string    `json:"title_source"` // custom, ai, summary, prompt, none
	Cwd          string    `json:"cwd"`
	Project      string    `json:"project"`
	ProjectKey   string    `json:"project_key"`
	Started      time.Time `json:"started"`
	Last         time.Time `json:"last"`
	ActiveMS     int64     `json:"active_ms"`
	Prompts      int       `json:"prompts"`
	OtherPrompts int       `json:"other_prompts"`
	Replies      int       `json:"replies"`
	ToolCalls    int       `json:"tool_calls"`
	Models       []string  `json:"models"`
	Tokens       Tokens    `json:"tokens"`
	Cost         Cost      `json:"cost"`
	Subagents    int       `json:"subagents"`
	Runs         int       `json:"workflow_runs"`
	Team         string    `json:"team,omitempty"`
	AgentName    string    `json:"agent_name,omitempty"`
	Live         bool      `json:"live"`
	Hidden       bool      `json:"hidden,omitempty"` // set aside with `ccli hide`; counted in totals, left out of lists
	Missing      bool      `json:"transcript_missing"`
	GitBranch    string    `json:"git_branch,omitempty"`
	Entrypoint   string    `json:"entrypoint,omitempty"`
	Version      string    `json:"version,omitempty"`
}

// SessionPage is a filtered, sorted list of sessions.
type SessionPage struct {
	Filter   Filter       `json:"filter"`
	Sessions []SessionRow `json:"sessions"`
	Matched  int          `json:"matched"`
	Hidden   int          `json:"hidden"` // hidden sessions in range that were left out
	Tokens   Tokens       `json:"tokens"`
	Cost     Cost         `json:"cost"`
}

// DB is the Service backed by the index.
type DB struct {
	st    *store.Store
	accts []account.Account
	live  map[string]account.Live // session id -> live process

	cacheMu  sync.Mutex
	cache    map[string]*SessionDetail // by ref; nil unless CacheSessions was called
	cacheGen uint64                    // bumped when cached details go stale
}

// CacheSessions makes Session keep what it loads for the life of the DB, so
// a browser that reads one session many times builds its graph once. Call it
// before the DB is shared; details are read-only once built.
func (d *DB) CacheSessions() { d.cache = map[string]*SessionDetail{} }

// New builds a Service over the store.
func New(st *store.Store, accts []account.Account) *DB {
	live := map[string]account.Live{}
	for _, l := range account.LiveSessions(accts) {
		live[l.SessionID] = l
	}
	return &DB{st: st, accts: accts, live: live}
}

func (d *DB) sql() *sql.DB { return d.st.DB() }

// Accounts known to the service.
func (d *DB) AccountList() []account.Account { return d.accts }

// IsLive reports whether a process is running the session.
func (d *DB) IsLive(id string) (account.Live, bool) {
	l, ok := d.live[id]
	return l, ok
}

func ms(t int64) time.Time {
	if t == 0 {
		return time.Time{}
	}
	return time.UnixMilli(t)
}

func projectName(cwd, key string) string {
	if cwd != "" {
		return filepath.Base(cwd)
	}
	parts := strings.Split(strings.Trim(key, "-"), "-")
	return parts[len(parts)-1]
}

func title(custom, ai, summary, prompt string) (string, string) {
	switch {
	case custom != "":
		return custom, "custom"
	case ai != "":
		return ai, "ai"
	case summary != "":
		return summary, "summary"
	case prompt != "":
		return prompt, "prompt"
	}
	return "", "none"
}

// ShortID is the first 8 characters of a session id, or all of a shorter one.
func ShortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
