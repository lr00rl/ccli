// Package cursorusage reads Cursor's dashboard usage for the signed-in
// account and joins it to local CLI and IDE sessions by conversation id.
// The access token Cursor already stored is sent only to api2.cursor.sh.
// It is never written to the cache or included in an error.
package cursorusage

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lr00rl/ccli/internal/cursoride"

	_ "modernc.org/sqlite"
)

const (
	maxPages = 40
	pageSize = 100
	freshFor = 15 * time.Minute
)

var (
	refreshMu sync.Mutex
	apiBase   = "https://api2.cursor.sh/aiserver.v1.DashboardService/"
	httpDo    = http.DefaultClient
	// stateDBOverride is the state.vscdb tests point at. Empty uses the IDE's.
	stateDBOverride string
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

// Primary is the model with the highest billed amount.
func (u *Use) Primary() string {
	best, bestUSD := "", -1.0
	for name, m := range u.Models {
		if best == "" || m.USD > bestUSD {
			best, bestUSD = name, m.USD
		}
	}
	return best
}

// Period is the account's current billing cycle. Amounts are US dollars.
// UsedUSD is included spend against LimitUSD. BonusUSD is free usage the
// plan adds on top, so it is not spend against that limit. Percent is the
// included total. APIPercent is the named-model allowance.
type Period struct {
	Plan       string    `json:"plan,omitempty"`
	UsedUSD    float64   `json:"used_usd"`
	LimitUSD   float64   `json:"limit_usd"`
	BonusUSD   float64   `json:"bonus_usd,omitempty"`
	Percent    float64   `json:"percent"`
	APIPercent float64   `json:"api_percent,omitempty"`
	Message    string    `json:"message,omitempty"`
	CycleEnd   time.Time `json:"cycle_end,omitzero"`
}

// Ledger is a cached dashboard read.
type Ledger struct {
	FetchedAt time.Time       `json:"fetched_at"`
	Sessions  map[string]*Use `json:"sessions,omitempty"`
	Period    *Period         `json:"period,omitempty"`
	Error     string          `json:"error,omitempty"`
}

// Load reads the cache. A missing file is an empty ledger, not an error.
func Load(dataDir string) *Ledger {
	b, err := os.ReadFile(filepath.Join(dataDir, "cursor-usage.json"))
	if err != nil {
		return &Ledger{}
	}
	var l Ledger
	if json.Unmarshal(b, &l) != nil {
		return &Ledger{}
	}
	if l.Sessions == nil {
		l.Sessions = map[string]*Use{}
	}
	return &l
}

// Refresh fetches the dashboard when the cache is older than freshFor.
// A network or auth failure keeps the previous cache. Callers can still
// use a partial ledger when Period or Sessions is set.
func Refresh(ctx context.Context, dataDir string) (*Ledger, error) {
	refreshMu.Lock()
	defer refreshMu.Unlock()
	cur := Load(dataDir)
	if cur.Error == "" && !cur.FetchedAt.IsZero() && time.Since(cur.FetchedAt) < freshFor && (len(cur.Sessions) > 0 || cur.Period != nil) {
		return cur, nil
	}
	next, err := fetch(ctx)
	if next != nil && (len(next.Sessions) > 0 || next.Period != nil) {
		if err != nil {
			next.Error = err.Error()
		}
		if saveErr := save(dataDir, next); saveErr != nil && err == nil {
			err = saveErr
		}
		return next, err
	}
	if err != nil {
		if cur.FetchedAt.IsZero() {
			cur.Error = err.Error()
		}
		return cur, err
	}
	if err := save(dataDir, next); err != nil {
		return next, err
	}
	return next, nil
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
	path := filepath.Join(dataDir, "cursor-usage.json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func fetch(ctx context.Context) (*Ledger, error) {
	token, err := accessToken()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	led := &Ledger{FetchedAt: time.Now().UTC(), Sessions: map[string]*Use{}}
	var soft error
	if p, err := period(ctx, token); err == nil {
		led.Period = p
	} else {
		soft = err
	}
	end := time.Now()
	start := end.AddDate(-1, 0, 0)
	ev, evErr := events(ctx, token, start, end)
	for _, e := range ev {
		id := e.ConversationID
		if id == "" {
			continue
		}
		u := led.Sessions[id]
		if u == nil {
			u = &Use{Models: map[string]*Model{}}
			led.Sessions[id] = u
		}
		addEvent(u, e)
	}
	if evErr != nil {
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

func addEvent(u *Use, e event) {
	in := int64(e.TokenUsage.Input)
	out := int64(e.TokenUsage.Output)
	cr := int64(e.TokenUsage.CacheRead)
	cw := int64(e.TokenUsage.CacheWrite)
	usd := float64(e.ChargedCents)
	if usd == 0 {
		usd = float64(e.TokenUsage.TotalCents)
	}
	usd /= 100
	u.Input += in
	u.Output += out
	u.CacheRead += cr
	u.CacheWrite += cw
	u.USD += usd
	u.Events++
	name := e.Model
	if name == "" {
		name = "(unknown)"
	}
	if u.Models == nil {
		u.Models = map[string]*Model{}
	}
	m := u.Models[name]
	if m == nil {
		m = &Model{}
		u.Models[name] = m
	}
	m.Input += in
	m.Output += out
	m.CacheRead += cr
	m.CacheWrite += cw
	m.USD += usd
	m.N++
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
	ChargedCents   flexF  `json:"chargedCents"`
	TokenUsage     struct {
		Input      flexI `json:"inputTokens"`
		Output     flexI `json:"outputTokens"`
		CacheRead  flexI `json:"cacheReadTokens"`
		CacheWrite flexI `json:"cacheWriteTokens"`
		TotalCents flexF `json:"totalCents"`
	} `json:"tokenUsage"`
}

func events(ctx context.Context, token string, start, end time.Time) ([]event, error) {
	var all []event
	for page := 1; page <= maxPages; page++ {
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
			return all, err
		}
		batch := resp.UsageEventsDisplay
		if len(batch) == 0 {
			batch = resp.UsageEvents
		}
		if len(batch) == 0 {
			break
		}
		all = append(all, batch...)
		if resp.Total > 0 && len(all) >= resp.Total {
			break
		}
	}
	return all, nil
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
	if err := post(ctx, token, "GetCurrentPeriodUsage", map[string]any{}, &usage); err != nil {
		return nil, err
	}
	var plan struct {
		PlanInfo struct {
			PlanName string `json:"planName"`
		} `json:"planInfo"`
	}
	_ = post(ctx, token, "GetPlanInfo", map[string]any{}, &plan)
	included := float64(usage.PlanUsage.IncludedSpend)
	if included == 0 {
		included = float64(usage.PlanUsage.TotalSpend)
	}
	p := &Period{
		Plan:       plan.PlanInfo.PlanName,
		UsedUSD:    included / 100,
		LimitUSD:   float64(usage.PlanUsage.Limit) / 100,
		BonusUSD:   float64(usage.PlanUsage.BonusSpend) / 100,
		Percent:    float64(usage.PlanUsage.TotalPercentUsed),
		APIPercent: float64(usage.PlanUsage.APIPercentUsed),
		Message:    usage.DisplayMessage,
	}
	if p.Percent == 0 && p.LimitUSD > 0 {
		p.Percent = p.UsedUSD / p.LimitUSD * 100
	}
	if ms := int64(usage.BillingCycleEnd); ms > 0 {
		p.CycleEnd = time.UnixMilli(ms)
	}
	return p, nil
}

func post(ctx context.Context, token, method string, body, dest any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiBase+method, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")
	resp, err := httpDo.Do(req)
	if err != nil {
		return fmt.Errorf("cursor usage: %s", method)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("cursor usage: %s returned %d", method, resp.StatusCode)
	}
	if dest == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, dest); err != nil {
		return fmt.Errorf("cursor usage: %s", method)
	}
	return nil
}

// accessToken is the IDE's stored login. The value is not returned in errors.
func accessToken() (string, error) {
	dbPath, err := stateDB()
	if err != nil {
		return "", err
	}
	u := url.URL{Scheme: "file", Path: dbPath, RawQuery: "mode=ro&immutable=1"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return "", fmt.Errorf("cursor usage: no local login")
	}
	defer db.Close()
	var raw []byte
	if err := db.QueryRow(`SELECT value FROM ItemTable WHERE key = 'cursorAuth/accessToken'`).Scan(&raw); err != nil || len(raw) == 0 {
		return "", fmt.Errorf("cursor usage: no local login")
	}
	tok := strings.TrimSpace(string(raw))
	if strings.HasPrefix(tok, `"`) {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			tok = s
		}
	}
	if tok == "" {
		return "", fmt.Errorf("cursor usage: no local login")
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
		return "", fmt.Errorf("cursor usage: no local login")
	}
	p := cursoride.DBPath(cursorUser(home))
	if st, err := os.Stat(p); err != nil || !st.Mode().IsRegular() {
		return "", fmt.Errorf("cursor usage: no local login")
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
