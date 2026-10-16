package store

import (
	"context"
	"time"
)

// Titles are names a person gave sessions in ccli. Like hide marks, they
// live only in this database: transcripts are never touched. A title is
// keyed by session id, so it names the session's copies in every account.

// SetTitle names a session, replacing an earlier name.
func (s *Store) SetTitle(ctx context.Context, id, title string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO titles(session_id, title, at) VALUES(?, ?, ?)
		ON CONFLICT(session_id) DO UPDATE SET title = excluded.title, at = excluded.at`, id, title, at.UnixMilli())
	return err
}

// ClearTitle removes the name given to a session.
func (s *Store) ClearTitle(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM titles WHERE session_id = ?`, id)
	return err
}

// Titles returns the name given to each named session.
func (s *Store) Titles(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT session_id, title FROM titles`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, title string
		if err := rows.Scan(&id, &title); err != nil {
			return nil, err
		}
		out[id] = title
	}
	return out, rows.Err()
}
