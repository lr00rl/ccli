package query

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/lr00rl/ccli/internal/account"
)

// sessionSQL lists sessions from their files, those active in the range
// filter (?1, ?2). %[1]s takes a WHERE clause that limits it to one session
// (?3, ?4), or nothing for every session. Usage comes after the other
// filters, from usageSQL, for the sessions they keep.
const sessionSQL = `
WITH f AS (
  SELECT account, session_id,
    MAX(CASE WHEN kind = 'main' THEN id END) AS main_id,
    MIN(NULLIF(first_ts, 0)) AS started,
    MAX(last_ts) AS last,
    SUM(CASE WHEN kind = 'main' THEN active_ms ELSE 0 END) AS active,
    SUM(typed_prompts) AS typed,
    SUM(other_prompts) AS other,
    SUM(tool_calls) AS tools,
    SUM(kind = 'subagent') AS subagents,
    COUNT(DISTINCT NULLIF(run_id, '')) AS runs,
    MAX(project_key) AS project_key,
    MAX(team_name) AS team,
    MAX(agent_name) AS agent_name
  FROM files %[1]s GROUP BY account, session_id
)
SELECT f.account, f.session_id, COALESCE(m.cwd, ''), COALESCE(m.custom_title, ''), COALESCE(m.ai_title, ''),
  COALESCE(m.summary, ''), COALESCE(m.first_prompt, ''), COALESCE(f.started, 0), COALESCE(f.last, 0), f.active,
  f.typed, f.other, f.tools, f.subagents, f.runs, f.project_key, COALESCE(f.team, ''), COALESCE(f.agent_name, ''),
  COALESCE(m.missing, 1), COALESCE(m.git_branch, ''), COALESCE(m.entrypoint, ''), COALESCE(m.version, '')
FROM f
LEFT JOIN files m ON m.id = f.main_id
WHERE COALESCE(f.last, 0) >= ?1 AND COALESCE(f.started, 0) <= ?2`

// usageSQL sums responses per session and model, for the sessions in a JSON
// array of [account, session id] pairs. One pass gives both the totals and
// the model list, and the (account, session_id) index keeps it to those
// sessions.
const usageSQL = `
SELECT account, session_id, model,
  SUM(input), SUM(output), SUM(cache_read), SUM(cache_5m + cache_1h), SUM(web_search),
  SUM(COALESCE(cost, 0)),
  SUM(CASE WHEN kind = 'adjustment' THEN COALESCE(cost, 0) ELSE 0 END),
  SUM(cost IS NULL AND kind <> 'adjustment' AND input + output + cache_read + cache_5m + cache_1h + web_search > 0),
  SUM(kind = 'main'), COUNT(*)
FROM responses
WHERE (account, session_id) IN (SELECT json_extract(value, '$[0]'), json_extract(value, '$[1]') FROM json_each(?))
GROUP BY account, session_id, model`

func (d *DB) loadSessions(ctx context.Context, f Filter) ([]SessionRow, error) {
	return d.querySessions(ctx, f, nil)
}

// querySessions loads the sessions the filter selects, or with only set,
// just that one. Responses are read only for the sessions that pass every
// filter but the model one, which needs them.
func (d *DB) querySessions(ctx context.Context, f Filter, only *skey) ([]SessionRow, error) {
	tags, err := d.st.Tags(ctx)
	if err != nil {
		return nil, err
	}
	copied, err := d.copiedFrom(ctx)
	if err != nil {
		return nil, err
	}
	q, args := fmt.Sprintf(sessionSQL, ""), []any{f.sinceMS(), f.untilMS()}
	if only != nil {
		q = fmt.Sprintf(sessionSQL, "WHERE account = ?3 AND session_id = ?4")
		args = append(args, only.account, only.id)
	}
	rows, err := d.sql().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionRow
	for rows.Next() {
		var s SessionRow
		var custom, ai, summary, prompt string
		var started, last int64
		var missing int
		if err := rows.Scan(&s.Account, &s.ID, &s.Cwd, &custom, &ai, &summary, &prompt, &started, &last, &s.ActiveMS,
			&s.Prompts, &s.OtherPrompts, &s.ToolCalls, &s.Subagents, &s.Runs, &s.ProjectKey, &s.Team, &s.AgentName,
			&missing, &s.GitBranch, &s.Entrypoint, &s.Version); err != nil {
			return nil, err
		}
		if !f.wantsAccount(s.Account) {
			continue
		}
		s.Tool = d.toolOf(s.Account)
		s.Title, s.TitleSource = title(custom, ai, summary, prompt)
		s.Tag = tags[s.ID]
		s.Started, s.Last = ms(started), ms(last)
		s.Missing = missing != 0
		s.Project = projectName(s.Cwd, s.ProjectKey)
		_, s.Live = d.live[skey{s.Account, s.ID}]
		s.CopiedFrom = copied[skey{s.Account, s.ID}]
		if !matches(s, f) {
			continue
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	if err := d.addUsage(ctx, out); err != nil {
		return nil, err
	}
	if f.Model != "" {
		out = slices.DeleteFunc(out, func(s SessionRow) bool { return !usesModel(s, f.Model) })
	}
	if err := d.markHidden(ctx, out); err != nil {
		return nil, err
	}
	if err := d.applyCursorUsage(ctx, out); err != nil {
		return nil, err
	}
	return out, nil
}

// addUsage fills in tokens, cost, replies and models from the responses of
// the given sessions. Models come costliest first; models without a cost
// (Cursor's, before billed usage joins) come by how often they answered.
func (d *DB) addUsage(ctx context.Context, ss []SessionRow) error {
	if len(ss) == 0 {
		return nil
	}
	keys := make([][2]string, len(ss))
	at := make(map[skey]int, len(ss))
	for i, s := range ss {
		keys[i] = [2]string{s.Account, s.ID}
		at[skey{s.Account, s.ID}] = i
	}
	list, err := json.Marshal(keys)
	if err != nil {
		return err
	}
	rows, err := d.sql().QueryContext(ctx, usageSQL, string(list))
	if err != nil {
		return err
	}
	defer rows.Close()
	type priced struct {
		model string
		usd   float64
		calls int
	}
	models := map[int][]priced{}
	for rows.Next() {
		var k skey
		var model string
		var t Tokens
		var c Cost
		var replies, calls int
		if err := rows.Scan(&k.account, &k.id, &model, &t.Input, &t.Output, &t.CacheRead, &t.CacheWrite, &t.WebSearch,
			&c.USD, &c.Unlogged, &c.Unpriced, &replies, &calls); err != nil {
			return err
		}
		i, ok := at[k]
		if !ok {
			continue
		}
		ss[i].Tokens.add(t)
		ss[i].Cost.add(c)
		ss[i].Replies += replies
		if model != "" {
			models[i] = append(models[i], priced{model, c.USD, calls})
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i, ms := range models {
		sort.Slice(ms, func(a, b int) bool {
			if ms[a].usd != ms[b].usd {
				return ms[a].usd > ms[b].usd
			}
			if ms[a].calls != ms[b].calls {
				return ms[a].calls > ms[b].calls
			}
			return ms[a].model < ms[b].model
		})
		for _, m := range ms {
			if name := modelName(m.model); !slices.Contains(ss[i].Models, name) {
				ss[i].Models = append(ss[i].Models, name)
			}
		}
	}
	return nil
}

func matches(s SessionRow, f Filter) bool {
	if f.Project != "" && !containsFold(s.Cwd, f.Project) && !containsFold(s.ProjectKey, f.Project) {
		return false
	}
	if len(f.Dirs) > 0 && UnderAny(s.Cwd, f.Dirs) == "" {
		return false
	}
	if len(f.Tools) > 0 && !slices.Contains(f.Tools, s.Tool) {
		return false
	}
	if f.Search != "" && !containsFold(s.Title, f.Search) && !containsFold(s.Tag, f.Search) &&
		!strings.HasPrefix(s.ID, f.Search) && !containsFold(s.Cwd, f.Search) {
		return false
	}
	return true
}

// usesModel says whether one of a session's models matches the --model
// filter. It is checked once responses are read, after the other filters.
func usesModel(s SessionRow, model string) bool {
	for _, m := range s.Models {
		if containsFold(m, model) {
			return true
		}
	}
	return false
}

func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

// ideDraft is a Cursor IDE composer that was never used: no title and no
// prompt. Cursor writes one of these for an empty window and for a chat
// that was opened and closed without a message.
func ideDraft(s SessionRow) bool {
	return s.Tool == account.CursorIDE && s.Prompts == 0 && s.Title == ""
}

// Sessions lists sessions active in the filter's range.
func (d *DB) Sessions(ctx context.Context, f Filter) (SessionPage, error) {
	rows, err := d.loadSessions(ctx, f)
	if err != nil {
		return SessionPage{}, err
	}
	page := SessionPage{Filter: f}
	all := rows[:0]
	for _, s := range rows {
		switch {
		case !f.Show.keeps(s.Hidden):
			if s.Hidden {
				page.Hidden++
			}
		case !f.Drafts && ideDraft(s):
			page.Drafts++
		default:
			all = append(all, s)
		}
	}
	sortSessions(all, f.Sort)
	page.Matched = len(all)
	for _, s := range all {
		page.Tokens.add(s.Tokens)
		page.Cost.add(s.Cost)
	}
	if f.Limit > 0 && len(all) > f.Limit {
		all = all[:f.Limit]
	}
	page.Sessions = all
	if len(all) == 0 && !f.Since.IsZero() {
		wide := f
		wide.Since, wide.Until, wide.Limit = time.Time{}, time.Time{}, 0
		older, err := d.loadSessions(ctx, wide)
		if err != nil {
			return page, err
		}
		for _, s := range older {
			if f.Show.keeps(s.Hidden) && (f.Drafts || !ideDraft(s)) {
				page.Older++
			}
		}
	}
	return page, nil
}

// SessionCopies returns the same rows as Sessions, scoped to every account
// holding one exact session id. Hidden sessions and drafts remain visible so
// an ambiguity report never omits a candidate.
func (d *DB) SessionCopies(ctx context.Context, id string) (SessionPage, error) {
	accounts, err := d.CopiesOf(ctx, id)
	if err != nil {
		return SessionPage{}, err
	}
	if len(accounts) == 0 {
		return SessionPage{}, fmt.Errorf("%w %q", ErrNotFound, id)
	}
	f := Filter{Accounts: accounts, Search: id, Sort: "last", Show: AnyHidden, Drafts: true}
	page := SessionPage{Filter: f}
	for _, acct := range accounts {
		rows, err := d.querySessions(ctx, f, &skey{acct, id})
		if err != nil {
			return SessionPage{}, err
		}
		if len(rows) != 1 || rows[0].ID != id {
			return SessionPage{}, fmt.Errorf("session %s in account %s has no list row", id, acct)
		}
		page.Sessions = append(page.Sessions, rows[0])
		page.Tokens.add(rows[0].Tokens)
		page.Cost.add(rows[0].Cost)
	}
	sortSessions(page.Sessions, f.Sort)
	page.Matched = len(page.Sessions)
	return page, nil
}

func sortSessions(ss []SessionRow, by string) {
	less := func(i, j int) bool { return ss[i].Last.After(ss[j].Last) }
	switch by {
	case "start":
		less = func(i, j int) bool { return ss[i].Started.After(ss[j].Started) }
	case "cost":
		less = func(i, j int) bool { return ss[i].Cost.USD > ss[j].Cost.USD }
	case "tokens":
		less = func(i, j int) bool { return ss[i].Tokens.InOut() > ss[j].Tokens.InOut() }
	case "active":
		less = func(i, j int) bool { return ss[i].ActiveMS > ss[j].ActiveMS }
	}
	sort.SliceStable(ss, less)
}

// Accounts summarizes every account; usage counts responses in the range.
func (d *DB) Accounts(ctx context.Context, f Filter) ([]AccountSummary, error) {
	out := make([]AccountSummary, 0, len(d.accts))
	idx := map[string]int{}
	for _, a := range d.accts {
		if !f.wantsAccount(a.Name) {
			continue
		}
		idx[a.Name] = len(out)
		out = append(out, AccountSummary{Account: a})
	}
	rows, err := d.sql().QueryContext(ctx, `SELECT account, COUNT(DISTINCT session_id), MAX(last_ts),
		COUNT(DISTINCT CASE WHEN kind = 'main' AND missing = 0 THEN session_id END),
		COUNT(DISTINCT CASE WHEN last_ts >= ? AND first_ts <= ? THEN session_id END)
		FROM files GROUP BY account`, f.sinceMS(), f.untilMS())
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var acct string
		var n, onDisk, inRange int
		var last int64
		if err := rows.Scan(&acct, &n, &last, &onDisk, &inRange); err != nil {
			rows.Close()
			return nil, err
		}
		if i, ok := idx[acct]; ok {
			out[i].Sessions, out[i].Transcripts, out[i].SessionsInRange = n, onDisk, inRange
			out[i].LastActive = ms(last)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = d.sql().QueryContext(ctx, `SELECT account, SUM(input), SUM(output), SUM(cache_read), SUM(cache_5m + cache_1h), SUM(web_search),
		SUM(COALESCE(cost, 0)), SUM(cost IS NULL AND kind <> 'adjustment' AND input + output + cache_read + cache_5m + cache_1h + web_search > 0), SUM(CASE WHEN kind = 'adjustment' THEN COALESCE(cost, 0) ELSE 0 END)
		FROM responses WHERE ts BETWEEN ? AND ? GROUP BY account`, f.sinceMS(), f.untilMS())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var acct string
		var t Tokens
		var c Cost
		if err := rows.Scan(&acct, &t.Input, &t.Output, &t.CacheRead, &t.CacheWrite, &t.WebSearch, &c.USD, &c.Unpriced, &c.Unlogged); err != nil {
			return nil, err
		}
		if i, ok := idx[acct]; ok {
			out[i].Tokens, out[i].Cost = t, c
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := d.addCursorAccountUsage(ctx, f, out, idx); err != nil {
		return nil, err
	}
	for _, l := range d.live {
		if i, ok := idx[l.Account]; ok {
			out[i].Live++
		}
	}
	return out, rows.Err()
}

// RangeDays returns a filter for the last n days (n <= 0 means all time).
func RangeDays(n int, loc *time.Location) (time.Time, time.Time) {
	if n <= 0 {
		return time.Time{}, time.Time{}
	}
	now := time.Now().In(loc)
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, -(n - 1))
	return start, time.Time{}
}

// copiedFrom maps a session in an account to the account the newest copy or
// move into it came from. A fork is left out: it gets a new id.
func (d *DB) copiedFrom(ctx context.Context) (map[skey]string, error) {
	rows, err := d.sql().QueryContext(ctx, `SELECT to_account, session_id, from_account FROM transfers WHERE mode <> 'fork' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[skey]string{}
	for rows.Next() {
		var k skey
		var from string
		if err := rows.Scan(&k.account, &k.id, &from); err != nil {
			return nil, err
		}
		out[k] = from
	}
	return out, rows.Err()
}

// Usage sums an account's usage since a time.
func (d *DB) Usage(ctx context.Context, acct string, since time.Time) (Tokens, Cost, error) {
	var t Tokens
	var c Cost
	err := d.sql().QueryRowContext(ctx, `SELECT COALESCE(SUM(input), 0), COALESCE(SUM(output), 0), COALESCE(SUM(cache_read), 0),
		COALESCE(SUM(cache_5m + cache_1h), 0), COALESCE(SUM(web_search), 0), COALESCE(SUM(COALESCE(cost, 0)), 0),
		COALESCE(SUM(cost IS NULL AND kind <> 'adjustment' AND input + output + cache_read + cache_5m + cache_1h + web_search > 0), 0),
		COALESCE(SUM(CASE WHEN kind = 'adjustment' THEN COALESCE(cost, 0) ELSE 0 END), 0)
		FROM responses WHERE account = ? AND ts >= ?`, acct, since.UnixMilli()).
		Scan(&t.Input, &t.Output, &t.CacheRead, &t.CacheWrite, &t.WebSearch, &c.USD, &c.Unpriced, &c.Unlogged)
	return t, c, err
}
