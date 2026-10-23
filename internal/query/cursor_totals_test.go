package query_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lr00rl/ccli/internal/account"
	"github.com/lr00rl/ccli/internal/cursorusage"
	"github.com/lr00rl/ccli/internal/index"
	"github.com/lr00rl/ccli/internal/pricing"
	"github.com/lr00rl/ccli/internal/query"
	"github.com/lr00rl/ccli/internal/store"
)

// The session list, account totals and the dashboard add billed Cursor IDE
// usage under one rule, so their figures agree: a composer ccli could price
// keeps its own cost, and one without token counts takes the billed tokens
// and cost.
func TestCursorIDEBilledTotalsAgree(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	user := filepath.Join(root, "Cursor", "User")
	const priced = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	const bare = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	now := time.Now().Add(-time.Hour).UnixMilli()
	exec := ideDB(t, user, now, priced, bare)
	exec(`INSERT INTO cursorDiskKV VALUES (?, ?)`, "bubbleId:"+priced+":r",
		`{"type":2,"bubbleId":"r","text":"done","createdAt":`+itoa(now+500)+`,"tokenCount":{"inputTokens":1000,"outputTokens":100},"modelInfo":{"modelName":"claude-opus-5-5"}}`)
	exec(`INSERT INTO cursorDiskKV VALUES (?, ?)`, "bubbleId:"+bare+":r", `{"type":2,"bubbleId":"r","text":"done","createdAt":`+itoa(now+500)+`}`)
	writeLedger(t, map[string]*cursorusage.Use{
		priced: {Input: 1000, Output: 100, USD: 2, Events: 1, Hits: []cursorusage.Hit{{At: now + 500, Input: 1000, Output: 100, USD: 2}}},
		bare:   {Input: 500, Output: 50, USD: 3, Events: 1, Hits: []cursorusage.Hit{{At: now + 500, Input: 500, Output: 50, USD: 3}}},
	})
	accts := []account.Account{{Name: "cursor-ide", Tool: account.CursorIDE, Dir: user}}
	st := indexed(t, root, accts)
	ctx := context.Background()
	f := query.Filter{Since: time.Now().Add(-24 * time.Hour)}

	page, err := query.New(st, accts).Sessions(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	var listUSD float64
	var listTokens int64
	for _, s := range page.Sessions {
		switch s.ID {
		case priced:
			if s.Cost.USD <= 0 || s.Cost.USD == 2 || s.Tokens.Input != 1000 {
				t.Fatalf("priced composer: tokens %+v cost %+v, want its own priced figures", s.Tokens, s.Cost)
			}
		case bare:
			if s.Cost.USD != 3 || s.Tokens.Input != 500 {
				t.Fatalf("bare composer: tokens %+v cost %+v, want the billed figures", s.Tokens, s.Cost)
			}
		}
		listUSD += s.Cost.USD
		listTokens += s.Tokens.All()
	}

	accounts, err := query.New(st, accts).Accounts(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 || !near(accounts[0].Cost.USD, listUSD) || accounts[0].Tokens.All() != listTokens {
		t.Fatalf("account total %+v %+v, sessions add to %d tokens $%.4f", accounts[0].Tokens, accounts[0].Cost, listTokens, listUSD)
	}
	o, err := query.New(st, accts).Overview(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	if !near(o.Cost.USD, listUSD) || o.Tokens.All() != listTokens {
		t.Fatalf("dashboard %+v %+v, sessions add to %d tokens $%.4f", o.Tokens, o.Cost, listTokens, listUSD)
	}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

// An IDE agent also writes its transcript into ~/.cursor/projects, so the
// composer turns up in the Cursor CLI account too. The conversation is
// billed once, on the IDE row, and the CLI row is marked as the IDE's copy.
func TestCursorIDECopyBilledOnce(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	user := filepath.Join(root, "Cursor", "User")
	const shared = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	const cliOnly = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	now := time.Now().Add(-time.Hour).UnixMilli()
	exec := ideDB(t, user, now, shared)
	exec(`INSERT INTO cursorDiskKV VALUES (?, ?)`, "bubbleId:"+shared+":r", `{"type":2,"bubbleId":"r","text":"done","createdAt":`+itoa(now+500)+`}`)
	home := filepath.Join(root, ".cursor")
	for _, id := range []string{shared, cliOnly} {
		p := filepath.Join(home, "projects", "work-app", "agent-transcripts", id, id+".jsonl")
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		line := `{"role":"user","message":{"content":[{"type":"text","text":"<user_query>\nask\n</user_query>"}]}}` + "\n" +
			`{"role":"assistant","message":{"content":[{"type":"text","text":"done"}]}}` + "\n"
		if err := os.WriteFile(p, []byte(line), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	hit := func(usd float64) *cursorusage.Use {
		return &cursorusage.Use{Input: 500, Output: 50, USD: usd, Events: 1, Hits: []cursorusage.Hit{{At: now + 500, Input: 500, Output: 50, USD: usd}}}
	}
	writeLedger(t, map[string]*cursorusage.Use{shared: hit(3), cliOnly: hit(5)})
	accts := []account.Account{
		{Name: "cursor", Tool: account.Cursor, Dir: home},
		{Name: "cursor-ide", Tool: account.CursorIDE, Dir: user},
	}
	st := indexed(t, root, accts)
	ctx := context.Background()
	f := query.Filter{Since: time.Now().Add(-24 * time.Hour)}

	page, err := query.New(st, accts).Sessions(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]query.SessionRow{}
	for _, s := range page.Sessions {
		got[s.Account+":"+s.ID] = s
	}
	if s := got["cursor-ide:"+shared]; s.Cost.USD != 3 || s.CopiedFrom != "" {
		t.Fatalf("IDE row: cost %+v copied from %q, want $3 and no copy mark", s.Cost, s.CopiedFrom)
	}
	if s := got["cursor:"+shared]; s.Cost.USD != 0 || s.Tokens.All() != 0 || s.CopiedFrom != "cursor-ide" {
		t.Fatalf("CLI copy: tokens %+v cost %+v copied from %q, want nothing billed and the IDE named", s.Tokens, s.Cost, s.CopiedFrom)
	}
	if s := got["cursor:"+cliOnly]; s.Cost.USD != 5 || s.CopiedFrom != "" {
		t.Fatalf("CLI session: cost %+v copied from %q, want $5", s.Cost, s.CopiedFrom)
	}
	if !near(page.Cost.USD, 8) {
		t.Fatalf("list total $%.2f, want $8", page.Cost.USD)
	}

	accounts, err := query.New(st, accts).Accounts(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range accounts {
		if want := map[string]float64{"cursor": 5, "cursor-ide": 3}[a.Name]; !near(a.Cost.USD, want) {
			t.Fatalf("account %s: $%.2f, want $%.2f", a.Name, a.Cost.USD, want)
		}
	}
	o, err := query.New(st, accts).Overview(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	if !near(o.Cost.USD, 8) {
		t.Fatalf("dashboard $%.2f, want $8", o.Cost.USD)
	}
	det, err := query.New(st, accts).Session(ctx, "cursor:"+shared)
	if err != nil {
		t.Fatal(err)
	}
	if det.Cost.USD != 0 || len(det.ByModel) != 0 || det.CopiedFrom != "cursor-ide" {
		t.Fatalf("CLI copy detail: cost %+v models %+v copied from %q", det.Cost, det.ByModel, det.CopiedFrom)
	}
}

// ideDB writes a Cursor IDE database with one composer, and its prompt, per
// id, and returns a way to add rows.
func ideDB(t *testing.T, user string, now int64, ids ...string) func(string, ...any) {
	t.Helper()
	dbPath := filepath.Join(user, "globalStorage", "state.vscdb")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`CREATE TABLE composerHeaders (composerId TEXT PRIMARY KEY, createdAt INTEGER, lastUpdatedAt INTEGER, isSubagent INTEGER, value TEXT)`)
	exec(`CREATE TABLE cursorDiskKV (key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB)`)
	for _, id := range ids {
		exec(`INSERT INTO composerHeaders VALUES (?, ?, ?, 0, '{"name":"chat"}')`, id, now, now+1000)
		exec(`INSERT INTO cursorDiskKV VALUES (?, ?)`, "bubbleId:"+id+":u", `{"type":1,"bubbleId":"u","text":"ask","createdAt":`+itoa(now)+`}`)
	}
	return exec
}

func writeLedger(t *testing.T, sessions map[string]*cursorusage.Use) {
	t.Helper()
	b, _ := json.Marshal(cursorusage.Ledger{Version: 2, FetchedAt: time.Now(), Sessions: sessions})
	if err := os.MkdirAll(store.DataDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.DataDir(), "cursor-usage.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func indexed(t *testing.T, root string, accts []account.Account) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(root, "ccli.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	prices, _ := pricing.Load("")
	if _, err := index.Run(context.Background(), st, accts, prices, index.Options{}); err != nil {
		t.Fatal(err)
	}
	return st
}
