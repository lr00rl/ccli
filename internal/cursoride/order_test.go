package cursoride

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestBubbleOrderFollowsHeaders(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.vscdb")
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
	exec(`CREATE TABLE composerHeaders (
		composerId TEXT PRIMARY KEY, createdAt INTEGER, lastUpdatedAt INTEGER, isSubagent INTEGER, value TEXT)`)
	exec(`CREATE TABLE cursorDiskKV (key TEXT PRIMARY KEY, value TEXT)`)
	const id = "c1"
	exec(`INSERT INTO composerHeaders (composerId, createdAt, lastUpdatedAt, isSubagent, value) VALUES (?,?,?,?,?)`,
		id, int64(1), int64(2), 0, `{"name":"ordered"}`)
	exec(`INSERT INTO cursorDiskKV (key, value) VALUES (?, ?)`, "composerData:"+id,
		`{"fullConversationHeadersOnly":[{"bubbleId":"early","type":1},{"bubbleId":"late","type":1}]}`)
	exec(`INSERT INTO cursorDiskKV (key, value) VALUES (?, ?)`, "bubbleId:"+id+":late",
		`{"type":1,"bubbleId":"late","text":"second","createdAt":100}`)
	exec(`INSERT INTO cursorDiskKV (key, value) VALUES (?, ?)`, "bubbleId:"+id+":early",
		`{"type":1,"bubbleId":"early","text":"first","createdAt":900}`)
	exec(`INSERT INTO cursorDiskKV (key, value) VALUES (?, ?)`, "bubbleId:"+id+":extra",
		`{"type":2,"bubbleId":"extra","text":"dropped","createdAt":"","toolFormerData":{"name":"Shell","toolCallId":"t","params":{"command":"ls"}}}`)
	db.Close()

	res, _, err := Parse(dbPath + "#" + id)
	if err != nil {
		t.Fatal(err)
	}
	if res.TypedPrompts != 2 || res.ToolCalls != 0 || len(res.Turns) != 2 {
		t.Fatalf("prompts %d tools %d turns %d", res.TypedPrompts, res.ToolCalls, len(res.Turns))
	}
	if res.Turns[0].Text != "first" || res.Turns[1].Text != "second" {
		t.Fatalf("order %q then %q", res.Turns[0].Text, res.Turns[1].Text)
	}
}

func TestMultiRootWorkspaceIsItsFolder(t *testing.T) {
	user := t.TempDir()
	dir := filepath.Join(user, "workspaceStorage", "ws2")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "workspace.json"), []byte(`{"workspace":"file:///work/mono/mono.code-workspace"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := workspacePath(user, "ws2"); got != "/work/mono" {
		t.Fatalf("cwd %q, want /work/mono", got)
	}
}
