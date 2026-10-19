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
// started it. Rows that already had list-price cost keep it; the dashboard
// figure replaces the unpriced marker because it is the billed amount.
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
		u := usageOf(led, rows[i].ID, kids[rows[i].ID])
		if u.Events == 0 {
			continue
		}
		rows[i].Tokens.Input += u.Input
		rows[i].Tokens.Output += u.Output
		rows[i].Tokens.CacheRead += u.CacheRead
		rows[i].Tokens.CacheWrite += u.CacheWrite
		rows[i].Cost.USD += u.USD
		rows[i].Cost.Unpriced = 0
		if m := u.Primary(); m != "" && !hasModel(rows[i].Models, m) {
			rows[i].Models = append([]string{m}, rows[i].Models...)
		}
	}
}

func (d *DB) applyCursorDetail(ctx context.Context, det *SessionDetail) {
	led := d.cursorLedger()
	if led == nil || (det.Tool != account.Cursor && det.Tool != account.CursorIDE) {
		return
	}
	kids := d.cursorChildren(ctx)
	u := usageOf(led, det.ID, kids[det.ID])
	if u.Events == 0 {
		return
	}
	by := map[string]int{}
	for i := range det.ByModel {
		by[det.ByModel[i].Name] = i
	}
	for name, m := range u.Models {
		if i, ok := by[name]; ok {
			det.ByModel[i].Tokens.Input += m.Input
			det.ByModel[i].Tokens.Output += m.Output
			det.ByModel[i].Tokens.CacheRead += m.CacheRead
			det.ByModel[i].Tokens.CacheWrite += m.CacheWrite
			det.ByModel[i].Cost += m.USD
			det.ByModel[i].Sessions += m.N
			continue
		}
		det.ByModel = append(det.ByModel, Share{
			Name:     name,
			Sessions: m.N,
			Tokens:   Tokens{Input: m.Input, Output: m.Output, CacheRead: m.CacheRead, CacheWrite: m.CacheWrite},
			Cost:     m.USD,
		})
	}
}

func usageOf(led *cursorusage.Ledger, id string, children []string) cursorusage.Use {
	var u cursorusage.Use
	u.Add(led.Sessions[id])
	seen := map[string]bool{id: true}
	for _, child := range children {
		if seen[child] {
			continue
		}
		seen[child] = true
		u.Add(led.Sessions[child])
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

// addCursorAccountUsage adds billed Cursor usage onto account totals. The
// index itself has no token rows for these sessions, so this is the whole
// figure, not a second copy of what SQL already summed.
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
		u := usageOf(led, s.ID, kids[s.ID])
		if u.Events == 0 {
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

// addCursorOverview adds the same billed usage to the dashboard. Daily bars
// use the session's last activity, because a usage event has no local turn
// to attach to.
func (d *DB) addCursorOverview(ctx context.Context, sessions []SessionRow, o *Overview, days map[string]*Day, projects, models, accts map[string]*Share, projSess, modelSess, acctSess map[string]map[skey]bool, loc *time.Location) {
	led := d.cursorLedger()
	if len(led.Sessions) == 0 {
		return
	}
	kids := d.cursorChildren(ctx)
	for i := range sessions {
		s := &sessions[i]
		if s.Tool != account.Cursor && s.Tool != account.CursorIDE {
			continue
		}
		u := usageOf(led, s.ID, kids[s.ID])
		if u.Events == 0 {
			continue
		}
		k := skey{s.Account, s.ID}
		t := Tokens{Input: u.Input, Output: u.Output, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite}
		o.Tokens.add(t)
		o.Cost.USD += u.USD
		date := s.Last.In(loc).Format("2006-01-02")
		dd := days[date]
		if dd == nil {
			dd = &Day{Date: date}
			days[date] = dd
		}
		dd.Tokens.add(t)
		dd.Cost += u.USD
		addShare(projects, projSess, s.Project, k, t, u.USD)
		addShare(accts, acctSess, s.Account, k, t, u.USD)
		for name, m := range u.Models {
			mt := Tokens{Input: m.Input, Output: m.Output, CacheRead: m.CacheRead, CacheWrite: m.CacheWrite}
			addShare(models, modelSess, name, k, mt, m.USD)
		}
	}
}

func hasModel(ms []string, m string) bool {
	for _, s := range ms {
		if s == m {
			return true
		}
	}
	return false
}
