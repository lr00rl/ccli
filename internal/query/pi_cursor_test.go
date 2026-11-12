package query_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lr00rl/ccli/internal/account"
	"github.com/lr00rl/ccli/internal/cursorusage"
	"github.com/lr00rl/ccli/internal/query"
)

// A pi session on Cursor records tokens and no price. Its row, its account
// and the dashboard take what Cursor billed for the conversations the
// transcripts name, its subagent run's included, and agree with each other.
func TestPiCursorSessionsTakeTheBilledCost(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	t.Setenv("CURSOR_BRIDGE_SESSIONS", "off")
	home := filepath.Join(root, "pi-agent")
	dir := filepath.Join(home, "sessions", "--work-app--")
	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	const (
		parentID = "01a00000-0000-7000-8000-00000000p001"
		childID  = "01a00000-0000-7000-8000-00000000p002"
		runID    = "aaaaaaaa-0000-4000-8000-000000000001"
		// conversationID(session, {role:"user",content:"fix <a> & b\u2028next"}),
		// as the plugin makes it; pinned in internal/pi too.
		parentConv = "aef6f1ec-9621-4403-a7df-0e7368302846"
	)
	now := time.Now().Add(-time.Hour)
	ts := func(d time.Duration) string { return now.Add(d).UTC().Format(time.RFC3339Nano) }
	ms := func(d time.Duration) string { return itoa(now.Add(d).UnixMilli()) }
	reply := func(id, parent string, d time.Duration) string {
		return `{"type":"message","id":"` + id + `","parentId":"` + parent + `","timestamp":"` + ts(d) + `","message":{"role":"assistant","content":[{"type":"text","text":"ok"}],"provider":"cursor","model":"claude-opus-5-5@300k","usage":{"input":100,"output":10,"cacheRead":0,"cacheWrite":0,"totalTokens":110,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}},"timestamp":` + ms(d) + `}}` + "\n"
	}
	user := func(id, content string, d time.Duration) string {
		return `{"type":"message","id":"` + id + `","parentId":null,"timestamp":"` + ts(d) + `","message":{"role":"user","content":` + content + `,"timestamp":` + ms(d) + `}}` + "\n"
	}
	parentFile := filepath.Join(dir, "2026-08-01T00-00-00-000Z_"+parentID+".jsonl")
	write(parentFile, `{"type":"session","version":3,"id":"`+parentID+`","timestamp":"`+ts(0)+`","cwd":"/work/app"}`+"\n"+
		user("u1", `"fix <a> & b\u2028next"`, time.Second)+reply("a1", "u1", 2*time.Second))
	write(filepath.Join(dir, "2026-08-01T00-00-00-000Z_"+parentID, runID, "run-0", "session.jsonl"),
		`{"type":"session","version":3,"id":"`+childID+`","timestamp":"`+ts(time.Minute)+`","cwd":"/work/app","parentSession":"`+parentFile+`"}`+"\n"+
			user("c1", `"look up the numbers"`, time.Minute)+reply("c2", "c1", time.Minute+time.Second))

	accts := []account.Account{{Name: "pi", Tool: account.Pi, Dir: home}}
	st := indexed(t, root, accts)
	ctx := context.Background()

	// The child's id comes from the index, so the test does not restate the
	// hash: the parent row's billed cost must include it.
	at := now.Add(2 * time.Second).UnixMilli()
	ledger := map[string]*cursorusage.Use{
		parentConv: {Input: 100, Output: 10, USD: 0.5, Events: 1, Hits: []cursorusage.Hit{{At: at, Input: 100, Output: 10, USD: 0.5, Model: "claude-opus-5-5"}}},
	}
	childConv := childConversation(t, root)
	ledger[childConv] = &cursorusage.Use{Input: 100, Output: 10, USD: 0.25, Events: 1, Hits: []cursorusage.Hit{{At: at + 60_000, Input: 100, Output: 10, USD: 0.25, Model: "claude-opus-5-5"}}}
	writeLedger(t, ledger)

	q := query.New(st, accts)

	f := query.Filter{Since: time.Now().Add(-24 * time.Hour)}
	page, err := q.Sessions(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Sessions) != 1 {
		t.Fatalf("sessions %+v, want the parent only", page.Sessions)
	}
	s := page.Sessions[0]
	if !near(s.Cost.USD, 0.75) || s.Cost.Unpriced != 0 || s.Tokens.Output != 20 || s.Subagents != 1 {
		t.Fatalf("parent row tokens %+v cost %+v subagents %d, want $0.75 billed for both conversations", s.Tokens, s.Cost, s.Subagents)
	}
	accounts, err := q.Accounts(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 || !near(accounts[0].Cost.USD, 0.75) || accounts[0].Cost.Unpriced != 0 {
		t.Fatalf("account %+v", accounts)
	}
	o, err := q.Overview(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	if !near(o.Cost.USD, 0.75) || o.Cost.Unpriced != 0 {
		t.Fatalf("dashboard cost %+v", o.Cost)
	}
}

// childConversation reads the subagent run's conversation id from the index.
func childConversation(t *testing.T, root string) string {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(root, "ccli.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var conv string
	if err := db.QueryRow(`SELECT l.target FROM links l JOIN files f ON f.id = l.file_id
		WHERE l.kind = 'cursor-conversation' AND f.kind = 'subagent'`).Scan(&conv); err != nil {
		t.Fatal(err)
	}
	return conv
}
