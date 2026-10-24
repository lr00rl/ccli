package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lr00rl/ccli/internal/cursorusage"
	"github.com/lr00rl/ccli/internal/store"
)

func TestMain(m *testing.M) {
	// Main must never start the test binary again as a background update.
	startRefresh = func() error { return nil }
	os.Exit(m.Run())
}

// cursorHome adds a Cursor CLI account with one chat to cleanHome, and a
// cache of its usage fetched at the given time.
func cursorHome(t *testing.T, fetched time.Time, failure string) string {
	t.Helper()
	root := cleanHome(t)
	const id = "eeeeeeee-0000-4000-8000-000000000001"
	p := filepath.Join(root, ".cursor", "projects", "work-app", "agent-transcripts", id, id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	chat := `{"role":"user","message":{"content":[{"type":"text","text":"<user_query>\nask\n</user_query>"}]}}` + "\n" +
		`{"role":"assistant","message":{"content":[{"type":"text","text":"done"}]}}` + "\n"
	if err := os.WriteFile(p, []byte(chat), 0o600); err != nil {
		t.Fatal(err)
	}
	led := cursorusage.Ledger{Version: 2, FetchedAt: fetched, Error: failure, Sessions: map[string]*cursorusage.Use{
		id: {Input: 500, Output: 50, USD: 2, Events: 1},
	}}
	b, _ := json.Marshal(led)
	if err := os.MkdirAll(store.DataDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.DataDir(), "cursor-usage.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

// A stale cache with usage in it is shown at once, and one update starts
// in the background; none starts while another runs.
func TestStaleCursorUsageUpdatesInTheBackground(t *testing.T) {
	cursorHome(t, time.Now().Add(-time.Hour), "")
	starts := 0
	startRefresh = func() error { starts++; return nil }
	t.Cleanup(func() { startRefresh = func() error { return nil } })

	var page struct {
		Cost struct {
			USD float64 `json:"usd"`
		} `json:"cost"`
	}
	if err := json.Unmarshal(runCLI(t, "sessions", "-a", "cursor", "--json"), &page); err != nil || page.Cost.USD != 2 {
		t.Fatalf("cost $%v (%v), want the cached $2", page.Cost.USD, err)
	}
	if starts != 1 {
		t.Fatalf("updates started: %d, want 1", starts)
	}
	release, ok := cursorusage.Lock(store.DataDir())
	if !ok {
		t.Fatal("lock not taken")
	}
	defer release()
	runCLI(t, "sessions", "-a", "cursor")
	if starts != 1 {
		t.Fatalf("an update started while another ran: %d", starts)
	}
}

// Usage whose last update failed says so, instead of passing for current.
func TestFailedCursorUpdateIsReported(t *testing.T) {
	cursorHome(t, time.Now().Add(-time.Hour), "dashboard said 401")
	var out, errw bytes.Buffer
	if code := Main([]string{"sessions", "-a", "cursor"}, &out, &errw); code != 0 ||
		!strings.Contains(errw.String(), "the last update failed: dashboard said 401") {
		t.Fatalf("exit %d, stderr %q", code, errw.String())
	}
}

// The background command leaves the cache alone while another update holds
// the lock.
func TestCursorRefreshWaitsForTheLock(t *testing.T) {
	fetched := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	cursorHome(t, fetched, "")
	release, ok := cursorusage.Lock(store.DataDir())
	if !ok {
		t.Fatal("lock not taken")
	}
	defer release()
	runCLI(t, cursorRefreshCmd)
	if got := cursorusage.Load(store.DataDir()); !got.FetchedAt.Equal(fetched) || got.Error != "" {
		t.Fatalf("cache changed under another update's lock: fetched %v, error %q", got.FetchedAt, got.Error)
	}
}
