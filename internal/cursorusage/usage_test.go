package cursorusage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestAddEventCentsAndModels(t *testing.T) {
	got := &Use{Models: map[string]*Model{}}
	addEvent(got, ev("conv-1", "composer-2", 3.45, 100, 20, 400, 0))
	// No chargedCents, so totalCents (150) is the bill.
	second := ev("conv-1", "gpt", 0, 10, 5, 0, 0)
	second.TokenUsage.TotalCents = 150
	addEvent(got, second)
	if got.Input != 110 || got.Output != 25 || got.CacheRead != 400 || got.Events != 2 {
		t.Fatalf("tokens %+v", got)
	}
	if got.USD < 0.0344 || got.USD > 0.0346+1.5 {
		t.Fatalf("usd %v", got.USD)
	}
	// 3.45 cents + 150 cents.
	if got.USD < 1.534 || got.USD > 1.535 {
		t.Fatalf("usd %v, want 1.5345", got.USD)
	}
	if gpt, c2 := got.Models["gpt"], got.Models["composer-2"]; gpt == nil || c2 == nil || gpt.USD != 1.5 || c2.USD < 0.0344 || c2.USD > 0.0346 {
		t.Fatalf("models %+v", got.Models)
	}
}

func ev(id, model string, charged float64, in, out, cache int64, total float64) event {
	e := event{ConversationID: id, Model: model, ChargedCents: flexF(charged)}
	e.TokenUsage.Input = flexI(in)
	e.TokenUsage.Output = flexI(out)
	e.TokenUsage.CacheRead = flexI(cache)
	e.TokenUsage.TotalCents = flexF(total)
	return e
}

func TestRefreshJoinsConversationAndHidesToken(t *testing.T) {
	const secret = "test-token-do-not-store"
	dbPath := filepath.Join(t.TempDir(), "state.vscdb")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE ItemTable (key TEXT, value TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO ItemTable (key, value) VALUES ('cursorAuth/accessToken', ?)`, `"`+secret+`"`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	var pages int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || !strings.Contains(r.Header.Get("Authorization"), secret) {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "GetFilteredUsageEvents"):
			pages++
			if pages == 1 {
				fmtJSON(w, map[string]any{
					"totalUsageEventsCount": 2,
					"usageEventsDisplay": []any{
						map[string]any{"conversationId": "sess-a", "model": "composer-2", "chargedCents": "3.45", "tokenUsage": map[string]any{"inputTokens": "100", "outputTokens": 20, "cacheReadTokens": 5, "cacheWriteTokens": 1, "totalCents": 9}},
						map[string]any{"conversationId": "sess-a", "model": "composer-2", "chargedCents": 10, "tokenUsage": map[string]any{"inputTokens": 1, "outputTokens": 1, "cacheReadTokens": 0, "cacheWriteTokens": 0, "totalCents": 10}},
					},
				})
				return
			}
			fmtJSON(w, map[string]any{"totalUsageEventsCount": 2, "usageEventsDisplay": []any{}})
		case strings.HasSuffix(r.URL.Path, "GetCurrentPeriodUsage"):
			fmtJSON(w, map[string]any{
				"billingCycleEnd": "1893456000000",
				"displayMessage":  "Pro",
				"planUsage": map[string]any{
					"totalSpend": 41963, "includedSpend": 2000, "bonusSpend": 39963,
					"limit": 2000, "totalPercentUsed": 33.57, "apiPercentUsed": 100,
				},
			})
		case strings.HasSuffix(r.URL.Path, "GetPlanInfo"):
			fmtJSON(w, map[string]any{"planInfo": map[string]any{"planName": "Pro"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	oldBase, oldDo, oldDB := apiBase, httpDo, stateDBOverride
	apiBase, httpDo, stateDBOverride = srv.URL+"/", srv.Client(), dbPath
	t.Cleanup(func() {
		apiBase, httpDo, stateDBOverride = oldBase, oldDo, oldDB
	})

	dir := t.TempDir()
	led, err := Refresh(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	u := led.Sessions["sess-a"]
	if u == nil || u.Input != 101 || u.Output != 21 || u.CacheRead != 5 || u.CacheWrite != 1 || u.Events != 2 {
		t.Fatalf("use %+v", u)
	}
	if u.USD < 0.134 || u.USD > 0.135 {
		t.Fatalf("usd %v", u.USD)
	}
	p := led.Period
	if p == nil || p.Plan != "Pro" || p.UsedUSD != 20 || p.LimitUSD != 20 || p.APIPercent != 100 || p.TotalPercent != 33.57 || p.Percent != 100 || p.BonusUSD < 399.62 || p.BonusUSD > 399.64 {
		t.Fatalf("period %+v", p)
	}
	b, err := os.ReadFile(filepath.Join(dir, "cursor-usage.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), secret) {
		t.Fatal("cache contains the access token")
	}
	again, err := Refresh(context.Background(), dir)
	if err != nil || again.Sessions["sess-a"].Events != 2 {
		t.Fatalf("cached %+v %v", again, err)
	}
}

func TestTraceNamesRequestsAndSkipsToken(t *testing.T) {
	const secret = "trace-token-do-not-print"
	dbPath := filepath.Join(t.TempDir(), "state.vscdb")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE ItemTable (key TEXT, value TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO ItemTable (key, value) VALUES ('cursorAuth/accessToken', ?)`, secret); err != nil {
		t.Fatal(err)
	}
	db.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "GetFilteredUsageEvents"):
			var body struct {
				Page int `json:"page"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Page > 1 {
				fmtJSON(w, map[string]any{"totalUsageEventsCount": 1, "usageEventsDisplay": []any{}})
				return
			}
			fmtJSON(w, map[string]any{"totalUsageEventsCount": 1, "usageEventsDisplay": []any{
				map[string]any{"conversationId": "s", "model": "m", "timestamp": "1000", "chargedCents": 1, "tokenUsage": map[string]any{"inputTokens": 1, "outputTokens": 1}},
			}})
		default:
			fmtJSON(w, map[string]any{"planInfo": map[string]any{"planName": "Pro"}, "planUsage": map[string]any{"includedSpend": 1, "limit": 2}})
		}
	}))
	defer srv.Close()
	oldBase, oldDo, oldDB := apiBase, httpDo, stateDBOverride
	apiBase, httpDo, stateDBOverride = srv.URL+"/", srv.Client(), dbPath
	t.Cleanup(func() { apiBase, httpDo, stateDBOverride = oldBase, oldDo, oldDB })

	var lines []string
	trace := func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
	dir := t.TempDir()
	if _, err := RefreshOption(context.Background(), dir, Option{Trace: trace}); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(lines, "\n")
	if strings.Contains(got, secret) || !strings.Contains(got, "GetCurrentPeriodUsage") || !strings.Contains(got, "-> 200") {
		t.Fatalf("trace:\n%s", got)
	}
	lines = nil
	cached, err := RefreshOption(context.Background(), dir, Option{Trace: trace})
	if err != nil || !cached.FromCache || !strings.Contains(strings.Join(lines, "\n"), "no request") {
		t.Fatalf("cache %+v %v %q", cached, err, lines)
	}
	lines = nil
	forced, err := RefreshOption(context.Background(), dir, Option{Force: true, Trace: trace})
	if err != nil || forced.FromCache {
		t.Fatalf("force %+v %v", forced, err)
	}
	got = strings.Join(lines, "\n")
	if !strings.Contains(got, "fetching again") || !strings.Contains(got, "POST ") || strings.Contains(got, secret) {
		t.Fatalf("force trace:\n%s", got)
	}
}

func TestFetchPeriodSkipsEvents(t *testing.T) {
	const secret = "period-token"
	dbPath := filepath.Join(t.TempDir(), "state.vscdb")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE ItemTable (key TEXT, value TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO ItemTable (key, value) VALUES ('cursorAuth/accessToken', ?)`, secret); err != nil {
		t.Fatal(err)
	}
	db.Close()
	var events int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "GetFilteredUsageEvents") {
			events++
		}
		fmtJSON(w, map[string]any{
			"planInfo":        map[string]any{"planName": "Team"},
			"planUsage":       map[string]any{"includedSpend": 2000, "limit": 2000, "apiPercentUsed": 100, "totalPercentUsed": 37.5},
			"displayMessage":  "ok",
			"billingCycleEnd": "1893456000000",
		})
	}))
	defer srv.Close()
	oldBase, oldDo, oldDB := apiBase, httpDo, stateDBOverride
	apiBase, httpDo, stateDBOverride = srv.URL+"/", srv.Client(), dbPath
	t.Cleanup(func() { apiBase, httpDo, stateDBOverride = oldBase, oldDo, oldDB })
	var lines []string
	var bodies []string
	p, err := FetchPeriod(context.Background(), Option{
		Trace: func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) },
		Body:  func(format string, args ...any) { bodies = append(bodies, fmt.Sprintf(format, args...)) },
	})
	if err != nil || p == nil || p.Plan != "Team" || events != 0 {
		t.Fatalf("period %+v err %v events %d", p, err, events)
	}
	got := strings.Join(lines, "\n")
	if strings.Contains(got, secret) || strings.Contains(got, "GetFilteredUsageEvents") || !strings.Contains(got, "GetCurrentPeriodUsage") {
		t.Fatalf("trace:\n%s", got)
	}
	body := strings.Join(bodies, "\n")
	if strings.Contains(body, secret) || !strings.Contains(body, "request GetCurrentPeriodUsage") || !strings.Contains(body, `"planName": "Team"`) {
		t.Fatalf("body:\n%s", body)
	}
}

func TestBetweenUsesEventTime(t *testing.T) {
	u := &Use{Models: map[string]*Model{}}
	early := ev("c", "gpt", 100, 10, 1, 0, 0)
	early.Timestamp = 1_000
	late := ev("c", "gpt", 200, 20, 2, 0, 0)
	late.Timestamp = 5_000
	addEvent(u, early)
	addEvent(u, late)
	day := u.Between(time.UnixMilli(4_000), time.UnixMilli(6_000))
	if day.Events != 1 || day.Input != 20 || day.USD < 1.99 || day.USD > 2.01 {
		t.Fatalf("day %+v", day)
	}
	none := u.Between(time.UnixMilli(10_000), time.Time{})
	if none.Events != 0 {
		t.Fatalf("late range %+v", none)
	}
}

func TestIncludedZeroStaysZero(t *testing.T) {
	p := fillPeriod(0, 500, 2000, 0, 0)
	if p.UsedUSD != 0 || p.BonusUSD != 5 || p.Percent != 0 || p.LimitUSD != 20 {
		t.Fatalf("%+v", p)
	}
	full := fillPeriod(1288, 0, 2000, 2.64, 3.73)
	if full.UsedUSD < 12.87 || full.UsedUSD > 12.89 || full.Percent < 64.3 || full.Percent > 64.5 || full.APIPercent != 2.64 || full.TotalPercent != 3.73 {
		t.Fatalf("%+v", full)
	}
}

func fmtJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func loginDB(t *testing.T, token string) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "state.vscdb")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE ItemTable (key TEXT, value TEXT)`); err != nil {
		t.Fatal(err)
	}
	if token != "" {
		if _, err := db.Exec(`INSERT INTO ItemTable (key, value) VALUES ('cursorAuth/accessToken', ?)`, token); err != nil {
			t.Fatal(err)
		}
	}
	return dbPath
}

// A failed events read keeps the complete event list the cache had, takes
// the new billing cycle, and is not retried by the next command.
func TestFailedEventsKeepCachedSessions(t *testing.T) {
	dir := t.TempDir()
	old := &Ledger{Version: ledgerVersion, FetchedAt: time.Now().Add(-time.Hour), EventCount: 1,
		Sessions: map[string]*Use{"conv-1": {USD: 1.5, Events: 1}}}
	if err := save(dir, old); err != nil {
		t.Fatal(err)
	}
	var asked atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		if strings.HasSuffix(r.URL.Path, "GetFilteredUsageEvents") {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		fmtJSON(w, map[string]any{"planUsage": map[string]any{"includedSpend": 100, "limit": 2000}})
	}))
	defer srv.Close()
	oldBase, oldDo, oldDB := apiBase, httpDo, stateDBOverride
	apiBase, httpDo, stateDBOverride = srv.URL+"/", srv.Client(), loginDB(t, "tok")
	t.Cleanup(func() { apiBase, httpDo, stateDBOverride = oldBase, oldDo, oldDB })

	led, err := Refresh(context.Background(), dir)
	if err == nil {
		t.Fatal("the failed events read was not reported")
	}
	if led.Sessions["conv-1"] == nil || led.Sessions["conv-1"].USD != 1.5 || led.Period == nil {
		t.Fatalf("ledger after a failed events read: %+v", led)
	}
	if saved := Load(dir); saved.Sessions["conv-1"] == nil || saved.FailedAt.IsZero() {
		t.Fatalf("saved %+v", saved)
	}
	n := asked.Load()
	if !Fresh(dir) {
		t.Fatal("a failure a moment ago should not be retried yet")
	}
	if _, err := Refresh(context.Background(), dir); err != nil || asked.Load() != n {
		t.Fatalf("second refresh: err %v, %d more requests", err, asked.Load()-n)
	}
}

// With no IDE login nothing is asked, the error says so, and the next
// command does not try again at once.
func TestNoLoginIsRememberedQuietly(t *testing.T) {
	dir := t.TempDir()
	oldDB := stateDBOverride
	stateDBOverride = loginDB(t, "")
	t.Cleanup(func() { stateDBOverride = oldDB })
	asked := false
	_, err := RefreshOption(context.Background(), dir, Option{Asking: func() { asked = true }})
	if !errors.Is(err, ErrNoLogin) || asked {
		t.Fatalf("err %v, asked %t", err, asked)
	}
	if !Fresh(dir) {
		t.Fatal("no login was not remembered")
	}
}

// The token goes to the API host only: a redirect is not followed.
func TestRedirectIsNotFollowed(t *testing.T) {
	var followed bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			followed = true
			return
		}
		http.Redirect(w, r, "/elsewhere", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	oldBase, oldDB := apiBase, stateDBOverride
	apiBase, stateDBOverride = srv.URL+"/", loginDB(t, "tok")
	t.Cleanup(func() { apiBase, stateDBOverride = oldBase, oldDB })
	if _, err := FetchPeriod(context.Background(), Option{}); err == nil || followed {
		t.Fatalf("err %v, followed %t", err, followed)
	}
}

func TestLoadIgnoresAnOlderLedger(t *testing.T) {
	dir := t.TempDir()
	if err := save(dir, &Ledger{Version: ledgerVersion - 1, Sessions: map[string]*Use{"c": {USD: 1, Events: 1}}}); err != nil {
		t.Fatal(err)
	}
	if l := Load(dir); len(l.Sessions) != 0 {
		t.Fatalf("an older ledger was used: %+v", l)
	}
}

// eventServer answers GetFilteredUsageEvents from a list, newest first, the
// way the service pages it, and records each request's window and page size.
type eventServer struct {
	mu     sync.Mutex
	events []map[string]any
	starts []int64
	sizes  []int
}

func (s *eventServer) handler(w http.ResponseWriter, r *http.Request) {
	if !strings.HasSuffix(r.URL.Path, "GetFilteredUsageEvents") {
		fmtJSON(w, map[string]any{"planUsage": map[string]any{"includedSpend": 1, "limit": 2}})
		return
	}
	var body struct {
		Start    string `json:"startDate"`
		Page     int    `json:"page"`
		PageSize int    `json:"pageSize"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	start, _ := strconv.ParseInt(body.Start, 10, 64)
	s.mu.Lock()
	s.starts, s.sizes = append(s.starts, start), append(s.sizes, body.PageSize)
	var in []map[string]any
	for _, e := range s.events {
		if ts, _ := strconv.ParseInt(e["timestamp"].(string), 10, 64); ts >= start {
			in = append(in, e)
		}
	}
	s.mu.Unlock()
	lo := min((body.Page-1)*body.PageSize, len(in))
	hi := min(lo+body.PageSize, len(in))
	fmtJSON(w, map[string]any{"totalUsageEventsCount": len(in), "usageEventsDisplay": in[lo:hi]})
}

func usageEvent(conv string, at time.Time) map[string]any {
	return map[string]any{"conversationId": conv, "model": "m", "timestamp": strconv.FormatInt(at.UnixMilli(), 10),
		"chargedCents": 100, "tokenUsage": map[string]any{"inputTokens": 1, "outputTokens": 1}}
}

func serveEvents(t *testing.T, s *eventServer) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(s.handler))
	t.Cleanup(srv.Close)
	oldBase, oldDo, oldDB := apiBase, httpDo, stateDBOverride
	apiBase, httpDo, stateDBOverride = srv.URL+"/", srv.Client(), loginDB(t, "tok")
	t.Cleanup(func() { apiBase, httpDo, stateDBOverride = oldBase, oldDo, oldDB })
}

// A year of events comes in pages of 1000, the pages after the first fetched
// at once, and every event is counted once.
func TestEventsComeInLargePages(t *testing.T) {
	s := &eventServer{}
	now := time.Now()
	for i := range 2500 {
		s.events = append(s.events, usageEvent(fmt.Sprintf("conv-%d", i%7), now.Add(-time.Duration(i)*time.Minute)))
	}
	serveEvents(t, s)
	led, err := Refresh(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, u := range led.Sessions {
		n += u.Events
	}
	if n != 2500 || led.EventCount != 2500 || led.FullAt.IsZero() {
		t.Fatalf("counted %d events (EventCount %d, full at %v), want 2500", n, led.EventCount, led.FullAt)
	}
	if len(s.sizes) != 3 || s.sizes[0] != 1000 {
		t.Fatalf("event requests: page sizes %v, want 3 pages of 1000", s.sizes)
	}
}

// After a full read, the next read asks only from 48 hours before the newest
// cached event, and the events it gets again replace the cached ones.
func TestEventsReadOnlySinceTheCache(t *testing.T) {
	s := &eventServer{}
	now := time.Now()
	old, recent := now.Add(-10*24*time.Hour), now.Add(-time.Hour)
	s.events = []map[string]any{usageEvent("a", recent), usageEvent("a", old), usageEvent("b", old)}
	serveEvents(t, s)
	dir := t.TempDir()
	if _, err := Refresh(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.events = append([]map[string]any{usageEvent("a", now.Add(-time.Minute))}, s.events...)
	s.starts = nil
	s.mu.Unlock()
	led, err := RefreshOption(context.Background(), dir, Option{Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if want := recent.Add(-overlap).UnixMilli(); len(s.starts) != 1 || s.starts[0] != want {
		t.Fatalf("second read started at %v, want %d", s.starts, want)
	}
	if a, b := led.Sessions["a"], led.Sessions["b"]; a == nil || a.Events != 3 || b == nil || b.Events != 1 || led.EventCount != 4 {
		t.Fatalf("after the incremental read: a %+v, b %+v, count %d", a, b, led.EventCount)
	}
	// A week after the last full read, the next read is full again.
	led.FullAt = now.Add(-fullEvery - time.Minute).UTC()
	led.FetchedAt = time.Time{}
	if err := save(dir, led); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.starts = nil
	s.mu.Unlock()
	if _, err := Refresh(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if len(s.starts) != 1 || s.starts[0] > now.AddDate(-1, 0, 1).UnixMilli() {
		t.Fatalf("read after a week started at %v, want a year back", s.starts)
	}
}
