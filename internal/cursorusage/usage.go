// Package cursorusage reads Cursor's dashboard usage for the signed-in
// account and joins it to local CLI and IDE sessions by conversation id.
// The access token Cursor already stored is sent only to api2.cursor.sh.
// It is never written to the cache or included in an error.
package cursorusage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lr00rl/ccli/internal/cursoride"
	"github.com/lr00rl/ccli/internal/sqlitero"
)

const (
	maxPages = 200
	// pageSize is the most events the service returns per page; 2000 is
	// refused. Each request costs seconds, so fewer, larger pages win.
	pageSize      = 1000
	parallelPages = 4 // event pages fetched at once after the first
	freshFor      = 15 * time.Minute
	retryAfter    = 5 * time.Minute // after a failed read, before asking again
	// A read asks only for events from overlap before the newest cached one,
	// which also picks up late changes to recent events. A full read of the
	// year's events runs every fullEvery.
	overlap       = 48 * time.Hour
	fullEvery     = 7 * 24 * time.Hour
	ledgerVersion = 2
)

var (
	refreshMu sync.Mutex
	apiBase   = "https://api2.cursor.sh/aiserver.v1.DashboardService/"
	// httpDo never follows a redirect, so the token goes to apiBase's host
	// and nowhere else. A 3xx is an error like any other status.
	httpDo = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	// stateDBOverride is the state.vscdb tests point at. Empty uses the IDE's.
	stateDBOverride string
	// cliToken reads cursor-agent's login. Tests replace it.
	cliToken = keychainToken
)

// Use is one conversation's usage, summed from the dashboard's event list.
type Use struct {
	Input      int64             `json:"input"`
	Output     int64             `json:"output"`
	CacheRead  int64             `json:"cache_read"`
	CacheWrite int64             `json:"cache_write"`
	USD        float64           `json:"usd"`
	Events     int               `json:"events"`
	Models     map[string]*Model `json:"models,omitempty"`
	Hits       []Hit             `json:"hits,omitempty"`
}

// Hit is one billed event. Day and range totals use its time; the summed
// fields above are the whole conversation.
type Hit struct {
	At         int64   `json:"at"`
	Model      string  `json:"model,omitempty"`
	Input      int64   `json:"input,omitempty"`
	Output     int64   `json:"output,omitempty"`
	CacheRead  int64   `json:"cache_read,omitempty"`
	CacheWrite int64   `json:"cache_write,omitempty"`
	USD        float64 `json:"usd,omitempty"`
}

// Model is one model's share of a conversation.
type Model struct {
	Input      int64   `json:"input"`
	Output     int64   `json:"output"`
	CacheRead  int64   `json:"cache_read"`
	CacheWrite int64   `json:"cache_write"`
	USD        float64 `json:"usd"`
	N          int     `json:"n"`
}

// Add merges b into u.
func (u *Use) Add(b *Use) {
	if b == nil {
		return
	}
	u.Input += b.Input
	u.Output += b.Output
	u.CacheRead += b.CacheRead
	u.CacheWrite += b.CacheWrite
	u.USD += b.USD
	u.Events += b.Events
	for name, m := range b.Models {
		dst := u.Models[name]
		if dst == nil {
			if u.Models == nil {
				u.Models = map[string]*Model{}
			}
			dst = &Model{}
			u.Models[name] = dst
		}
		dst.Input += m.Input
		dst.Output += m.Output
		dst.CacheRead += m.CacheRead
		dst.CacheWrite += m.CacheWrite
		dst.USD += m.USD
		dst.N += m.N
	}
}

// Period is the account's current billing cycle. Amounts are US dollars.
// UsedUSD is included spend against LimitUSD. BonusUSD is free usage the
// plan adds on top, so it is not spend against that limit. Percent is
// included spend over the limit. APIPercent and TotalPercent are the
// dashboard's own pool figures, which are a different measurement.
type Period struct {
	Plan         string    `json:"plan,omitempty"`
	UsedUSD      float64   `json:"used_usd"`
	LimitUSD     float64   `json:"limit_usd"`
	BonusUSD     float64   `json:"bonus_usd,omitempty"`
	Percent      float64   `json:"percent"`
	APIPercent   float64   `json:"api_percent,omitempty"`
	TotalPercent float64   `json:"total_percent,omitempty"`
	Message      string    `json:"message,omitempty"`
	CycleEnd     time.Time `json:"cycle_end,omitzero"`
}

// Ledger is a cached dashboard read.
type Ledger struct {
	Version    int             `json:"version,omitempty"`
	FetchedAt  time.Time       `json:"fetched_at"`
	Sessions   map[string]*Use `json:"sessions,omitempty"`
	Period     *Period         `json:"period,omitempty"`
	EventCount int             `json:"event_count,omitempty"`
	Truncated  bool            `json:"truncated,omitempty"`
	FullAt     time.Time       `json:"full_at,omitzero"` // the last read of the whole year's events
	Error      string          `json:"error,omitempty"`
	FailedAt   time.Time       `json:"failed_at,omitzero"` // the last read that failed; no new read until retryAfter
	FromCache  bool            `json:"-"`

	eventsFailed bool // the event list is incomplete
}

// ErrNoLogin means the Cursor IDE has no stored login to read usage with.
var ErrNoLogin = errors.New("cursor usage: no local login")

// Load reads the cache. A missing file is an empty ledger, not an error, and
// so is one an older ccli wrote: it lacks fields this one needs, such as the
// per-event times that day and range figures use.
func Load(dataDir string) *Ledger {
	b, err := os.ReadFile(filepath.Join(dataDir, "cursor-usage.json"))
	if err != nil {
		return &Ledger{}
	}
	var l Ledger
	if json.Unmarshal(b, &l) != nil || l.Version != ledgerVersion {
		return &Ledger{}
	}
	if l.Sessions == nil {
		l.Sessions = map[string]*Use{}
	}
	return &l
}

// Option controls one dashboard read.
type Option struct {
	// Force fetches even when the cache is still fresh.
	Force bool
	// Trace receives one line per request. It must not retain the access token.
	Trace func(format string, args ...any)
	// Body receives the request JSON and the response JSON. It must not
	// retain the access token. The response is not logged by Trace.
	Body func(format string, args ...any)
	// Asking is called once a login was found, just before the first request.
	Asking func()
}

// Fresh reports whether the cache can be used without asking Cursor: it is
// recent, or a read failed a short while ago and is not retried yet.
func Fresh(dataDir string) bool { return Load(dataDir).Fresh() }

// Fresh is the rule of the function Fresh, for a ledger already loaded.
func (l *Ledger) Fresh() bool {
	if l.Version != ledgerVersion {
		return false
	}
	if !l.FailedAt.IsZero() && time.Since(l.FailedAt) < retryAfter {
		return true
	}
	return l.Error == "" && !l.FetchedAt.IsZero() && time.Since(l.FetchedAt) < freshFor && l.Usable()
}

// Usable says whether a ledger has anything to show, fresh or not.
func (l *Ledger) Usable() bool {
	return l.Version == ledgerVersion && (len(l.Sessions) > 0 || l.Period != nil)
}

// lockFor is how long a lock file holds before it counts as left behind by
// a run that stopped: longer than a read, which times out at 45 seconds.
const lockFor = 2 * time.Minute

// Lock claims the update of the cache across processes, so commands that
// each find it stale start one read between them. ok is false when another
// process holds it. release removes the lock only while it is still this
// one's, not one that took over after it was counted as left behind.
func Lock(dataDir string) (release func(), ok bool) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, false
	}
	p := filepath.Join(dataDir, "cursor-usage.lock")
	token := fmt.Sprintf("%d %d", os.Getpid(), time.Now().UnixNano())
	for range 2 {
		f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_, werr := f.WriteString(token)
			if cerr := f.Close(); werr != nil || cerr != nil {
				os.Remove(p)
				return nil, false
			}
			return func() {
				if b, err := os.ReadFile(p); err == nil && string(b) == token {
					os.Remove(p)
				}
			}, true
		}
		if !os.IsExist(err) || Busy(dataDir) {
			return nil, false
		}
		os.Remove(p) // left behind
	}
	return nil, false
}

// Busy says whether another process is updating the cache.
func Busy(dataDir string) bool {
	st, err := os.Stat(filepath.Join(dataDir, "cursor-usage.lock"))
	return err == nil && time.Since(st.ModTime()) < lockFor
}

// A network or auth failure keeps the previous cache. Callers can still
// use a partial ledger when Period or Sessions is set.
func Refresh(ctx context.Context, dataDir string) (*Ledger, error) {
	return RefreshOption(ctx, dataDir, Option{})
}

// RefreshOption is Refresh with a forced fetch or a per-request trace.
func RefreshOption(ctx context.Context, dataDir string, opt Option) (*Ledger, error) {
	refreshMu.Lock()
	defer refreshMu.Unlock()
	ctx = withTrace(ctx, opt.Trace)
	ctx = withBody(ctx, opt.Body)
	cur := Load(dataDir)
	fresh := cur.Fresh()
	if fresh && !opt.Force {
		cur.FromCache = true
		trace(ctx, "cursor usage: no request, cache is %s old", time.Since(cur.FetchedAt).Round(time.Second))
		return cur, nil
	}
	if fresh && opt.Force {
		trace(ctx, "cursor usage: fetching again although the cache is fresh")
	}
	next, err := fetch(ctx, opt.Asking, cur)
	if err == nil {
		return next, save(dataDir, next)
	}
	// A failure is remembered so the next commands do not ask again at
	// once, and a part that failed keeps what the cache had for it.
	now := time.Now().UTC()
	if cur.Version != ledgerVersion {
		cur = &Ledger{Version: ledgerVersion, Sessions: map[string]*Use{}}
	}
	if next != nil && (len(next.Sessions) > 0 || next.Period != nil) {
		if next.eventsFailed && len(cur.Sessions) > 0 {
			next.Sessions, next.EventCount, next.Truncated = cur.Sessions, cur.EventCount, cur.Truncated
		}
		if next.Period == nil {
			next.Period = cur.Period
		}
		cur = next
	}
	cur.Error, cur.FailedAt = err.Error(), now
	if saveErr := save(dataDir, cur); saveErr != nil {
		trace(ctx, "cursor usage: could not save the cache: %v", saveErr)
	}
	return cur, err
}

// FetchPeriod reads the billing cycle. It does not download usage events
// and does not touch the event cache.
func FetchPeriod(ctx context.Context, opt Option) (*Period, error) {
	ctx = withTrace(ctx, opt.Trace)
	ctx = withBody(ctx, opt.Body)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var p *Period
	err := withToken(func(token string) error {
		var err error
		p, err = period(ctx, token)
		return err
	})
	return p, err
}

func save(dataDir string, l *Ledger) error {
	if l == nil {
		return nil
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(l)
	if err != nil {
		return err
	}
	// A name of its own per write: another ccli may be saving at once. One
	// left an hour ago belongs to a run that stopped mid-save.
	if left, _ := filepath.Glob(filepath.Join(dataDir, "cursor-usage-*.tmp")); len(left) > 0 {
		for _, p := range left {
			if st, err := os.Stat(p); err == nil && time.Since(st.ModTime()) > time.Hour {
				os.Remove(p)
			}
		}
	}
	f, err := os.CreateTemp(dataDir, "cursor-usage-*.tmp")
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), filepath.Join(dataDir, "cursor-usage.json"))
	}
	if err != nil {
		os.Remove(f.Name())
	}
	return err
}

func fetch(ctx context.Context, asking func(), cur *Ledger) (*Ledger, error) {
	if _, err := tokens(); err != nil {
		return nil, err
	}
	if asking != nil {
		asking()
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	// One cheap call picks the login the service accepts, so the parallel
	// event pages below all use it instead of each finding the dead one.
	var token string
	var picked *Period
	var pickErr error
	if err := withToken(func(t string) error {
		p, err := period(ctx, t)
		if err != nil && errors.Is(err, errUnauthorized) {
			return err
		}
		token, picked, pickErr = t, p, err
		return nil
	}); err != nil {
		return nil, err
	}
	now := time.Now()
	led := &Ledger{Version: ledgerVersion, FetchedAt: now.UTC(), Sessions: map[string]*Use{}}
	// The billing period came with the login probe; a failure there other
	// than a refusal is soft, as it was when it ran beside the events.
	p, soft := picked, pickErr
	start, full := now.AddDate(-1, 0, 0), true
	if from := cur.incrementalFrom(now); !from.IsZero() {
		start, full = from, false
		led.Sessions, led.EventCount = cur.sessionsBefore(from)
		led.FullAt = cur.FullAt
	}
	trace(ctx, "cursor usage: events %s to %s (%s)", start.UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339),
		map[bool]string{true: "full read", false: "since the cached events"}[full])
	ev, truncated, evErr := events(ctx, token, start, now)
	led.Period = p
	if full {
		led.Truncated = truncated
		if evErr == nil {
			led.FullAt = now.UTC()
		}
	} else {
		led.Truncated = cur.Truncated || truncated
	}
	for _, e := range ev {
		id := e.ConversationID
		if id == "" {
			continue // joins no session, and is not counted either way
		}
		led.EventCount++
		u := led.Sessions[id]
		if u == nil {
			u = &Use{Models: map[string]*Model{}}
			led.Sessions[id] = u
		}
		addEvent(u, e)
	}
	if evErr != nil {
		led.eventsFailed = true
		return led, evErr
	}
	if led.Period == nil && len(led.Sessions) == 0 {
		if soft != nil {
			return nil, soft
		}
		return nil, fmt.Errorf("cursor usage: empty")
	}
	return led, soft
}

// incrementalFrom is where a read can start instead of a year back: overlap
// before the newest cached event, when the cache holds a full read from
// less than fullEvery ago. Zero means a full read.
func (l *Ledger) incrementalFrom(now time.Time) time.Time {
	if l == nil || l.Version != ledgerVersion || l.FullAt.IsZero() || now.Sub(l.FullAt) >= fullEvery {
		return time.Time{}
	}
	var newest int64
	for _, u := range l.Sessions {
		for _, h := range u.Hits {
			newest = max(newest, h.At)
		}
	}
	if newest == 0 {
		return time.Time{}
	}
	return time.UnixMilli(newest).Add(-overlap)
}

// sessionsBefore rebuilds the cached sessions from their events before
// cutoff, which a read from cutoff does not return again, and counts them.
// An event with no time cannot be asked for again, so it is kept.
func (l *Ledger) sessionsBefore(cutoff time.Time) (map[string]*Use, int) {
	out := map[string]*Use{}
	n := 0
	for id, u := range l.Sessions {
		var kept *Use
		for _, h := range u.Hits {
			if h.At >= cutoff.UnixMilli() {
				continue
			}
			if kept == nil {
				kept = &Use{Models: map[string]*Model{}}
			}
			addHit(kept, h)
			n++
		}
		if kept != nil {
			out[id] = kept
		}
	}
	return out, n
}

func addEvent(u *Use, e event) {
	// The API speaks Connect JSON, which leaves out a field whose value is
	// zero, so an event charged nothing and one with no charge look the same.
	// Both take totalCents, the event's full price, which is also what ccli
	// shows for other tools: list price.
	usd := float64(e.ChargedCents)
	if usd == 0 {
		usd = float64(e.TokenUsage.TotalCents)
	}
	name := e.Model
	if name == "" {
		name = "(unknown)"
	}
	addHit(u, Hit{
		At: int64(e.Timestamp), Model: name,
		Input: int64(e.TokenUsage.Input), Output: int64(e.TokenUsage.Output),
		CacheRead: int64(e.TokenUsage.CacheRead), CacheWrite: int64(e.TokenUsage.CacheWrite), USD: usd / 100,
	})
}

// addHit adds one billed event to a conversation's totals and its list.
func addHit(u *Use, h Hit) {
	u.Input += h.Input
	u.Output += h.Output
	u.CacheRead += h.CacheRead
	u.CacheWrite += h.CacheWrite
	u.USD += h.USD
	u.Events++
	if u.Models == nil {
		u.Models = map[string]*Model{}
	}
	m := u.Models[h.Model]
	if m == nil {
		m = &Model{}
		u.Models[h.Model] = m
	}
	m.Input += h.Input
	m.Output += h.Output
	m.CacheRead += h.CacheRead
	m.CacheWrite += h.CacheWrite
	m.USD += h.USD
	m.N++
	u.Hits = append(u.Hits, h)
}

// Between sums the events whose time falls in [since, until). A zero bound
// is open. Events with no timestamp are left out, so a bounded range never
// receives a whole conversation just because its time was missing.
func (u *Use) Between(since, until time.Time) Use {
	var out Use
	if u == nil {
		return out
	}
	for _, h := range u.Hits {
		if !h.In(since, until) {
			continue
		}
		part := &Use{
			Input: h.Input, Output: h.Output, CacheRead: h.CacheRead, CacheWrite: h.CacheWrite,
			USD: h.USD, Events: 1,
		}
		if h.Model != "" {
			part.Models = map[string]*Model{h.Model: {
				Input: h.Input, Output: h.Output, CacheRead: h.CacheRead, CacheWrite: h.CacheWrite,
				USD: h.USD, N: 1,
			}}
		}
		out.Add(part)
	}
	return out
}

// In reports whether the event falls in [since, until). A zero bound is
// open. An event with no timestamp is out of every range.
func (h Hit) In(since, until time.Time) bool {
	if h.At <= 0 {
		return false
	}
	if !since.IsZero() && h.At < since.UnixMilli() {
		return false
	}
	if !until.IsZero() && h.At >= until.UnixMilli() {
		return false
	}
	return true
}

type flexF float64

func (f *flexF) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		if s == "" {
			return nil
		}
		v, err := strconv.ParseFloat(s, 64)
		*f = flexF(v)
		return err
	}
	v, err := strconv.ParseFloat(string(b), 64)
	*f = flexF(v)
	return err
}

type flexI int64

func (n *flexI) UnmarshalJSON(b []byte) error {
	var f flexF
	if err := f.UnmarshalJSON(b); err != nil {
		return err
	}
	*n = flexI(f)
	return nil
}

type event struct {
	ConversationID string `json:"conversationId"`
	Model          string `json:"model"`
	Timestamp      flexI  `json:"timestamp"`
	ChargedCents   flexF  `json:"chargedCents"`
	TokenUsage     struct {
		Input      flexI `json:"inputTokens"`
		Output     flexI `json:"outputTokens"`
		CacheRead  flexI `json:"cacheReadTokens"`
		CacheWrite flexI `json:"cacheWriteTokens"`
		TotalCents flexF `json:"totalCents"`
	} `json:"tokenUsage"`
}

// events returns the billed events in [start, end]. The first page says how
// many there are; the rest are fetched parallelPages at a time.
func events(ctx context.Context, token string, start, end time.Time) ([]event, bool, error) {
	fetchPage := func(ctx context.Context, page int) ([]event, int, error) {
		body := map[string]any{
			"startDate": strconv.FormatInt(start.UnixMilli(), 10),
			"endDate":   strconv.FormatInt(end.UnixMilli(), 10),
			"page":      page,
			"pageSize":  pageSize,
		}
		var resp struct {
			Total              int     `json:"totalUsageEventsCount"`
			UsageEventsDisplay []event `json:"usageEventsDisplay"`
			UsageEvents        []event `json:"usageEvents"`
		}
		if err := post(ctx, token, "GetFilteredUsageEvents", body, &resp); err != nil {
			return nil, 0, err
		}
		batch := resp.UsageEventsDisplay
		if len(batch) == 0 {
			batch = resp.UsageEvents
		}
		trace(ctx, "GetFilteredUsageEvents page %d: %d events of %d", page, len(batch), resp.Total)
		return batch, resp.Total, nil
	}
	all, total, err := fetchPage(ctx, 1)
	if err != nil || len(all) < pageSize {
		return all, false, err
	}
	pages := (total + pageSize - 1) / pageSize
	if total <= 0 {
		pages = maxPages // no count given: ask until a short page
	}
	truncated := pages > maxPages
	pages = min(pages, maxPages)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	batches := make([][]event, pages+1)
	errs := make([]error, pages+1)
	sem := make(chan struct{}, parallelPages)
	var wg sync.WaitGroup
	for page := 2; page <= pages; page++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				errs[page] = ctx.Err()
				return
			}
			if batches[page], _, errs[page] = fetchPage(ctx, page); errs[page] != nil {
				cancel()
			}
		}()
	}
	wg.Wait()
	for page := 2; page <= pages; page++ {
		if errs[page] != nil {
			return all, truncated, errs[page]
		}
		all = append(all, batches[page]...)
		if total <= 0 && len(batches[page]) < pageSize {
			break // the last page when no count was given
		}
	}
	return all, truncated, nil
}

func period(ctx context.Context, token string) (*Period, error) {
	var usage struct {
		BillingCycleEnd flexI  `json:"billingCycleEnd"`
		DisplayMessage  string `json:"displayMessage"`
		PlanUsage       struct {
			TotalSpend       flexF `json:"totalSpend"`
			IncludedSpend    flexF `json:"includedSpend"`
			BonusSpend       flexF `json:"bonusSpend"`
			Limit            flexF `json:"limit"`
			TotalPercentUsed flexF `json:"totalPercentUsed"`
			APIPercentUsed   flexF `json:"apiPercentUsed"`
		} `json:"planUsage"`
	}
	var plan struct {
		PlanInfo struct {
			PlanName string `json:"planName"`
		} `json:"planInfo"`
	}
	// Two small requests of a second or two each; ask both at once.
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := post(ctx, token, "GetPlanInfo", map[string]any{}, &plan); err != nil {
			trace(ctx, "cursor usage: plan name unknown: %v", err)
		}
	}()
	err := post(ctx, token, "GetCurrentPeriodUsage", map[string]any{}, &usage)
	<-done
	if err != nil {
		return nil, err
	}
	p := fillPeriod(
		float64(usage.PlanUsage.IncludedSpend),
		float64(usage.PlanUsage.BonusSpend),
		float64(usage.PlanUsage.Limit),
		float64(usage.PlanUsage.APIPercentUsed),
		float64(usage.PlanUsage.TotalPercentUsed),
	)
	p.Plan = plan.PlanInfo.PlanName
	p.Message = usage.DisplayMessage
	if ms := int64(usage.BillingCycleEnd); ms > 0 {
		p.CycleEnd = time.UnixMilli(ms)
	}
	return p, nil
}

func fillPeriod(includedCents, bonusCents, limitCents, apiPct, totalPct float64) *Period {
	p := &Period{
		UsedUSD:      includedCents / 100,
		BonusUSD:     bonusCents / 100,
		LimitUSD:     limitCents / 100,
		APIPercent:   apiPct,
		TotalPercent: totalPct,
	}
	if p.LimitUSD > 0 {
		p.Percent = p.UsedUSD / p.LimitUSD * 100
	}
	return p
}

type traceKey struct{}

// withTrace attaches fn, made safe to call from the requests that run at
// once.
func withTrace(ctx context.Context, fn func(string, ...any)) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, traceKey{}, serial(fn))
}

func serial(fn func(string, ...any)) func(string, ...any) {
	var mu sync.Mutex
	return func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		fn(format, args...)
	}
}

func trace(ctx context.Context, format string, args ...any) {
	fn, _ := ctx.Value(traceKey{}).(func(string, ...any))
	if fn != nil {
		fn(format, args...)
	}
}

type bodyKey struct{}

func withBody(ctx context.Context, fn func(string, ...any)) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, bodyKey{}, serial(fn))
}

func bodyTrace(ctx context.Context, format string, args ...any) {
	fn, _ := ctx.Value(bodyKey{}).(func(string, ...any))
	if fn != nil {
		fn(format, args...)
	}
}

func prettyJSON(b []byte) string {
	const max = 512 << 10
	cut := false
	if len(b) > max {
		b, cut = b[:max], true
	}
	var buf bytes.Buffer
	text := string(b)
	if json.Indent(&buf, b, "", "  ") == nil {
		text = buf.String()
	}
	if cut {
		text += "\n… truncated"
	}
	return text
}

func post(ctx context.Context, token, method string, body, dest any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	url := apiBase + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")
	started := time.Now()
	resp, err := httpDo.Do(req)
	if err != nil {
		trace(ctx, "POST %s failed after %s", url, time.Since(started).Round(time.Millisecond))
		return fmt.Errorf("cursor usage: %s: %w", method, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	trace(ctx, "POST %s -> %d, %d bytes, %s", url, resp.StatusCode, len(raw), time.Since(started).Round(time.Millisecond))
	bodyTrace(ctx, "request %s\n%s", method, prettyJSON(b))
	bodyTrace(ctx, "response %s\n%s", method, prettyJSON(raw))
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("cursor usage: %s returned %d: %w", method, resp.StatusCode, errUnauthorized)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("cursor usage: %s returned %d", method, resp.StatusCode)
	}
	if dest == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, dest); err != nil {
		return fmt.Errorf("cursor usage: %s: %w", method, err)
	}
	return nil
}

// tokens are the logins this machine holds, the one most likely to work
// first. CURSOR_TOKEN wins when set. cursor-agent's token comes before the
// IDE's: cursor-agent renews it on each run, while the IDE's stays expired
// in state.vscdb until the app is opened and signs in again, and the
// service then answers 401 for it.
func tokens() ([]string, error) {
	var out []string
	add := func(t string) {
		if t = strings.TrimSpace(t); t != "" {
			for _, have := range out {
				if have == t {
					return
				}
			}
			out = append(out, t)
		}
	}
	add(os.Getenv("CURSOR_TOKEN"))
	add(cliToken())
	if t, err := accessToken(); err == nil {
		add(t)
	}
	if len(out) == 0 {
		return nil, ErrNoLogin
	}
	return out, nil
}

// keychainToken is cursor-agent's login on a Mac: the Keychain item
// `cursor-access-token`, account `cursor-user`, which `cursor-agent login`
// writes. Elsewhere, and when the item is missing, it is "".
func keychainToken() string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "security", "find-generic-password", "-s", "cursor-access-token", "-a", "cursor-user", "-w").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// errUnauthorized is a 401 or 403: this token is refused, another may not be.
var errUnauthorized = errors.New("cursor usage: login refused")

// withToken runs call with each login in turn and moves on only when the
// service refuses one, so a dead token in one store does not hide a working
// one in another. The first refusal is the one reported when all fail.
func withToken(call func(token string) error) error {
	toks, err := tokens()
	if err != nil {
		return err
	}
	var first error
	for _, t := range toks {
		err := call(t)
		if err == nil || !errors.Is(err, errUnauthorized) {
			return err
		}
		if first == nil {
			first = err
		}
	}
	return first
}

// accessToken is the IDE's stored login. The value is not returned in errors.
func accessToken() (string, error) {
	dbPath, err := stateDB()
	if err != nil {
		return "", err
	}
	db, err := sqlitero.Open(dbPath)
	if err != nil {
		return "", ErrNoLogin
	}
	defer db.Close()
	var raw []byte
	if err := db.QueryRow(`SELECT value FROM ItemTable WHERE key = 'cursorAuth/accessToken'`).Scan(&raw); err != nil || len(raw) == 0 {
		return "", ErrNoLogin
	}
	tok := strings.TrimSpace(string(raw))
	if strings.HasPrefix(tok, `"`) {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			tok = s
		}
	}
	if tok == "" {
		return "", ErrNoLogin
	}
	return tok, nil
}

func stateDB() (string, error) {
	if stateDBOverride != "" {
		return stateDBOverride, nil
	}
	if v := os.Getenv("CCLI_CURSOR_IDE_DIRS"); v != "" {
		for _, d := range strings.Split(v, string(os.PathListSeparator)) {
			d = strings.TrimSpace(d)
			if d == "" {
				continue
			}
			p := cursoride.DBPath(d)
			if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
				return p, nil
			}
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", ErrNoLogin
	}
	p := cursoride.DBPath(cursorUser(home))
	if st, err := os.Stat(p); err != nil || !st.Mode().IsRegular() {
		return "", ErrNoLogin
	}
	return p, nil
}

func cursorUser(home string) string {
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Cursor", "User")
	case "windows":
		root := os.Getenv("APPDATA")
		if root == "" {
			root = filepath.Join(home, "AppData", "Roaming")
		}
		return filepath.Join(root, "Cursor", "User")
	default:
		if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
			return filepath.Join(x, "Cursor", "User")
		}
		return filepath.Join(home, ".config", "Cursor", "User")
	}
}
