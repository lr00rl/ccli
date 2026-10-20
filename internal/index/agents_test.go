package index

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/lr00rl/ccli/internal/account"
	"github.com/lr00rl/ccli/internal/pricing"
	"github.com/lr00rl/ccli/internal/query"
	"github.com/lr00rl/ccli/internal/store"
)

func TestPiCursorAndIDE(t *testing.T) {
	root := t.TempDir()
	piHome := filepath.Join(root, "pi-agent")
	curHome := filepath.Join(root, "cursor")
	ideHome := filepath.Join(root, "Cursor", "User")

	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	const piID = "11111111-1111-4111-8111-111111111111"
	write(filepath.Join(piHome, "sessions", "--work-app--", "2026-08-01T00-00-00_"+piID+".jsonl"),
		`{"type":"session","version":3,"id":"`+piID+`","timestamp":"2026-08-01T00:00:00Z","cwd":"/work/app"}`+"\n"+
			`{"type":"message","id":"u1","parentId":null,"timestamp":"2026-08-01T00:00:01Z","message":{"role":"user","content":"pi prompt","timestamp":1000}}`+"\n"+
			`{"type":"message","id":"a1","parentId":"u1","timestamp":"2026-08-01T00:00:02Z","message":{"role":"assistant","content":[{"type":"text","text":"done"}],"model":"gpt-5.4","usage":{"input":10,"output":4,"cacheRead":0,"cacheWrite":0,"totalTokens":14,"cost":{"total":0.2}},"timestamp":2000}}`+"\n")

	const curID = "22222222-2222-4222-8222-222222222222"
	const subID = "33333333-3333-4333-8333-333333333333"
	write(filepath.Join(curHome, "projects", "work-app", "agent-transcripts", curID, curID+".jsonl"),
		`{"role":"user","message":{"content":[{"type":"text","text":"<timestamp>t</timestamp>\n<user_query>\nfix the parser\n</user_query>"}]}}`+"\n"+
			`{"role":"assistant","message":{"content":[{"type":"text","text":"Looking."},{"type":"tool_use","name":"Shell","input":{"command":"go test"}}]}}`+"\n"+
			`{"type":"turn_ended","status":"success"}`+"\n")
	write(filepath.Join(curHome, "projects", "work-app", "agent-transcripts", curID, "subagents", subID+".jsonl"),
		`{"role":"user","message":{"content":[{"type":"text","text":"<user_query>\nreview the diff\n</user_query>"}]}}`+"\n"+
			`{"role":"assistant","message":{"content":[{"type":"text","text":"LGTM"}]}}`+"\n")
	write(filepath.Join(curHome, "chats", "abc", curID, "meta.json"),
		`{"schemaVersion":1,"createdAtMs":1000,"updatedAtMs":5000,"hasConversation":true,"title":"Parser","cwd":"/work/app"}`+"\n")
	const ownSub = "66666666-6666-4666-8666-666666666666"
	write(filepath.Join(curHome, "projects", "work-app", "agent-transcripts", ownSub, ownSub+".jsonl"),
		`{"role":"user","message":{"content":[{"type":"text","text":"<user_query>\nowned folder\n</user_query>"}]}}`+"\n"+
			`{"role":"assistant","message":{"content":[{"type":"text","text":"noted"}]}}`+"\n")
	write(filepath.Join(curHome, "chats", "abc", ownSub, "meta.json"),
		`{"isSubagent":true,"createdAtMs":2000,"updatedAtMs":4000,"title":"own folder"}`+"\n")
	sdb, err := sql.Open("sqlite", filepath.Join(curHome, "chats", "abc", ownSub, "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sdb.Exec(`CREATE TABLE meta (key TEXT, value TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := sdb.Exec(`INSERT INTO meta (key, value) VALUES ('0', ?)`,
		`{"lastUsedModel":"composer-2","subagentInfo":{"parentAgentId":"`+curID+`","rootParentAgentId":"`+curID+`","toolCallId":"call-sub","typeName":"reviewer"}}`); err != nil {
		t.Fatal(err)
	}
	sdb.Close()

	const ideID = "44444444-4444-4444-8444-444444444444"
	const ideSub = "55555555-5555-4555-8555-555555555555"
	dbPath := filepath.Join(ideHome, "globalStorage", "state.vscdb")
	write(filepath.Join(ideHome, "workspaceStorage", "ws1", "workspace.json"),
		`{"folder":"file:///work/ide"}`+"\n")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	mustExec(`CREATE TABLE composerHeaders (
		composerId TEXT PRIMARY KEY, workspaceId TEXT, createdAt INTEGER, lastUpdatedAt INTEGER,
		isArchived INTEGER, isSubagent INTEGER, recency INTEGER, checkpointAt INTEGER,
		subagentTypeName TEXT, value TEXT)`)
	mustExec(`CREATE TABLE cursorDiskKV (key TEXT PRIMARY KEY, value TEXT)`)
	mustExec(`INSERT INTO composerHeaders (composerId, createdAt, lastUpdatedAt, isSubagent, value) VALUES (?,?,?,?,?)`,
		ideID, int64(1700000000000), int64(1700000005000), 0,
		`{"name":"IDE chat","workspaceIdentifier":{"id":"ws1"}}`)
	mustExec(`INSERT INTO composerHeaders (composerId, createdAt, lastUpdatedAt, isSubagent, value) VALUES (?,?,?,?,?)`,
		ideSub, int64(1700000001000), int64(1700000004000), 1,
		`{"name":"reviewer","workspaceIdentifier":{"id":"ws1"},"subagentInfo":{"parentComposerId":"`+ideID+`","rootParentConversationId":"`+ideID+`","subagentTypeName":"reviewer","toolCallId":"call-1"}}`)
	mustExec(`INSERT INTO cursorDiskKV (key, value) VALUES (?, ?)`, "bubbleId:"+ideID+":b1",
		`{"type":1,"bubbleId":"b1","text":"explain the index","createdAt":1700000000000}`)
	mustExec(`INSERT INTO cursorDiskKV (key, value) VALUES (?, ?)`, "bubbleId:"+ideID+":b2",
		`{"type":2,"bubbleId":"b2","text":"It scans files.","createdAt":1700000002000,"tokenCount":{"inputTokens":12,"outputTokens":6},"toolFormerData":{"name":"Read","toolCallId":"t1","params":{"path":"main.go"},"status":"success"}}`)
	mustExec(`INSERT INTO cursorDiskKV (key, value) VALUES (?, ?)`, "bubbleId:"+ideSub+":s1",
		`{"type":1,"bubbleId":"s1","text":"check the lock","createdAt":1700000003000}`)
	const draftID = "draft-00000000-0000-4000-8000-0000000000d1"
	mustExec(`INSERT INTO composerHeaders (composerId, createdAt, lastUpdatedAt, isSubagent, value) VALUES (?,?,?,?,?)`,
		draftID, int64(1700000006000), int64(1700000007000), 0,
		`{"isDraft":true,"workspaceIdentifier":{"id":"empty-window"}}`)
	db.Close()

	accts := []account.Account{
		{Name: "pi", Tool: account.Pi, Dir: piHome},
		{Name: "cursor", Tool: account.Cursor, Dir: curHome},
		{Name: "cursor-ide", Tool: account.CursorIDE, Dir: ideHome},
	}
	st, err := store.Open(filepath.Join(root, "ccli.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	prices, err := pricing.Load("")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), st, accts, prices, Options{}); err != nil {
		t.Fatal(err)
	}
	// A second run must not double the prompts.
	if _, err := Run(context.Background(), st, accts, prices, Options{}); err != nil {
		t.Fatal(err)
	}
	q := query.New(st, accts)
	page, err := q.Sessions(context.Background(), query.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]query.SessionRow{}
	for _, s := range page.Sessions {
		byID[s.ID] = s
	}
	if len(byID) != 3 {
		t.Fatalf("sessions %d, want 3: %+v", len(byID), page.Sessions)
	}
	if page.Drafts != 1 {
		t.Fatalf("drafts left out = %d, want 1", page.Drafts)
	}
	withDrafts, err := q.Sessions(context.Background(), query.Filter{Drafts: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(withDrafts.Sessions) != 4 || withDrafts.Drafts != 0 {
		t.Fatalf("--drafts sessions %d left out %d", len(withDrafts.Sessions), withDrafts.Drafts)
	}
	piRow := byID[piID]
	if piRow.Cwd != "/work/app" || piRow.Prompts != 1 || piRow.Tool != account.Pi || piRow.Cost.USD < 0.19 {
		t.Fatalf("pi row %+v", piRow)
	}
	cur := byID[curID]
	if cur.Cwd != "/work/app" || cur.Prompts < 1 || cur.Title != "Parser" || cur.Subagents != 2 || cur.ToolCalls < 1 {
		t.Fatalf("cursor row %+v", cur)
	}
	if _, ok := byID[ownSub]; ok {
		t.Fatal("subagent transcript was indexed as its own session")
	}
	ide := byID[ideID]
	if ide.Cwd != "/work/ide" || ide.Prompts < 1 || ide.Title != "IDE chat" || ide.Subagents != 1 || ide.Tokens.Input != 12 || ide.Tool != account.CursorIDE {
		t.Fatalf("ide row %+v", ide)
	}
	pro, err := q.Projects(context.Background(), query.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]int{}
	for _, p := range pro.Projects {
		paths[p.Path] = p.Sessions
	}
	if paths["/work/app"] < 2 || paths["/work/ide"] != 1 {
		t.Fatalf("projects %+v", paths)
	}
	det, err := q.Session(context.Background(), piID)
	if err != nil {
		t.Fatal(err)
	}
	if !det.Resume.Available || det.Resume.Program != "pi" {
		t.Fatalf("pi resume %+v", det.Resume)
	}
	det, err = q.Session(context.Background(), curID)
	if err != nil {
		t.Fatal(err)
	}
	if !det.Resume.Available || det.Resume.Program != "agent" {
		t.Fatalf("cursor resume %+v", det.Resume)
	}
	det, err = q.Session(context.Background(), ideID)
	if err != nil {
		t.Fatal(err)
	}
	if det.Resume.Available || det.Resume.Program != "Cursor IDE" {
		t.Fatalf("ide resume %+v", det.Resume)
	}
	prompts, err := q.Prompts(context.Background(), query.Filter{Tools: []account.Tool{account.Cursor}})
	if err != nil {
		t.Fatal(err)
	}
	if len(prompts.Prompts) != 1 || prompts.Prompts[0].Prompt != "fix the parser" {
		t.Fatalf("cursor prompts %+v", prompts.Prompts)
	}
}

func TestCursorDuplicateIDKeepsNewer(t *testing.T) {
	root := t.TempDir()
	const id = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	older := filepath.Join(root, "projects", "old", "agent-transcripts", id, id+".jsonl")
	newer := filepath.Join(root, "projects", "new", "agent-transcripts", id, id+".jsonl")
	subOld := filepath.Join(root, "projects", "old", "agent-transcripts", id, "subagents", "sub.jsonl")
	subNew := filepath.Join(root, "projects", "new", "agent-transcripts", id, "subagents", "sub.jsonl")
	for _, p := range []string{older, newer, subOld, subNew} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	oldT := time.Now().Add(-time.Hour)
	newT := time.Now()
	if err := os.Chtimes(older, oldT, oldT); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(newer, newT, newT); err != nil {
		t.Fatal(err)
	}
	got, err := scanCursor(account.Account{Name: "cursor", Tool: account.Cursor, Dir: root})
	if err != nil {
		t.Fatal(err)
	}
	var mains, subs int
	for _, d := range got {
		switch d.file.Kind {
		case "main":
			mains++
			if d.file.Path != newer {
				t.Fatalf("kept %s, want the newer copy", d.file.Path)
			}
		case "subagent":
			subs++
			if d.file.Path != subNew {
				t.Fatalf("subagent %s, want the one beside the newer copy", d.file.Path)
			}
		}
	}
	if mains != 1 || subs != 1 {
		t.Fatalf("mains %d subs %d, want 1 and 1", mains, subs)
	}
}

func TestDiscoverNewHomes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CCLI_CONFIG_DIRS", filepath.Join(home, "none-claude"))
	t.Setenv("CCLI_CODEX_DIRS", filepath.Join(home, "none-codex"))
	piDir := filepath.Join(home, "agent")
	curDir := filepath.Join(home, "cursor-data")
	ideDir := filepath.Join(home, "ide-user")
	for _, d := range []string{
		filepath.Join(piDir, "sessions"),
		filepath.Join(curDir, "projects"),
		filepath.Join(ideDir, "globalStorage"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(ideDir, "globalStorage", "state.vscdb"), []byte("not really"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CCLI_PI_DIRS", piDir)
	t.Setenv("CCLI_CURSOR_DIRS", curDir)
	t.Setenv("CCLI_CURSOR_IDE_DIRS", ideDir)
	accts, err := account.Discover()
	if err != nil {
		t.Fatal(err)
	}
	got := map[account.Tool]string{}
	for _, a := range accts {
		got[a.Kind()] = a.Name
	}
	if got[account.Pi] == "" || got[account.Cursor] == "" || got[account.CursorIDE] == "" {
		t.Fatalf("discovered %+v", accts)
	}
	if _, ok := got[account.Claude]; ok {
		t.Fatalf("claude home leaked in: %+v", accts)
	}
}

// A Pi range is read from one parse of the file, and a turn the file no
// longer has is an error rather than an empty turn.
func TestPiTurnRange(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "pi-agent")
	const id = "44444444-4444-4444-8444-444444444444"
	path := filepath.Join(home, "sessions", "--work-app--", "2026-08-01T00-00-00_"+id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	head := `{"type":"session","version":3,"id":"` + id + `","timestamp":"2026-08-01T00:00:00Z","cwd":"/work/app"}` + "\n"
	msg := func(id, parent, role, ts, body string) string {
		p := "null"
		if parent != "" {
			p = `"` + parent + `"`
		}
		return `{"type":"message","id":"` + id + `","parentId":` + p + `,"timestamp":"2026-08-01T00:00:` + ts + `Z","message":{"role":"` + role + `","content":` + body + `}}` + "\n"
	}
	one := msg("u1", "", "user", "01", `"first ask"`) + msg("a1", "u1", "assistant", "02", `[{"type":"text","text":"first answer"}]`)
	two := msg("u2", "a1", "user", "03", `"second ask"`) + msg("a2", "u2", "assistant", "04", `[{"type":"text","text":"second answer"}]`)
	if err := os.WriteFile(path, []byte(head+one+two), 0o644); err != nil {
		t.Fatal(err)
	}
	accts := []account.Account{{Name: "pi", Tool: account.Pi, Dir: home}}
	st, err := store.Open(filepath.Join(root, "ccli.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	prices, _ := pricing.Load("")
	if _, err := Run(context.Background(), st, accts, prices, Options{}); err != nil {
		t.Fatal(err)
	}
	q := query.New(st, accts)
	got, err := q.TurnRange(context.Background(), id[:8], 1, 2, false)
	if err != nil || len(got) != 2 {
		t.Fatalf("range: %d turns, err %v", len(got), err)
	}
	for _, want := range []string{"first answer", "second answer"} {
		found := false
		for _, d := range got {
			for _, e := range d.Turn.Entries {
				found = found || e.Text == want
			}
		}
		if !found {
			t.Fatalf("%q missing from the range", want)
		}
	}
	// The file loses its second turn after the index read it.
	if err := os.WriteFile(path, []byte(head+one), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := q.TurnRange(context.Background(), id[:8], 2, 2, false); err == nil || !strings.Contains(err.Error(), "no longer in") {
		t.Fatalf("err = %v, want the turn reported as gone", err)
	}
}
