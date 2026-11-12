package query

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/lr00rl/ccli/internal/account"
	"github.com/lr00rl/ccli/internal/cursorusage"
)

// A pi session that talks to Cursor through cursor-bridge has no price in its
// transcript: Cursor bills a subscription and publishes no per-token rate, so
// pi records the tokens and leaves the cost at zero (internal/pi reads that
// zero as no price). What Cursor charged is on its usage page, keyed by the
// conversation id the bridge's plugin made from the pi session id and the
// first user message pi sent.
//
// Two sources name those ids, and both are read:
//
//   - the index: internal/pi rebuilds the ids from the transcript and the
//     indexer stores them as links of kind cursor-conversation, so sessions
//     from before the bridge recorded anything join too;
//   - cursor-bridge's own record of what it sent, one JSON object per line,
//     in $CURSOR_BRIDGE_SESSIONS or $XDG_DATA_HOME/cursor-bridge/sessions.jsonl:
//
//	{"conversation":"<cursor id>","session":"<pi session id>","model":"…","at":<ms>}
//
// which covers a first message the rebuild cannot reproduce byte for byte.
//
// A pi subagent runs as a pi session of its own, so its conversations name
// its own session id; it counts on the parent's row through the subagent
// link (files.agent_id), as Cursor's own subagents do.

// piCursorSessions maps each pi session id to the Cursor conversations it ran.
func piCursorSessions() map[string][]string {
	path := os.Getenv("CURSOR_BRIDGE_SESSIONS")
	if path == "off" {
		return nil
	}
	if path == "" {
		base := os.Getenv("XDG_DATA_HOME")
		if base == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil
			}
			base = filepath.Join(home, ".local", "share")
		}
		path = filepath.Join(base, "cursor-bridge", "sessions.jsonl")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	out := map[string][]string{}
	seen := map[string]bool{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
	for sc.Scan() {
		var p struct {
			Conversation string `json:"conversation"`
			Session      string `json:"session"`
		}
		// A line cut short by a crash, or one from a later format, is skipped.
		if json.Unmarshal(sc.Bytes(), &p) != nil || p.Conversation == "" || p.Session == "" || seen[p.Conversation] {
			continue
		}
		seen[p.Conversation] = true
		out[p.Session] = append(out[p.Session], p.Conversation)
	}
	return out
}

// piCursor is the join, loaded once per DB.
type piCursor struct {
	convs map[string][]string // pi session id -> Cursor conversation ids
	kids  map[string][]string // pi session id -> its subagent runs' session ids
}

func (d *DB) piCursorJoin(ctx context.Context) (*piCursor, error) {
	d.piMu.Lock()
	defer d.piMu.Unlock()
	if d.pi != nil {
		return d.pi, nil
	}
	j := &piCursor{convs: piCursorSessions(), kids: map[string][]string{}}
	if j.convs == nil {
		j.convs = map[string][]string{}
	}
	var names []any
	marks := ""
	for _, a := range d.accts {
		if a.Kind() == account.Pi {
			names = append(names, a.Name)
			marks += ",?"
		}
	}
	if len(names) > 0 {
		in := marks[1:]
		// The conversations the transcript names, as the session or subagent run
		// whose file it is: a subagent's are filed under its own id (agent_id).
		seen := map[string]bool{}
		for _, cs := range j.convs {
			for _, c := range cs {
				seen[c] = true
			}
		}
		rows, err := d.sql().QueryContext(ctx, `SELECT CASE WHEN f.kind = 'subagent' AND f.agent_id <> '' THEN f.agent_id ELSE f.session_id END, l.target
			FROM links l JOIN files f ON f.id = l.file_id
			WHERE l.kind = 'cursor-conversation' AND f.account IN (`+in+`)`, names...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var session, conv string
			if err := rows.Scan(&session, &conv); err != nil {
				rows.Close()
				return nil, err
			}
			if !seen[conv] {
				seen[conv] = true
				j.convs[session] = append(j.convs[session], conv)
			}
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
		if len(j.convs) > 0 {
			rows, err := d.sql().QueryContext(ctx, `SELECT session_id, agent_id FROM files
				WHERE kind = 'subagent' AND agent_id <> '' AND account IN (`+in+`)`, names...)
			if err != nil {
				return nil, err
			}
			for rows.Next() {
				var session, agent string
				if err := rows.Scan(&session, &agent); err != nil {
					rows.Close()
					return nil, err
				}
				if agent != session {
					j.kids[session] = append(j.kids[session], agent)
				}
			}
			if err := rows.Close(); err != nil {
				return nil, err
			}
		}
	}
	d.pi = j
	return j, nil
}

// conversations is every Cursor conversation a pi session ran, its own and
// its subagent runs'.
func (j *piCursor) conversations(id string) []string {
	if j == nil {
		return nil
	}
	out := append([]string(nil), j.convs[id]...)
	for _, kid := range j.kids[id] {
		out = append(out, j.convs[kid]...)
	}
	return out
}

// billed is what Cursor charged for a pi session's conversations, between
// since and until when either is set.
func (j *piCursor) billed(led *cursorusage.Ledger, id string, since, until time.Time) cursorusage.Use {
	var u cursorusage.Use
	if led == nil {
		return u
	}
	whole := since.IsZero() && until.IsZero()
	seen := map[string]bool{}
	for _, c := range j.conversations(id) {
		if seen[c] {
			continue
		}
		seen[c] = true
		src := led.Sessions[c]
		if src == nil {
			continue
		}
		if whole {
			u.Add(src)
			continue
		}
		part := src.Between(since, until)
		u.Add(&part)
	}
	return u
}

// addPiCursorAccountUsage adds what Cursor billed for pi sessions to their
// accounts' totals, only the events inside the filter, as
// addCursorAccountUsage does for Cursor's own accounts. The SQL totals left
// those replies unpriced; each covered session takes its Cursor replies off
// the unpriced count.
func (d *DB) addPiCursorAccountUsage(ctx context.Context, f Filter, out []AccountSummary, idx map[string]int) error {
	led := d.cursorLedger()
	if led == nil || len(led.Sessions) == 0 {
		return nil
	}
	j, err := d.piCursorJoin(ctx)
	if err != nil || len(j.convs) == 0 {
		return err
	}
	rows, err := d.loadSessions(ctx, f)
	if err != nil {
		return err
	}
	for _, s := range rows {
		if s.Tool != account.Pi || s.CursorBilled == 0 {
			continue
		}
		i, ok := idx[s.Account]
		if !ok {
			continue
		}
		u := j.billed(led, s.ID, f.Since, f.Until)
		if u.Events == 0 {
			continue
		}
		out[i].Cost.USD += u.USD
		out[i].Cost.Unpriced = max(0, out[i].Cost.Unpriced-s.cursorUnpriced)
	}
	return nil
}

// addPiCursorOverview adds what Cursor billed for pi sessions to the
// dashboard, each event on its own day, as addCursorOverview does. Tokens are
// already in the SQL sum; only the cost is added.
func (d *DB) addPiCursorOverview(ctx context.Context, sessions []SessionRow, o *Overview, days map[string]*Day, projects, models, accts map[string]*Share, projSess, modelSess, acctSess map[string]map[skey]bool, loc *time.Location) error {
	led := d.cursorLedger()
	if led == nil || len(led.Sessions) == 0 {
		return nil
	}
	j, err := d.piCursorJoin(ctx)
	if err != nil || len(j.convs) == 0 {
		return err
	}
	since, until := o.Filter.Since, o.Filter.Until
	for i := range sessions {
		s := &sessions[i]
		if s.Tool != account.Pi || s.CursorBilled == 0 {
			continue
		}
		k := skey{s.Account, s.ID}
		seen := map[string]bool{}
		for _, c := range j.conversations(s.ID) {
			src := led.Sessions[c]
			if src == nil || seen[c] {
				continue
			}
			seen[c] = true
			for _, h := range src.Hits {
				if !h.In(since, until) {
					continue
				}
				o.Cost.USD += h.USD
				date := time.UnixMilli(h.At).In(loc).Format("2006-01-02")
				dd := days[date]
				if dd == nil {
					dd = &Day{Date: date}
					days[date] = dd
				}
				dd.Cost += h.USD
				addShare(projects, projSess, s.Project, k, Tokens{}, h.USD)
				addShare(accts, acctSess, s.Account, k, Tokens{}, h.USD)
				if h.Model != "" {
					addShare(models, modelSess, modelName(h.Model), k, Tokens{}, h.USD)
				}
			}
		}
		o.Cost.Unpriced = max(0, o.Cost.Unpriced-s.cursorUnpriced)
	}
	return nil
}

// applyPiCursorUsage puts what Cursor billed on the pi rows whose Cursor
// replies carry no price. The billed amount replaces nothing: a row's priced
// replies (another provider in the same session) keep their cost, and the
// billed USD is added for the Cursor part, whose replies pi left unpriced.
// Tokens stay as pi counted them; Cursor's ledger counts the same calls.
// A row stays marked unpriced ("+") only for Cursor replies no ledger event
// covers yet, such as the last few minutes before the usage page updates.
func (d *DB) applyPiCursorUsage(ctx context.Context, rows []SessionRow, since, until time.Time) error {
	led := d.cursorLedger()
	if led == nil || len(led.Sessions) == 0 {
		return nil
	}
	var j *piCursor
	for i := range rows {
		if rows[i].Tool != account.Pi || rows[i].Cost.Unpriced == 0 {
			continue
		}
		if j == nil {
			var err error
			if j, err = d.piCursorJoin(ctx); err != nil {
				return err
			}
			if len(j.convs) == 0 {
				return nil
			}
		}
		u := j.billed(led, rows[i].ID, since, until)
		if u.Events == 0 {
			continue
		}
		rows[i].cursorUnpriced = rows[i].Cost.Unpriced
		rows[i].Cost.USD += u.USD
		rows[i].Cost.Unpriced = 0
		rows[i].CursorBilled = u.USD
	}
	return nil
}
