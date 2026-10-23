package cursoredits

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// trackingDB writes a tracking database with Cursor's table and the rows
// given as (conversation, file, model, timestamp), one row per line written.
func trackingDB(t *testing.T, rows [][4]any) string {
	t.Helper()
	p := DBPath(t.TempDir())
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", p+"?_pragma=journal_mode(delete)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE ai_code_hashes (hash TEXT PRIMARY KEY, source TEXT NOT NULL, fileExtension TEXT,
		fileName TEXT, requestId TEXT, conversationId TEXT, timestamp INTEGER, model TEXT, createdAt INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for i, r := range rows {
		if _, err := db.Exec(`INSERT INTO ai_code_hashes VALUES (?, 'cli', '', ?, '', ?, ?, ?, ?)`,
			i, r[1], r[0], r[3], r[2], r[3]); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

func TestReadListsFilesNewestFirst(t *testing.T) {
	p := trackingDB(t, [][4]any{
		{"main", "/w/a.go", "grok-4.7", int64(100)},
		{"main", "/w/a.go", "grok-4.7", int64(300)},
		{"main", "/w/a.go", "claude-opus-5-5", int64(200)},
		{"main", "/w/b.go", "claude-opus-5-5", int64(400)},
		{"sub", "/w/c.go", nil, int64(250)},
		{"other", "/w/d.go", "grok-4.7", int64(999)},
	})
	got, err := Read(context.Background(), p, []string{"main", "sub"})
	if err != nil {
		t.Fatal(err)
	}
	want := Edits{
		Files: []File{
			{Path: "/w/b.go", Models: []string{"claude-opus-5-5"}, Last: time.UnixMilli(400)},
			{Path: "/w/a.go", Models: []string{"grok-4.7", "claude-opus-5-5"}, Last: time.UnixMilli(300)},
			{Path: "/w/c.go", Last: time.UnixMilli(250)},
		},
		Models: []string{"claude-opus-5-5", "grok-4.7"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
}

func TestReadWithoutDatabase(t *testing.T) {
	got, err := Read(context.Background(), filepath.Join(t.TempDir(), "none.db"), []string{"main"})
	if err != nil || len(got.Files) != 0 {
		t.Fatalf("got %+v, %v; want nothing and no error", got, err)
	}
}
