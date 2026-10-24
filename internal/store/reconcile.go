package store

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"

	"github.com/lr00rl/ccli/internal/transcript"
)

// ReconcileVersion changes when the reconciliation rules change; a database
// reconciled under another version is redone in full.
const ReconcileVersion = "5"

// SessionKey names a session within an account.
type SessionKey struct {
	Account   string
	SessionID string
}

// Reconcile settles every session marked dirty. Claude Code's own running
// totals (cost-state) are compared with the responses recorded in the
// transcripts, and for each running total and model the shortfall becomes
// one "adjustment" response dated at the end of the session's last
// snapshot. A shortfall is never negative, so recorded calls are never
// subtracted.
//
// Claude Code keeps one running total per session and carries it on across
// resumes, in another account too once ccli has copied the session there.
// So a session's share of a total is the growth between consecutive
// snapshots in its own transcript, counted where the later snapshot is its
// own: a snapshot ccli copied in from another account only moves the base.
// A snapshot that several files hold as their own (a transcript copied by
// hand) counts once, in the file created first; one that only copies hold
// counts in the earliest of them. In a file created after the total began
// elsewhere (a continued or forked session), a first snapshot with nothing
// before it in the file only sets the base: it includes the parent's total
// up to an unknown point, so counting it could count the parent twice.
//
// The marks are cleared in the same transaction, so a run that stops early
// leaves them for the next one. normalize maps model names to price-table keys;
// weigh prices an aggregate at list prices, and its false result for an
// unpriced model leaves the adjustment without cost, as everywhere else.
func (s *Store) Reconcile(ctx context.Context, normalize func(string) string, weigh WeighFunc) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var v string
	err = tx.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = 'reconcile'").Scan(&v)
	if err != nil && err != sql.ErrNoRows {
		return 0, err
	}
	if v != ReconcileVersion {
		if err := markAllDirty(ctx, tx); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO meta(key, value) VALUES('reconcile', ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", ReconcileVersion); err != nil {
			return 0, err
		}
	}
	// Sessions sharing a running total with a marked one are redone too:
	// which file a shared snapshot counts in may have changed.
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO dirty(account, session_id)
		SELECT DISTINCT f2.account, f2.session_id FROM snapshots p2 JOIN files f2 ON f2.id = p2.file_id
		WHERE p2.start_ms IN (SELECT DISTINCT p1.start_ms FROM dirty d
			JOIN files f1 ON f1.account = d.account AND f1.session_id = d.session_id
			JOIN snapshots p1 ON p1.file_id = f1.id)`); err != nil {
		return 0, err
	}
	rows, err := tx.QueryContext(ctx, "SELECT account, session_id FROM dirty")
	if err != nil {
		return 0, err
	}
	var keys []SessionKey
	for rows.Next() {
		var k SessionKey
		if err := rows.Scan(&k.Account, &k.SessionID); err != nil {
			rows.Close()
			return 0, err
		}
		keys = append(keys, k)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, k := range keys {
		if err := reconcileOne(ctx, tx, k, normalize, weigh); err != nil {
			return 0, err
		}
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM dirty"); err != nil {
		return 0, err
	}
	return len(keys), tx.Commit()
}

// WeighFunc prices an aggregate of tokens at base list prices; false when
// the model is unpriced.
type WeighFunc func(model string, u transcript.Usage) (float64, bool)

// usage turns a sum into token counts. Claude Code reports cache writes as
// one number; they are weighed as 1-hour writes, the kind it makes.
func usage(s sums) transcript.Usage {
	return transcript.Usage{Input: s.input, Output: s.output, CacheRead: s.read, Cache1h: s.write, WebSearch: s.web}
}

type sums struct {
	input, output, read, write, web int64
	cost                            float64
	unpriced                        int
}

func (a *sums) add(b sums) {
	a.input += b.input
	a.output += b.output
	a.read += b.read
	a.write += b.write
	a.web += b.web
	a.cost += b.cost
	a.unpriced += b.unpriced
}

// holding is one file's snapshots of a running total, in file order.
type holding struct {
	fileID int64
	sess   SessionKey
	rank   int64 // creation order: birth time, else first line time
	snaps  []*snapshot
}

// snapshot is one cost-state line: the running total at that point.
type snapshot struct {
	end      int64
	imported bool // ccli copied this line in from another account
	models   map[string]sums
	key      string // the totals, to find the same snapshot in another file
}

// window is a time range (from, to] of a session's own growth.
type window struct{ from, to int64 }

func reconcileOne(ctx context.Context, tx *sql.Tx, k SessionKey, normalize func(string) string, weigh WeighFunc) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM responses WHERE kind = 'adjustment' AND account = ? AND session_id = ?`, k.Account, k.SessionID); err != nil {
		return err
	}
	starts, err := int64s(ctx, tx, `SELECT DISTINCT p.start_ms FROM snapshots p JOIN files f ON f.id = p.file_id
		WHERE f.account = ? AND f.session_id = ? ORDER BY p.start_ms`, k.Account, k.SessionID)
	if err != nil {
		return err
	}
	for _, start := range starts {
		hs, err := holdings(ctx, tx, start, normalize)
		if err != nil {
			return err
		}
		// Which file each distinct snapshot counts in: the earliest created
		// of those holding it as their own, else of those holding a copy.
		counts := map[string]*holding{}
		own := map[string]bool{}
		// The total began in the earliest created file holding a snapshot of
		// it as its own, or, if only copies hold it, in the earliest of them.
		var first *holding
		firstOwn := false
		for _, h := range hs {
			mine := slices.ContainsFunc(h.snaps, func(sn *snapshot) bool { return !sn.imported })
			if first == nil || mine && !firstOwn || mine == firstOwn && earlier(h, first) {
				first, firstOwn = h, mine
			}
			for _, sn := range h.snaps {
				c := counts[sn.key]
				switch {
				case c == nil, !sn.imported && !own[sn.key], sn.imported == !own[sn.key] && earlier(h, c):
					counts[sn.key] = h
					own[sn.key] = own[sn.key] || !sn.imported
				}
			}
		}
		grown := map[string]sums{}
		var wins []window
		var fileID, end int64
		for _, h := range hs {
			if h.sess != k {
				continue
			}
			base, from := map[string]sums{}, start-1
			for i, sn := range h.snaps {
				counted := counts[sn.key] == h && (i > 0 || h == first)
				if counted {
					for model, v := range sn.models {
						acc := grown[model]
						acc.add(less(v, base[model]))
						grown[model] = acc
					}
					wins = append(wins, window{from, max(sn.end, from)})
					if sn.end >= end {
						fileID, end = h.fileID, max(sn.end, start)
					}
				}
				// A total never shrinks; a lower snapshot does not lower the base.
				for model, v := range sn.models {
					base[model] = most(base[model], v)
				}
				from = max(from, sn.end)
			}
		}
		if len(wins) == 0 {
			continue // all of this total's growth counts in other files
		}
		have := map[string]sums{}
		if err := recorded(ctx, tx, k, wins, normalize, have); err != nil {
			return err
		}
		for model, want := range grown {
			got := have[model]
			d := sums{
				input:  pos(want.input - got.input),
				output: pos(want.output - got.output),
				read:   pos(want.read - got.read),
				write:  pos(want.write - got.write),
				web:    pos(want.web - got.web),
			}
			if d.input+d.output+d.read+d.write+d.web == 0 {
				continue // nothing missing; a cost difference alone is a price difference
			}
			// The missing tokens are priced from the same table as every recorded
			// call (at base rates: which of them ran in fast mode or past the
			// long-context threshold is unknown), so a price override applies to
			// them too and Claude Code's own prices never leak in.
			part, ok := weigh(model, usage(d))
			costed := got.unpriced == 0 && ok
			if costed {
				d.cost = part
			}
			key := fmt.Sprintf("adj|%s|%s|%d|%s", k.Account, k.SessionID, start, model)
			if _, err := tx.ExecContext(ctx, `INSERT INTO responses(key, file_id, account, session_id, kind, ts, model, speed, input, output, cache_read, cache_5m, cache_1h, web_search, cost)
				VALUES(?,?,?,?,'adjustment',?,?,'standard',?,?,?,?,0,?,?)
				ON CONFLICT(key) DO UPDATE SET file_id=excluded.file_id, ts=excluded.ts, input=excluded.input, output=excluded.output,
				  cache_read=excluded.cache_read, cache_5m=excluded.cache_5m, web_search=excluded.web_search, cost=excluded.cost`,
				key, fileID, k.Account, k.SessionID, end, model, d.input, d.output, d.read, d.write, d.web, nullCost(d.cost, costed)); err != nil {
				return err
			}
		}
	}
	return nil
}

// earlier orders holders by creation, then by file id.
func earlier(a, b *holding) bool {
	return a.rank < b.rank || a.rank == b.rank && a.fileID < b.fileID
}

// most is the larger of two snapshots of one model, field by field.
func most(a, b sums) sums {
	return sums{input: max(a.input, b.input), output: max(a.output, b.output), read: max(a.read, b.read),
		write: max(a.write, b.write), web: max(a.web, b.web)}
}

// less is a snapshot's growth over an earlier one of the same total.
func less(a, b sums) sums {
	return sums{input: pos(a.input - b.input), output: pos(a.output - b.output), read: pos(a.read - b.read),
		write: pos(a.write - b.write), web: pos(a.web - b.web)}
}

// holdings returns every file's snapshots of the running total that
// started at start, each file's in the order they appear in it.
func holdings(ctx context.Context, tx *sql.Tx, start int64, normalize func(string) string) ([]*holding, error) {
	rows, err := tx.QueryContext(ctx, `SELECT p.file_id, p.offset, p.end_ms, p.model, p.input, p.output, p.cache_read, p.cache_write, p.web_search, p.cost,
		f.account, f.session_id, f.born, f.first_ts, p.imported
		FROM snapshots p JOIN files f ON f.id = p.file_id WHERE p.start_ms = ? ORDER BY p.file_id, p.offset, p.model`, start)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*holding
	byFile := map[int64]*holding{}
	type at struct{ file, offset int64 }
	bySnap := map[at]*snapshot{}
	for rows.Next() {
		var fileID, offset, end, born, first int64
		var imported bool
		var model string
		var v sums
		var sess SessionKey
		if err := rows.Scan(&fileID, &offset, &end, &model, &v.input, &v.output, &v.read, &v.write, &v.web, &v.cost,
			&sess.Account, &sess.SessionID, &born, &first, &imported); err != nil {
			return nil, err
		}
		h := byFile[fileID]
		if h == nil {
			rank := born
			if rank == 0 {
				rank = first
			}
			if rank == 0 {
				rank = math.MaxInt64
			}
			h = &holding{fileID: fileID, sess: sess, rank: rank}
			byFile[fileID] = h
			out = append(out, h)
		}
		sn := bySnap[at{fileID, offset}]
		if sn == nil {
			sn = &snapshot{end: end, imported: imported, models: map[string]sums{}}
			bySnap[at{fileID, offset}] = sn
			h.snaps = append(h.snaps, sn)
		}
		m := normalize(model)
		acc := sn.models[m]
		acc.add(v)
		sn.models[m] = acc
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, h := range out {
		for _, sn := range h.snaps {
			models := make([]string, 0, len(sn.models))
			for m := range sn.models {
				models = append(models, m)
			}
			sort.Strings(models)
			var b strings.Builder
			for _, m := range models {
				v := sn.models[m]
				fmt.Fprintf(&b, "%s:%d,%d,%d,%d,%d;", m, v.input, v.output, v.read, v.write, v.web)
			}
			sn.key = b.String()
		}
	}
	return out, nil
}

// recorded adds a session's transcript responses within its windows to have.
func recorded(ctx context.Context, tx *sql.Tx, sess SessionKey, wins []window, normalize func(string) string, have map[string]sums) error {
	// Merge overlapping and touching windows: each response counts once,
	// and the query stays short however many snapshots there are.
	sort.Slice(wins, func(i, j int) bool { return wins[i].from < wins[j].from })
	merged := wins[:0:0]
	for _, w := range wins {
		if n := len(merged); n > 0 && w.from <= merged[n-1].to {
			merged[n-1].to = max(merged[n-1].to, w.to)
			continue
		}
		merged = append(merged, w)
	}
	var cond []string
	args := []any{sess.Account, sess.SessionID}
	for _, w := range merged {
		cond = append(cond, "(ts > ? AND ts <= ?)")
		args = append(args, w.from, w.to)
	}
	rows, err := tx.QueryContext(ctx, `SELECT model, input, output, cache_read, cache_5m + cache_1h, web_search, COALESCE(cost, 0), cost IS NULL
		FROM responses WHERE account = ? AND session_id = ? AND kind <> 'adjustment' AND (`+strings.Join(cond, " OR ")+`)`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var model string
		var v sums
		if err := rows.Scan(&model, &v.input, &v.output, &v.read, &v.write, &v.web, &v.cost, &v.unpriced); err != nil {
			return err
		}
		m := normalize(model)
		acc := have[m]
		acc.add(v)
		have[m] = acc
	}
	return rows.Err()
}

func int64s(ctx context.Context, tx *sql.Tx, q string, args ...any) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var n int64
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func nullCost(c float64, ok bool) sql.NullFloat64 {
	return sql.NullFloat64{Float64: c, Valid: ok}
}

func pos(n int64) int64 {
	if n < 0 {
		return 0
	}
	return n
}
