package query_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lr00rl/ccli/internal/account"
	"github.com/lr00rl/ccli/internal/cursoredits"
	"github.com/lr00rl/ccli/internal/query"
)

// show reads what Cursor records beside a conversation: the files its
// agents wrote (the main chat's and its subagents'), and for an IDE chat
// the header's context and change figures.
func TestCursorDetailEditsAndComposer(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	user := filepath.Join(root, "Cursor", "User")
	const ide = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
	const sub = "ffffffff-ffff-4fff-8fff-ffffffffffff"
	const cli = "99999999-9999-4999-8999-999999999999"
	now := time.Now().Add(-time.Hour).UnixMilli()
	exec := ideDB(t, user, now, ide, sub)
	exec(`UPDATE composerHeaders SET value = ? WHERE composerId = ?`,
		`{"name":"chat","contextUsagePercent":45.5,"totalLinesAdded":95,"totalLinesRemoved":1,"filesChangedCount":3}`, ide)
	exec(`UPDATE composerHeaders SET isSubagent = 1, value = ? WHERE composerId = ?`,
		`{"name":"sub","subagentInfo":{"parentComposerId":"`+ide+`","rootParentConversationId":"`+ide+`"}}`, sub)

	home := filepath.Join(root, ".cursor")
	p := filepath.Join(home, "projects", "work-app", "agent-transcripts", cli, cli+".jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(`{"role":"user","message":{"content":[{"type":"text","text":"<user_query>\nask\n</user_query>"}]}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	track := cursoredits.DBPath(home)
	if err := os.MkdirAll(filepath.Dir(track), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", track+"?_pragma=journal_mode(delete)")
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE ai_code_hashes (hash TEXT PRIMARY KEY, source TEXT NOT NULL, fileExtension TEXT, fileName TEXT,
			requestId TEXT, conversationId TEXT, timestamp INTEGER, model TEXT, createdAt INTEGER NOT NULL)`,
		`INSERT INTO ai_code_hashes VALUES ('h1', 'composer', 'go', '/w/main.go', '', '` + ide + `', 1, 'grok-4.7', 1)`,
		`INSERT INTO ai_code_hashes VALUES ('h2', 'composer', 'go', '/w/sub.go', '', '` + sub + `', 2, 'grok-4.7', 2)`,
		`INSERT INTO ai_code_hashes VALUES ('h3', 'cli', 'py', '/w/cli.py', '', '` + cli + `', 3, 'claude-opus-5-5', 3)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	accts := []account.Account{
		{Name: "cursor", Tool: account.Cursor, Dir: home},
		{Name: "cursor-ide", Tool: account.CursorIDE, Dir: user},
	}
	st := indexed(t, root, accts)
	ctx := context.Background()

	det, err := query.New(st, accts).Session(ctx, "cursor-ide:"+ide)
	if err != nil {
		t.Fatal(err)
	}
	if det.Edits == nil || len(det.Edits.Files) != 2 || det.Edits.Files[0].Path != "/w/sub.go" || det.Edits.Files[1].Path != "/w/main.go" {
		t.Fatalf("IDE edits: %+v, want the subagent's file and the chat's, newest first", det.Edits)
	}
	c := det.Composer
	if c == nil || c.ContextPercent == nil || *c.ContextPercent != 45.5 || c.LinesAdded != 95 || c.LinesRemoved != 1 || c.FilesChanged != 3 {
		t.Fatalf("composer: %+v", c)
	}

	det, err = query.New(st, accts).Session(ctx, "cursor:"+cli)
	if err != nil {
		t.Fatal(err)
	}
	if det.Edits == nil || len(det.Edits.Files) != 1 || det.Edits.Models[0] != "claude-opus-5-5" || det.Composer != nil {
		t.Fatalf("CLI detail: edits %+v composer %+v", det.Edits, det.Composer)
	}
}
