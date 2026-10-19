package cursorusage

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	if got.Primary() != "gpt" {
		t.Fatalf("primary %q", got.Primary())
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
	if p == nil || p.Plan != "Pro" || p.UsedUSD != 20 || p.LimitUSD != 20 || p.APIPercent != 100 || p.Percent != 33.57 || p.BonusUSD < 399.62 || p.BonusUSD > 399.64 {
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

func fmtJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
