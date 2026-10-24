package index

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/lr00rl/ccli/internal/account"
	"github.com/lr00rl/ccli/internal/cursoride"
	"github.com/lr00rl/ccli/internal/pricing"
	"github.com/lr00rl/ccli/internal/query"
	"github.com/lr00rl/ccli/internal/store"
)

type agentEnv struct {
	t     *testing.T
	accts []account.Account
	st    *store.Store
}

func newAgentEnv(t *testing.T, accts ...account.Account) *agentEnv {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "ccli.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &agentEnv{t: t, accts: accts, st: st}
}

func (e *agentEnv) sessions() map[string]query.SessionRow {
	e.t.Helper()
	prices, _ := pricing.Load("")
	if _, err := Run(context.Background(), e.st, e.accts, prices, Options{}); err != nil {
		e.t.Fatal(err)
	}
	page, err := query.New(e.st, e.accts).Sessions(context.Background(), query.Filter{})
	if err != nil {
		e.t.Fatal(err)
	}
	out := map[string]query.SessionRow{}
	for _, s := range page.Sessions {
		out[s.ID] = s
	}
	return out
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Cursor CLI's side files can change without the transcript changing: a
// chat's store.db may show only later that it is a subagent, and meta.json
// carries the title. Both are picked up on the next run.
func TestCursorSideDataChangeIsRead(t *testing.T) {
	home := filepath.Join(t.TempDir(), "cursor")
	const root = "77777777-7777-4777-8777-777777777777"
	const sub = "88888888-8888-4888-8888-888888888888"
	transcript := func(id, ask string) {
		writeFile(t, filepath.Join(home, "projects", "work-app", "agent-transcripts", id, id+".jsonl"),
			`{"role":"user","message":{"content":[{"type":"text","text":"<user_query>\n`+ask+`\n</user_query>"}]}}`+"\n"+
				`{"role":"assistant","message":{"content":[{"type":"text","text":"done"}]}}`+"\n")
	}
	transcript(root, "fix the parser")
	transcript(sub, "review the diff")
	writeFile(t, filepath.Join(home, "chats", "abc", root, "meta.json"), `{"title":"Parser","cwd":"/work/app","createdAtMs":1000,"updatedAtMs":2000}`)
	writeFile(t, filepath.Join(home, "chats", "abc", sub, "meta.json"), `{"isSubagent":true,"createdAtMs":1500,"updatedAtMs":1800}`)

	e := newAgentEnv(t, account.Account{Name: "cursor", Tool: account.Cursor, Dir: home})
	got := e.sessions()
	if _, ok := got[sub]; !ok || len(got) != 2 {
		t.Fatalf("before store.db: %d sessions, want the subagent on its own", len(got))
	}

	// store.db appears and names the root; the title changes. The transcripts
	// stay as they were.
	sdb, err := sql.Open("sqlite", filepath.Join(home, "chats", "abc", sub, "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sdb.Exec(`CREATE TABLE meta (key TEXT, value TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := sdb.Exec(`INSERT INTO meta (key, value) VALUES ('0', ?)`,
		`{"subagentInfo":{"parentAgentId":"`+root+`","rootParentAgentId":"`+root+`","typeName":"reviewer"}}`); err != nil {
		t.Fatal(err)
	}
	sdb.Close()
	meta := filepath.Join(home, "chats", "abc", root, "meta.json")
	writeFile(t, meta, `{"title":"Parser fix","cwd":"/work/app","createdAtMs":1000,"updatedAtMs":2000}`)
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(meta, later, later); err != nil {
		t.Fatal(err)
	}

	got = e.sessions()
	if _, ok := got[sub]; ok || len(got) != 1 {
		t.Fatalf("after store.db: %d sessions, want the subagent under its root", len(got))
	}
	if r := got[root]; r.Subagents != 1 || r.Title != "Parser fix" {
		t.Fatalf("root row: subagents %d, title %q", r.Subagents, r.Title)
	}

	// A store.db that cannot be read for a moment does not undo that.
	storeDB := filepath.Join(home, "chats", "abc", sub, "store.db")
	writeFile(t, storeDB, "not a database")
	later = later.Add(time.Minute)
	if err := os.Chtimes(storeDB, later, later); err != nil {
		t.Fatal(err)
	}
	got = e.sessions()
	if _, ok := got[sub]; ok || got[root].Subagents != 1 || got[root].Prompts != 1 {
		t.Fatalf("unreadable store.db: %d sessions, root %+v", len(got), got[root])
	}
}

// Cursor writes a reply's bubbles after it last moves the composer's
// lastUpdatedAt, so a new bubble alone must make the composer be read again.
func TestCursorIDEReadsBubblesWithoutHeaderChange(t *testing.T) {
	user := filepath.Join(t.TempDir(), "Cursor", "User")
	dbPath := filepath.Join(user, "globalStorage", "state.vscdb")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	const id = "99999999-9999-4999-8999-999999999999"
	exec(`CREATE TABLE composerHeaders (composerId TEXT PRIMARY KEY, createdAt INTEGER, lastUpdatedAt INTEGER, isSubagent INTEGER, value TEXT)`)
	exec(`CREATE TABLE cursorDiskKV (key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB)`)
	exec(`INSERT INTO composerHeaders VALUES (?, 1700000000000, 1700000001000, 0, '{"name":"chat"}')`, id)
	exec(`INSERT INTO cursorDiskKV VALUES (?, ?)`, "bubbleId:"+id+":b1", `{"type":1,"bubbleId":"b1","text":"explain","createdAt":1700000000500}`)
	db.Close()

	e := newAgentEnv(t, account.Account{Name: "cursor-ide", Tool: account.CursorIDE, Dir: user})
	if got := e.sessions()[id]; got.Tokens.Input != 0 {
		t.Fatalf("input before the reply = %d", got.Tokens.Input)
	}
	db, err = sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO cursorDiskKV VALUES (?, ?)`, "bubbleId:"+id+":b2",
		`{"type":2,"bubbleId":"b2","text":"It scans.","createdAt":1700000002000,"tokenCount":{"inputTokens":30,"outputTokens":5}}`)
	db.Close()
	if got := e.sessions()[id]; got.Tokens.Input != 30 {
		t.Fatalf("input after the reply = %d, want 30", got.Tokens.Input)
	}
}

// A bubble of a composer in use that is rewritten in place, keeping its row
// id, is still read again: its size changed.
func TestCursorIDEReadsBubbleRewrittenInPlace(t *testing.T) {
	user := filepath.Join(t.TempDir(), "Cursor", "User")
	dbPath := filepath.Join(user, "globalStorage", "state.vscdb")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	const id = "abababab-abab-4bab-8bab-abababababab"
	now := time.Now().UnixMilli()
	exec := func(q string, args ...any) {
		t.Helper()
		db, err := sql.Open("sqlite", dbPath)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	reply := func(in int) string {
		return `{"type":2,"bubbleId":"r","text":"It scans the folder.","createdAt":` + strconv.FormatInt(now, 10) + `,"tokenCount":{"inputTokens":` + strconv.Itoa(in) + `,"outputTokens":5}}`
	}
	exec(`CREATE TABLE composerHeaders (composerId TEXT PRIMARY KEY, createdAt INTEGER, lastUpdatedAt INTEGER, isSubagent INTEGER, value TEXT)`)
	exec(`CREATE TABLE cursorDiskKV (key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB)`)
	exec(`INSERT INTO composerHeaders VALUES (?, ?, ?, 0, '{"name":"chat"}')`, id, now, now)
	exec(`INSERT INTO cursorDiskKV VALUES (?, ?)`, "bubbleId:"+id+":u", `{"type":1,"bubbleId":"u","text":"explain","createdAt":`+strconv.FormatInt(now, 10)+`}`)
	exec(`INSERT INTO cursorDiskKV VALUES (?, ?)`, "bubbleId:"+id+":r", reply(30))

	e := newAgentEnv(t, account.Account{Name: "cursor-ide", Tool: account.CursorIDE, Dir: user})
	if got := e.sessions()[id]; got.Tokens.Input != 30 {
		t.Fatalf("input = %d, want 30", got.Tokens.Input)
	}
	exec(`UPDATE cursorDiskKV SET value = ? WHERE key = ?`, reply(31200), "bubbleId:"+id+":r")
	if got := e.sessions()[id]; got.Tokens.Input != 31200 {
		t.Fatalf("input after the rewrite = %d, want 31200", got.Tokens.Input)
	}
}

// An IDE database that has not changed since the last full read is not
// listed again; a change, or --rebuild, lists it.
func TestCursorIDEUnchangedDatabaseIsNotListedAgain(t *testing.T) {
	user := filepath.Join(t.TempDir(), "Cursor", "User")
	dbPath := filepath.Join(user, "globalStorage", "state.vscdb")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(qs ...string) {
		t.Helper()
		db, err := sql.Open("sqlite", dbPath)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		for _, q := range qs {
			if _, err := db.Exec(q); err != nil {
				t.Fatal(err)
			}
		}
	}
	const id = "88888888-8888-4888-8888-888888888888"
	write(`CREATE TABLE composerHeaders (composerId TEXT PRIMARY KEY, createdAt INTEGER, lastUpdatedAt INTEGER, isSubagent INTEGER, value TEXT)`,
		`CREATE TABLE cursorDiskKV (key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB)`,
		`INSERT INTO composerHeaders VALUES ('`+id+`', 1700000000000, 1700000001000, 0, '{"name":"chat"}')`,
		`INSERT INTO cursorDiskKV VALUES ('bubbleId:`+id+`:b1', '{"type":1,"bubbleId":"b1","text":"explain","createdAt":1700000000500}')`)
	lists := 0
	listComposers = func(user string) ([]cursoride.Head, error) {
		lists++
		return cursoride.List(user)
	}
	t.Cleanup(func() { listComposers = cursoride.List })

	e := newAgentEnv(t, account.Account{Name: "cursor-ide", Tool: account.CursorIDE, Dir: user})
	if _, ok := e.sessions()[id]; !ok || lists != 1 {
		t.Fatalf("first run: session found %v, listed %d times", ok, lists)
	}
	if got, ok := e.sessions()[id]; !ok || got.Missing || lists != 1 {
		t.Fatalf("unchanged database: session found %v (missing %v), listed %d times, want once", ok, got.Missing, lists)
	}
	write(`INSERT INTO cursorDiskKV VALUES ('bubbleId:` + id + `:b2', '{"type":2,"bubbleId":"b2","text":"It scans.","createdAt":1700000002000,"tokenCount":{"inputTokens":30,"outputTokens":5}}')`)
	if got := e.sessions()[id]; got.Tokens.Input != 30 || lists != 2 {
		t.Fatalf("after a write: input %d, listed %d times", got.Tokens.Input, lists)
	}
	prices, _ := pricing.Load("")
	if _, err := Run(context.Background(), e.st, e.accts, prices, Options{Rebuild: true}); err != nil || lists != 3 {
		t.Fatalf("rebuild: %v, listed %d times", err, lists)
	}
}
