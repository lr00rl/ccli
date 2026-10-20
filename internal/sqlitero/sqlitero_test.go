package sqlitero

import (
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

type files map[string]int64

func list(t *testing.T, dir string) files {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := files{}
	for _, e := range ents {
		st, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = st.Size()
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

// owner opens a WAL database and leaves three rows only in the -wal.
func owner(t *testing.T) (string, *sql.DB) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "state db.vscdb")
	w, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(wal)&_pragma=wal_autocheckpoint(0)")
	if err != nil {
		t.Fatal(err)
	}
	w.SetMaxOpenConns(1)
	for _, q := range []string{`CREATE TABLE kv(k TEXT PRIMARY KEY, v TEXT)`, `PRAGMA wal_checkpoint(TRUNCATE)`,
		`INSERT INTO kv VALUES('a', 'x'), ('b', 'x'), ('c', 'x')`} {
		if _, err := w.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return path, w
}

func TestOpenReadsTheOwnersWAL(t *testing.T) {
	path, w := owner(t)
	dir := filepath.Dir(path)
	before := list(t, dir)
	if got := count(t, path); got != 3 {
		t.Fatalf("read %d rows with the owner open, want 3", got)
	}
	if after := list(t, dir); !maps(before, after) {
		t.Fatalf("files changed: %v -> %v", before, after)
	}

	// The owner closes: SQLite checkpoints and removes the -wal and -shm.
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	before = list(t, dir)
	if got := count(t, path); got != 3 {
		t.Fatalf("read %d rows after the owner closed, want 3", got)
	}
	if after := list(t, dir); !maps(before, after) {
		t.Fatalf("files changed: %v -> %v", before, after)
	}
}

// The owner closes between the check for its sidecars and the read. The
// read falls back to the main file, and no -shm is created.
func TestOpenOwnerClosesAfterTheCheck(t *testing.T) {
	path, w := owner(t)
	afterCheck = func() { w.Close() }
	t.Cleanup(func() { afterCheck = func() {} })
	if got := count(t, path); got != 3 {
		t.Fatalf("read %d rows, want 3", got)
	}
	after := list(t, filepath.Dir(path))
	if _, ok := after[filepath.Base(path)+"-shm"]; ok {
		t.Fatalf("a -shm was created: %v", after)
	}
	if n, ok := after[filepath.Base(path)+"-wal"]; ok && n != 0 {
		t.Fatalf("a -wal with %d bytes was left: %v", n, after)
	}
}

func maps(a, b files) bool {
	ka, kb := keys(a), keys(b)
	if !slices.Equal(ka, kb) {
		return false
	}
	for k := range a {
		if a[k] != b[k] {
			return false
		}
	}
	return true
}

func keys(m files) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
