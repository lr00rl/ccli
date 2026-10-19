package query

import (
	"context"
	"time"

	"github.com/lr00rl/ccli/internal/account"
	"github.com/lr00rl/ccli/internal/cursorusage"
	"github.com/lr00rl/ccli/internal/store"
)

// applyCursorUsage adds dashboard usage, joined on conversation id, to Cursor
// CLI and IDE rows. A subagent's events are counted on the session that
// started it. The figures are the whole conversation: the range filter
// selects which sessions appear, the same way transcript totals do.
// An IDE row that already has bubble tokens keeps them and only takes the
// billed amount. A CLI row takes tokens and cost, because its transcript has
// neither.
func (d *DB) applyCursorUsage(ctx context.Context, rows []SessionRow) {
	led := d.cursorLedger()
	if led == nil || len(led.Sessions) == 0 {
		return
	}
	kids := d.cursorChildren(ctx)
	for i := range rows {
		if rows[i].Tool != account.Cursor && rows[i].Tool != account.CursorIDE {
			continue
		}
		u := usageOf(led, rows[i].ID, kids[rows[i].ID], time.Time{}, time.Time{})
		if u.Events == 0 {
			continue
		}
		rows[i].Tokens, rows[i].Cost = mergeBilled(rows[i].Tool, rows[i].Tokens, rows[i].Cost, u)
		if m := u.Primary(); m != "" && !hasModel(rows[i].Models, m) {
			rows[i].Models = append([]string{modelName(m)}, rows[i].Models...)
		}
	}
}

// mergeBilled folds dashboard usage into a row. IDE tokens that the index
// already stored are left in place.
func mergeBilled(tool account.Tool, tokens Tokens, cost Cost, u cursorusage.Use) (Tokens, Cost) {
	if u.Events == 0 {
		return tokens, cost
	}
	if tool == account.CursorIDE && tokens.All() > 0 {
		if cost.USD == 0 {
			cost.USD = u.USD
			cost.Unpriced = 0
		}
		return tokens, cost
	}
	tokens.Input += u.Input
	tokens.Output += u.Output
	tokens.CacheRead += u.CacheRead
	tokens.CacheWrite += u.CacheWrite
	cost.USD += u.USD
	cost.Unpriced = 0
	return tokens, cost
}

func (d *DB) applyCursorDetail(ctx context.Context, det *SessionDetail) {
	led := d.cursorLedger()
	if led == nil || (det.Tool != account.Cursor && det.Tool != account.CursorIDE) {
		return
	}
	kids := d.cursorChildren(ctx)
	u := usageOf(led, det.ID, kids[det.ID], time.Time{}, time.Time{})
	if u.Events == 0 {
		return
	}
	by := map[string]int{}
	for i := range det.ByModel {
		by[det.ByModel[i].Name] = i
	}
	for raw, m := range u.Models {
		name := modelName(raw)
		if i, ok := by[name]; ok {
			row := &det.ByModel[i]
			if det.Tool == account.CursorIDE && row.Tokens.All() > 0 {
				if row.Cost == 0 {
					row.Cost = m.USD
				}
				continue
			}
			row.Tokens.Input += m.Input
			row.Tokens.Output += m.Output
			row.Tokens.CacheRead += m.CacheRead
			row.Tokens.CacheWrite += m.CacheWrite
			row.Cost += m.USD
			row.Sessions += m.N
			continue
		}
		det.ByModel = append(det.ByModel, Share{
			Name:     name,
			Sessions: m.N,
			Tokens:   Tokens{Input: m.Input, Output: m.Output, CacheRead: m.CacheRead, CacheWrite: m.CacheWrite},
			Cost:     m.USD,
		})
	}
	for i := range det.Files {
		id := det.ID
		if det.Files[i].Kind == "subagent" && det.Files[i].AgentID != "" {
			id = det.Files[i].AgentID
		} else if det.Files[i].Kind != "main" {
			continue
		}
		one := led.Sessions[id]
		if one == nil || one.Events == 0 {
			continue
		}
		det.Files[i].Tokens, det.Files[i].Cost = mergeFile(det.Tool, det.Files[i].Tokens, det.Files[i].Cost, *one)
	}
}

func mergeFile(tool account.Tool, tokens Tokens, cost float64, u cursorusage.Use) (Tokens, float64) {
	tokens, c := mergeBilled(tool, tokens, Cost{USD: cost}, u)
	return tokens, c.USD
}

func usageOf(led *cursorusage.Ledger, id string, children []string, since, until time.Time) cursorusage.Use {
	var u cursorusage.Use
	whole := since.IsZero() && until.IsZero()
	add := func(id string) {
		src := led.Sessions[id]
		if src == nil {
			return
		}
		if whole {
			u.Add(src)
			return
		}
		part := src.Between(since, until)
		u.Add(&part)
	}
	add(id)
	seen := map[string]bool{id: true}
	for _, child := range children {
		if seen[child] {
			continue
		}
		seen[child] = true
		add(child)
	}
	return u
}

func (d *DB) cursorLedger() *cursorusage.Ledger {
	d.useOnce.Do(func() { d.use = cursorusage.Load(store.DataDir()) })
	if d.use == nil {
		return &cursorusage.Ledger{}
	}
	return d.use
}

func (d *DB) cursorChildren(ctx context.Context) map[string][]string {
	d.kidOnce.Do(func() {
		d.kids = map[string][]string{}
		rows, err := d.sql().QueryContext(ctx, `SELECT session_id, agent_id FROM files WHERE kind = 'subagent' AND agent_id <> ''`)
		if err != nil {
			return
		}
		defer rows.Close()
		for rows.Next() {
			var session, agent string
			if rows.Scan(&session, &agent) != nil || agent == session {
				continue
			}
			d.kids[session] = append(d.kids[session], agent)
		}
	})
	return d.kids
}

// addCursorAccountUsage adds billed Cursor usage onto account totals, and
// only the events inside the filter. IDE token rows are already in the SQL
// sum, so an IDE account takes the billed amount and not a second copy of
// the tokens. CLI transcripts have no token rows, so the events are the
// whole CLI figure.
func (d *DB) addCursorAccountUsage(ctx context.Context, f Filter, out []AccountSummary, idx map[string]int) error {
	led := d.cursorLedger()
	if len(led.Sessions) == 0 {
		return nil
	}
	rows, err := d.loadSessions(ctx, f)
	if err != nil {
		return err
	}
	kids := d.cursorChildren(ctx)
	for _, s := range rows {
		i, ok := idx[s.Account]
		if !ok || (s.Tool != account.Cursor && s.Tool != account.CursorIDE) {
			continue
		}
		u := usageOf(led, s.ID, kids[s.ID], f.Since, f.Until)
		if u.Events == 0 {
			continue
		}
		if s.Tool == account.CursorIDE {
			out[i].Cost.USD += u.USD
			out[i].Cost.Unpriced = 0
			continue
		}
		out[i].Tokens.Input += u.Input
		out[i].Tokens.Output += u.Output
		out[i].Tokens.CacheRead += u.CacheRead
		out[i].Tokens.CacheWrite += u.CacheWrite
		out[i].Cost.USD += u.USD
		out[i].Cost.Unpriced = 0
	}
	return nil
}

// addCursorOverview adds billed Cursor usage to the dashboard. Each event
// lands on its own day. IDE bubble tokens are already in the SQL totals, so
// those sessions contribute cost only.
func (d *DB) addCursorOverview(ctx context.Context, sessions []SessionRow, o *Overview, days map[string]*Day, projects, models, accts map[string]*Share, projSess, modelSess, acctSess map[string]map[skey]bool, loc *time.Location) {
	led := d.cursorLedger()
	if len(led.Sessions) == 0 {
		return
	}
	kids := d.cursorChildren(ctx)
	since, until := o.Filter.Since, o.Filter.Until
	for i := range sessions {
		s := &sessions[i]
		if s.Tool != account.Cursor && s.Tool != account.CursorIDE {
			continue
		}
		k := skey{s.Account, s.ID}
		ide := s.Tool == account.CursorIDE
		for _, h := range hitsOf(led, s.ID, kids[s.ID], since, until) {
			when := time.UnixMilli(h.At).In(loc)
			t := Tokens{}
			if !ide {
				t = Tokens{Input: h.Input, Output: h.Output, CacheRead: h.CacheRead, CacheWrite: h.CacheWrite}
				o.Tokens.add(t)
			}
			o.Cost.USD += h.USD
			date := when.Format("2006-01-02")
			dd := days[date]
			if dd == nil {
				dd = &Day{Date: date}
				days[date] = dd
			}
			dd.Tokens.add(t)
			dd.Cost += h.USD
			addShare(projects, projSess, s.Project, k, t, h.USD)
			addShare(accts, acctSess, s.Account, k, t, h.USD)
			if h.Model != "" {
				addShare(models, modelSess, modelName(h.Model), k, t, h.USD)
			}
		}
	}
}

func hitsOf(led *cursorusage.Ledger, id string, children []string, since, until time.Time) []cursorusage.Hit {
	var out []cursorusage.Hit
	seen := map[string]bool{}
	add := func(id string) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		src := led.Sessions[id]
		if src == nil {
			return
		}
		for _, h := range src.Hits {
			if h.In(since, until) {
				out = append(out, h)
			}
		}
	}
	add(id)
	for _, child := range children {
		add(child)
	}
	return out
}

func hasModel(ms []string, m string) bool {
	for _, s := range ms {
		if s == m {
			return true
		}
	}
	return false
}
