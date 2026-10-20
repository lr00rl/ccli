// Package sqlitero opens another program's SQLite database for reading
// without creating or writing any file beside it.
package sqlitero

import (
	"database/sql"
	"net/url"
	"os"

	_ "modernc.org/sqlite"
)

// Open opens path read-only on a single connection, and has already read
// the schema through it when it returns.
//
// While the owner has a WAL database open, its -wal and -shm sit beside it,
// and recent commits may exist only in the -wal. Open then reads them
// through the owner's -shm, opened read-only (readonly_shm), so SQLite can
// neither create that file nor write to it. With no -wal every commit is in
// the main file, and immutable=1 reads it without opening either sidecar.
//
// If the owner closes between the check and the read, it deletes both
// sidecars. The read-only -shm open then fails instead of creating one, and
// Open falls back to immutable=1; at most an empty -wal is left, which
// SQLite discards on its next open. Any other failure of the first read
// falls back the same way.
func Open(path string) (*sql.DB, error) {
	if exists(path+"-wal") && exists(path+"-shm") {
		afterCheck()
		if db, err := try(path, "mode=ro&readonly_shm=1&_pragma=busy_timeout(2000)"); err == nil {
			return db, nil
		}
	}
	return try(path, "mode=ro&immutable=1")
}

func try(path, query string) (*sql.DB, error) {
	u := url.URL{Scheme: "file", Path: path, RawQuery: query}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	// One connection, opened now: a later one would reuse this choice
	// after the sidecars may have changed.
	db.SetMaxOpenConns(1)
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master`).Scan(&n); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// afterCheck lets a test close the owner between the check and the read.
var afterCheck = func() {}

func exists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}
