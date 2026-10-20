package sqlitero

import (
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func names(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

func count(t *testing.T, path string) int {
	t.Helper()
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM kv`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestOpenReadsTheOwnersWAL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state db.vscdb")
	w, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(wal)&_pragma=wal_autocheckpoint(0)")
	if err != nil {
		t.Fatal(err)
	}
	w.SetMaxOpenConns(1)
	if _, err := w.Exec(`CREATE TABLE kv(k TEXT PRIMARY KEY, v TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	// These commits stay in the -wal while the writer is open.
	for _, k := range []string{"a", "b", "c"} {
		if _, err := w.Exec(`INSERT INTO kv VALUES(?, 'x')`, k); err != nil {
			t.Fatal(err)
		}
	}
	before := names(t, dir)
	if got := count(t, path); got != 3 {
		t.Fatalf("read %d rows with the owner open, want 3", got)
	}
	if after := names(t, dir); !slices.Equal(before, after) {
		t.Fatalf("files changed: %v -> %v", before, after)
	}

	// The owner closes: SQLite checkpoints and removes the -wal and -shm.
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	before = names(t, dir)
	if got := count(t, path); got != 3 {
		t.Fatalf("read %d rows after the owner closed, want 3", got)
	}
	if after := names(t, dir); !slices.Equal(before, after) {
		t.Fatalf("files changed: %v -> %v", before, after)
	}
}
