package store

import (
	"context"
	"database/sql"
	"time"
)

// Tags are short labels a person gives sessions in ccli, shown next to the
// session's own title. Like hide marks, they live only in this database:
// transcripts are never touched. A tag is keyed by session id, so it labels
// the session's copies in every account.

// SetTag tags a session, replacing an earlier tag.
func (s *Store) SetTag(ctx context.Context, id, tag string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO tags(session_id, tag, at) VALUES(?, ?, ?)
		ON CONFLICT(session_id) DO UPDATE SET tag = excluded.tag, at = excluded.at`, id, tag, at.UnixMilli())
	return err
}

// ClearTag removes a session's tag.
func (s *Store) ClearTag(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM tags WHERE session_id = ?`, id)
	return err
}

// Tags returns the tag of each tagged session.
func (s *Store) Tags(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT session_id, tag FROM tags`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, tag string
		if err := rows.Scan(&id, &tag); err != nil {
			return nil, err
		}
		out[id] = tag
	}
	return out, rows.Err()
}

// titlesToTags moves the names an earlier ccli gave sessions with `rename`
// into tags, once, and drops the old table.
func titlesToTags(db *sql.DB) error {
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'titles'`).Scan(&n); err != nil || n == 0 {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT OR IGNORE INTO tags(session_id, tag, at) SELECT session_id, title, at FROM titles`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DROP TABLE titles`); err != nil {
		return err
	}
	return tx.Commit()
}
