package store

import "context"

// Forget removes one session of one account from the index: its files and
// every row read from them, and its workflow runs. Tags, hide marks and the
// transfer ledger are kept, since they are keyed by session id and other
// accounts may hold the same session. It returns how many files it removed.
func (s *Store) Forget(ctx context.Context, account, sessionID string) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	files := `SELECT id FROM files WHERE account = ? AND session_id = ?`
	for _, table := range []string{"responses", "activity", "tools", "links", "processes", "spawns", "turns"} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE file_id IN (`+files+`)`, account, sessionID); err != nil {
			return 0, err
		}
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM files WHERE account = ? AND session_id = ?`, account, sessionID)
	if err != nil {
		return 0, err
	}
	for _, q := range []string{
		`DELETE FROM runs WHERE account = ? AND session_id = ?`,
		`DELETE FROM dirty WHERE account = ? AND session_id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, q, account, sessionID); err != nil {
			return 0, err
		}
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), tx.Commit()
}
