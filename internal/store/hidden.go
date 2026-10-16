package store

import (
	"context"
	"time"
)

// Hidden sessions are ones a person set aside so session lists leave them
// out. The mark lives only in this database: transcripts are never touched.
// The query layer counts a session as hidden while its last activity is no
// later than the mark, so using the session again brings it back.

// Hide marks sessions as set aside, each at its own time, replacing older
// marks.
func (s *Store) Hide(ctx context.Context, marks map[string]time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for id, at := range marks {
		if _, err := tx.ExecContext(ctx, `INSERT INTO hidden(session_id, at) VALUES(?, ?)
			ON CONFLICT(session_id) DO UPDATE SET at = excluded.at`, id, at.UnixMilli()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Unhide removes the marks of sessions.
func (s *Store) Unhide(ctx context.Context, ids []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `DELETE FROM hidden WHERE session_id = ?`, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Hidden returns when each marked session was hidden, in Unix milliseconds.
func (s *Store) Hidden(ctx context.Context) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT session_id, at FROM hidden`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var id string
		var at int64
		if err := rows.Scan(&id, &at); err != nil {
			return nil, err
		}
		out[id] = at
	}
	return out, rows.Err()
}
