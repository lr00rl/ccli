package query

import (
	"context"
	"fmt"
	"slices"
	"sort"
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
// neither. A CLI row that is the IDE's copy of a composer takes nothing and
// names the IDE account as where it came from.
func (d *DB) applyCursorUsage(ctx context.Context, rows []SessionRow) error {
	links, err := d.cursorLinks(ctx)
	if err != nil {
		return err
	}
	for i := range rows {
		if from := links.ideCopy(rows[i].Tool, rows[i].ID); from != "" && rows[i].CopiedFrom == "" {
			rows[i].CopiedFrom = from
		}
	}
	led := d.cursorLedger()
	if led == nil || len(led.Sessions) == 0 {
		return nil
	}
	for i := range rows {
		if !links.billedHere(rows[i].Tool, rows[i].ID) {
			continue
		}
		rows[i].ownTokens, rows[i].ownCost = rows[i].Tokens, rows[i].Cost
		u := usageOf(led, rows[i].ID, links.kids[rows[i].ID], time.Time{}, time.Time{})
		if u.Events == 0 {
			continue
		}
		rows[i].Tokens, rows[i].Cost = mergeBilled(rows[i].Tool, rows[i].Tokens, rows[i].Cost, u)
		rows[i].Models = byBilled(rows[i].Models, u)
	}
	return nil
}

// mergeBilled folds dashboard usage into a row.
func mergeBilled(tool account.Tool, tokens Tokens, cost Cost, u cursorusage.Use) (Tokens, Cost) {
	if u.Events == 0 {
		return tokens, cost
	}
	t, usd, ok := billedAdds(tool, tokens, cost, Tokens{Input: u.Input, Output: u.Output, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite}, u.USD)
	if !ok {
		return tokens, cost
	}
	tokens.add(t)
	cost.USD += usd
	cost.Unpriced = 0
	return tokens, cost
}

// billedAdds is the one rule for what billed usage adds to a Cursor
// session's own figures, used by the session list, account totals and the
// dashboard alike. A CLI transcript has neither tokens nor cost, so billed
// usage is the whole figure, as it is for an IDE composer with no token
// counts. An IDE composer whose bubbles carry tokens keeps them and takes
// only the billed amount, and only when ccli could not price those tokens
// itself (ok is false when it could).
func billedAdds(tool account.Tool, ownTokens Tokens, ownCost Cost, tokens Tokens, usd float64) (Tokens, float64, bool) {
	if tool == account.CursorIDE && ownTokens.All() > 0 {
		return Tokens{}, usd, ownCost.USD == 0
	}
	return tokens, usd, true
}

func (d *DB) applyCursorDetail(ctx context.Context, det *SessionDetail) error {
	led := d.cursorLedger()
	if led == nil || det.Tool != account.Cursor && det.Tool != account.CursorIDE {
		return nil
	}
	links, err := d.cursorLinks(ctx)
	if err != nil {
		return err
	}
	if !links.billedHere(det.Tool, det.ID) {
		return nil
	}
	u := usageOf(led, det.ID, links.kids[det.ID], time.Time{}, time.Time{})
	if u.Events == 0 {
		return nil
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
	return nil
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

// cursorLinks is what joins billed Cursor usage onto sessions.
type cursorLinks struct {
	kids   map[string][]string // a session's subagent ids
	ideIDs map[string]string   // session and subagent ids in Cursor IDE accounts, to the account
}

// cursorLinks loads the joins once per DB. A load that fails returns its
// error, so no total is shown short of a subagent's usage, and the next
// caller tries again.
func (d *DB) cursorLinks(ctx context.Context) (*cursorLinks, error) {
	d.linkMu.Lock()
	defer d.linkMu.Unlock()
	if d.links != nil {
		return d.links, nil
	}
	l := &cursorLinks{}
	var err error
	if l.kids, err = d.cursorChildren(ctx); err != nil {
		return nil, fmt.Errorf("cursor subagents: %w", err)
	}
	if l.ideIDs, err = d.ideComposers(ctx); err != nil {
		return nil, fmt.Errorf("cursor ide composers: %w", err)
	}
	d.links = l
	return l, nil
}

func (d *DB) cursorChildren(ctx context.Context) (map[string][]string, error) {
	rows, err := d.sql().QueryContext(ctx, `SELECT session_id, agent_id FROM files WHERE kind = 'subagent' AND agent_id <> ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	kids := map[string][]string{}
	for rows.Next() {
		var session, agent string
		if err := rows.Scan(&session, &agent); err != nil {
			return nil, err
		}
		if agent != session {
			kids[session] = append(kids[session], agent)
		}
	}
	return kids, rows.Err()
}

// ideComposers maps every session and subagent id in the Cursor IDE
// accounts to its account. A subagent composer counts too: its copy can be a
// CLI session of its own when no chat store says whose subagent it is.
func (d *DB) ideComposers(ctx context.Context) (map[string]string, error) {
	out := map[string]string{}
	var names []any
	marks := ""
	for _, a := range d.accts {
		if a.Kind() == account.CursorIDE {
			names = append(names, a.Name)
			marks += ",?"
		}
	}
	if len(names) == 0 {
		return out, nil
	}
	in := marks[1:]
	rows, err := d.sql().QueryContext(ctx, `SELECT account, session_id FROM files WHERE account IN (`+in+`)
		UNION SELECT account, agent_id FROM files WHERE account IN (`+in+`) AND agent_id <> ''`, append(names, names...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var acct, sid string
		if err := rows.Scan(&acct, &sid); err != nil {
			return nil, err
		}
		if out[sid] == "" {
			out[sid] = acct
		}
	}
	return out, rows.Err()
}

// billedHere says whether a session's row carries its billed Cursor usage:
// any Cursor row except the IDE's copy of a composer, so a conversation is
// billed once.
func (l *cursorLinks) billedHere(tool account.Tool, id string) bool {
	if tool != account.Cursor && tool != account.CursorIDE {
		return false
	}
	return l.ideCopy(tool, id) == ""
}

// ideCopy names the Cursor IDE account when a Cursor CLI session is a copy
// of one of its composers. An IDE agent writes its transcript into
// ~/.cursor/projects as the CLI does, under the composer's id, so the same
// conversation turns up in both accounts. The IDE is where it lives and is
// resumed; the copy has no chat store of its own.
func (l *cursorLinks) ideCopy(tool account.Tool, id string) string {
	if tool != account.Cursor {
		return ""
	}
	return l.ideIDs[id]
}

// addCursorAccountUsage adds billed Cursor usage onto account totals, and
// only the events inside the filter, under the rule in billedAdds. IDE token
// rows are already in the SQL sum, which is why billedAdds never adds a
// second copy of them.
func (d *DB) addCursorAccountUsage(ctx context.Context, f Filter, out []AccountSummary, idx map[string]int) error {
	led := d.cursorLedger()
	if len(led.Sessions) == 0 {
		return nil
	}
	rows, err := d.loadSessions(ctx, f)
	if err != nil {
		return err
	}
	links, err := d.cursorLinks(ctx)
	if err != nil {
		return err
	}
	for _, s := range rows {
		i, ok := idx[s.Account]
		if !ok || !links.billedHere(s.Tool, s.ID) {
			continue
		}
		u := usageOf(led, s.ID, links.kids[s.ID], f.Since, f.Until)
		if u.Events == 0 {
			continue
		}
		t, usd, ok := billedAdds(s.Tool, s.ownTokens, s.ownCost, Tokens{Input: u.Input, Output: u.Output, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite}, u.USD)
		if !ok {
			continue
		}
		out[i].Tokens.add(t)
		out[i].Cost.USD += usd
		out[i].Cost.Unpriced = 0
	}
	return nil
}

// addCursorOverview adds billed Cursor usage to the dashboard, under the
// rule in billedAdds. Each event lands on its own day.
func (d *DB) addCursorOverview(ctx context.Context, sessions []SessionRow, o *Overview, days map[string]*Day, projects, models, accts map[string]*Share, projSess, modelSess, acctSess map[string]map[skey]bool, loc *time.Location) error {
	led := d.cursorLedger()
	if len(led.Sessions) == 0 {
		return nil
	}
	links, err := d.cursorLinks(ctx)
	if err != nil {
		return err
	}
	since, until := o.Filter.Since, o.Filter.Until
	for i := range sessions {
		s := &sessions[i]
		if !links.billedHere(s.Tool, s.ID) {
			continue
		}
		k := skey{s.Account, s.ID}
		for _, h := range hitsOf(led, s.ID, links.kids[s.ID], since, until) {
			when := time.UnixMilli(h.At).In(loc)
			t, usd, ok := billedAdds(s.Tool, s.ownTokens, s.ownCost, Tokens{Input: h.Input, Output: h.Output, CacheRead: h.CacheRead, CacheWrite: h.CacheWrite}, h.USD)
			if !ok {
				break
			}
			o.Tokens.add(t)
			o.Cost.USD += usd
			date := when.Format("2006-01-02")
			dd := days[date]
			if dd == nil {
				dd = &Day{Date: date}
				days[date] = dd
			}
			dd.Tokens.add(t)
			dd.Cost += usd
			addShare(projects, projSess, s.Project, k, t, usd)
			addShare(accts, acctSess, s.Account, k, t, usd)
			if h.Model != "" {
				addShare(models, modelSess, modelName(h.Model), k, t, usd)
			}
		}
	}
	return nil
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

// byBilled orders a Cursor session's models by billed cost, costliest
// first, and adds the billed models its transcript does not name. Models
// with no billed cost keep their order after those.
func byBilled(models []string, u cursorusage.Use) []string {
	usd := map[string]float64{}
	for raw, m := range u.Models {
		usd[modelName(raw)] += m.USD
	}
	out := slices.Clone(models)
	var extra []string
	for name := range usd {
		if !slices.Contains(out, name) {
			extra = append(extra, name)
		}
	}
	sort.Strings(extra)
	out = append(out, extra...)
	sort.SliceStable(out, func(a, b int) bool { return usd[out[a]] > usd[out[b]] })
	return out
}
