// Package sqlitero opens another program's SQLite database for reading
// without creating any file beside it.
package sqlitero

import (
	"database/sql"
	"net/url"
	"os"

	_ "modernc.org/sqlite"
)

// Open opens path read-only.
//
// While the owner has a WAL database open, its -wal and -shm sit beside it,
// and recent commits may exist only in the -wal. A plain read-only
// connection reads them through the owner's -shm and creates nothing. With
// no -wal, every commit is in the main file, and immutable=1 reads it without
// creating a -shm. A -wal without a -shm also opens immutable: a normal
// connection would create the -shm, so those commits are not read until the
// owner checkpoints.
func Open(path string) (*sql.DB, error) {
	q := "mode=ro&immutable=1"
	if exists(path+"-wal") && exists(path+"-shm") {
		q = "mode=ro&_pragma=busy_timeout(2000)"
	}
	u := url.URL{Scheme: "file", Path: path, RawQuery: q}
	return sql.Open("sqlite", u.String())
}

func exists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}
