package query

import (
	"context"
	"sort"
	"time"
)

// Overview is the dashboard for a range: KPIs, daily series, shares,
// hour-by-weekday activity, session shape and skills.
type Overview struct {
	Filter       Filter       `json:"filter"`
	From         time.Time    `json:"from"`
	To           time.Time    `json:"to"`
	Sessions     int          `json:"sessions"`
	MedianTokens int64        `json:"median_session_tokens"`
	P90Tokens    int64        `json:"p90_session_tokens"`
	Tokens       Tokens       `json:"tokens"`
	Cost         Cost         `json:"cost"`
	CacheHitRate float64      `json:"cache_hit_rate"`
	ActiveMS     int64        `json:"active_ms"`
	ActiveDays   int          `json:"active_days"`
	Projects     int          `json:"projects"`
	Daily        []Day        `json:"daily"`
	ByProject    []Share      `json:"by_project"`
	ByModel      []Share      `json:"by_model"`
	ByAccount    []Share      `json:"by_account"`
	Heatmap      [7][24]int64 `json:"heatmap_active_ms"` // Monday first, local hours
	Shape        Shape        `json:"shape"`
	Skills       []Skill      `json:"skills"`
	Recent       []SessionRow `json:"recent"`
}

// Day is one local calendar day.
type Day struct {
	Date   string  `json:"date"`
	Tokens Tokens  `json:"tokens"`
	Cost   float64 `json:"cost"`
}

// Share is one slice of a breakdown.
type Share struct {
	Name     string  `json:"name"`
	Tokens   Tokens  `json:"tokens"`
	Cost     float64 `json:"cost"`
	Sessions int     `json:"sessions"`
}

// Bucket is one bar of a histogram.
type Bucket struct {
	Label string `json:"label"`
	Count int    `json:"count"`
}

// Shape describes how sessions are distributed.
type Shape struct {
	Prompts        []Bucket `json:"prompts"`
	ActiveMinutes  []Bucket `json:"active_minutes"`
	ToolsPerPrompt []Bucket `json:"tools_per_prompt"`
}

// Skill is one skill's use in the range.
type Skill struct {
	Name     string `json:"name"`
	Uses     int    `json:"uses"`
	Sessions int    `json:"sessions"`
}

type skey struct{ account, id string }

// Overview builds the dashboard for the filter.
func (d *DB) Overview(ctx context.Context, f Filter) (*Overview, error) {
	loc := f.loc()
	sessions, err := d.loadSessions(ctx, f)
	if err != nil {
		return nil, err
	}
	sortSessions(sessions, "last")
	o := &Overview{Filter: f, From: f.Since, To: f.Until}
	if o.To.IsZero() {
		o.To = time.Now()
	}
	allowed := map[skey]*SessionRow{}
	for i := range sessions {
		allowed[skey{sessions[i].Account, sessions[i].ID}] = &sessions[i]
	}
	o.Sessions = len(sessions)

	// Usage by hour, session and model.
	rows, err := d.sql().QueryContext(ctx, `SELECT ts / 3600000, account, session_id, model,
		SUM(input), SUM(output), SUM(cache_read), SUM(cache_5m + cache_1h), SUM(web_search),
		SUM(COALESCE(cost, 0)), SUM(cost IS NULL AND kind <> 'adjustment' AND input + output + cache_read + cache_5m + cache_1h + web_search > 0), SUM(CASE WHEN kind = 'adjustment' THEN COALESCE(cost, 0) ELSE 0 END)
		FROM responses WHERE ts BETWEEN ? AND ? GROUP BY 1, 2, 3, 4`, f.sinceMS(), f.untilMS())
	if err != nil {
		return nil, err
	}
	days := map[string]*Day{}
	projects := map[string]*Share{}
	models := map[string]*Share{}
	accts := map[string]*Share{}
	projSess := map[string]map[skey]bool{}
	modelSess := map[string]map[skey]bool{}
	acctSess := map[string]map[skey]bool{}
	var first int64
	for rows.Next() {
		var hour int64
		var k skey
		var model string
		var t Tokens
		var c Cost
		if err := rows.Scan(&hour, &k.account, &k.id, &model, &t.Input, &t.Output, &t.CacheRead, &t.CacheWrite, &t.WebSearch, &c.USD, &c.Unpriced, &c.Unlogged); err != nil {
			rows.Close()
			return nil, err
		}
		s, ok := allowed[k]
		if !ok || (f.Model != "" && !containsFold(model, f.Model)) {
			continue
		}
		at := time.UnixMilli(hour * 3_600_000).In(loc)
		if first == 0 || hour < first {
			first = hour
		}
		o.Tokens.add(t)
		o.Cost.add(c)
		date := at.Format("2006-01-02")
		dd := days[date]
		if dd == nil {
			dd = &Day{Date: date}
			days[date] = dd
		}
		dd.Tokens.add(t)
		dd.Cost += c.USD
		addShare(projects, projSess, s.Project, k, t, c.USD)
		addShare(models, modelSess, modelName(model), k, t, c.USD)
		addShare(accts, acctSess, k.account, k, t, c.USD)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := d.addCursorOverview(ctx, sessions, o, days, projects, models, accts, projSess, modelSess, acctSess, loc); err != nil {
		return nil, err
	}
	if o.From.IsZero() && first > 0 {
		o.From = time.UnixMilli(first * 3_600_000)
	}
	o.Daily = fillDays(days, o.From, o.To, loc)
	for _, dd := range o.Daily {
		if dd.Tokens.All() > 0 {
			o.ActiveDays++
		}
	}
	o.ByProject, o.ByModel, o.ByAccount = shares(projects), shares(models), shares(accts)
	o.Projects = len(projects)
	if denom := o.Tokens.Input + o.Tokens.CacheRead + o.Tokens.CacheWrite; denom > 0 {
		o.CacheHitRate = float64(o.Tokens.CacheRead) / float64(denom)
	}

	// Active time by local weekday and hour.
	arows, err := d.sql().QueryContext(ctx, `SELECT a.hour, a.ms, f.account, f.session_id FROM activity a JOIN files f ON f.id = a.file_id
		WHERE a.hour BETWEEN ? AND ?`, f.sinceMS()-3_600_000, f.untilMS())
	if err != nil {
		return nil, err
	}
	for arows.Next() {
		var hour, msv int64
		var k skey
		if err := arows.Scan(&hour, &msv, &k.account, &k.id); err != nil {
			arows.Close()
			return nil, err
		}
		if _, ok := allowed[k]; !ok {
			continue
		}
		at := time.UnixMilli(hour).In(loc)
		o.Heatmap[(int(at.Weekday())+6)%7][at.Hour()] += msv
		o.ActiveMS += msv
	}
	arows.Close()
	if err := arows.Err(); err != nil {
		return nil, err
	}

	// Session shape and token percentiles.
	var perSession []int64
	prompts, minutes, tpp := newHist(promptEdges), newHist(promptEdges), newHist(toolEdges)
	for _, s := range sessions {
		perSession = append(perSession, s.Tokens.InOut())
		if s.Prompts > 0 {
			prompts.add(int64(s.Prompts))
			tpp.add(int64(s.ToolCalls / s.Prompts))
		}
		if m := s.ActiveMS / 60_000; m > 0 {
			minutes.add(m)
		}
	}
	o.Shape = Shape{Prompts: prompts.buckets(), ActiveMinutes: minutes.buckets(), ToolsPerPrompt: tpp.buckets()}
	o.MedianTokens, o.P90Tokens = percentile(perSession, 0.5), percentile(perSession, 0.9)

	// Skills used by these sessions.
	srows, err := d.sql().QueryContext(ctx, `SELECT f.account, f.session_id, t.skill, t.n FROM tools t JOIN files f ON f.id = t.file_id
		WHERE t.name = 'Skill' AND t.skill <> ''`)
	if err != nil {
		return nil, err
	}
	skills := map[string]*Skill{}
	skillSess := map[string]map[skey]bool{}
	for srows.Next() {
		var k skey
		var name string
		var n int
		if err := srows.Scan(&k.account, &k.id, &name, &n); err != nil {
			srows.Close()
			return nil, err
		}
		if _, ok := allowed[k]; !ok {
			continue
		}
		sk := skills[name]
		if sk == nil {
			sk = &Skill{Name: name}
			skills[name] = sk
			skillSess[name] = map[skey]bool{}
		}
		sk.Uses += n
		skillSess[name][k] = true
	}
	srows.Close()
	if err := srows.Err(); err != nil {
		return nil, err
	}
	for name, sk := range skills {
		sk.Sessions = len(skillSess[name])
		o.Skills = append(o.Skills, *sk)
	}
	sort.Slice(o.Skills, func(i, j int) bool {
		if o.Skills[i].Uses != o.Skills[j].Uses {
			return o.Skills[i].Uses > o.Skills[j].Uses
		}
		return o.Skills[i].Name < o.Skills[j].Name
	})
	if len(sessions) > 8 {
		o.Recent = sessions[:8]
	} else {
		o.Recent = sessions
	}
	return o, nil
}

func addShare(m map[string]*Share, sess map[string]map[skey]bool, name string, k skey, t Tokens, cost float64) {
	s := m[name]
	if s == nil {
		s = &Share{Name: name}
		m[name] = s
		sess[name] = map[skey]bool{}
	}
	s.Tokens.add(t)
	s.Cost += cost
	if !sess[name][k] {
		sess[name][k] = true
		s.Sessions++
	}
}

func shares(m map[string]*Share) []Share {
	out := make([]Share, 0, len(m))
	for _, s := range m {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Cost != out[j].Cost {
			return out[i].Cost > out[j].Cost
		}
		return out[i].Tokens.All() > out[j].Tokens.All()
	})
	return out
}

func fillDays(days map[string]*Day, from, to time.Time, loc *time.Location) []Day {
	if from.IsZero() {
		return nil
	}
	start := time.Date(from.In(loc).Year(), from.In(loc).Month(), from.In(loc).Day(), 0, 0, 0, 0, loc)
	end := to.In(loc)
	var out []Day
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		key := d.Format("2006-01-02")
		if v, ok := days[key]; ok {
			out = append(out, *v)
		} else {
			out = append(out, Day{Date: key})
		}
	}
	return out
}

var (
	promptEdges = []int64{1, 6, 16, 31, 61, 121}
	toolEdges   = []int64{0, 1, 3, 6, 11, 21}
)

type hist struct {
	edges  []int64
	counts []int
}

func newHist(edges []int64) *hist { return &hist{edges: edges, counts: make([]int, len(edges))} }

func (h *hist) add(v int64) {
	for i := len(h.edges) - 1; i >= 0; i-- {
		if v >= h.edges[i] {
			h.counts[i]++
			return
		}
	}
}

func (h *hist) buckets() []Bucket {
	out := make([]Bucket, len(h.edges))
	for i, lo := range h.edges {
		var label string
		switch {
		case i == len(h.edges)-1:
			label = itoa(lo) + "+"
		case h.edges[i+1]-1 == lo:
			label = itoa(lo)
		default:
			label = itoa(lo) + "-" + itoa(h.edges[i+1]-1)
		}
		out[i] = Bucket{Label: label, Count: h.counts[i]}
	}
	return out
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func percentile(vs []int64, p float64) int64 {
	if len(vs) == 0 {
		return 0
	}
	s := append([]int64(nil), vs...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	i := int(p * float64(len(s)-1))
	return s[i]
}
