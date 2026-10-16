package query

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Visibility selects sessions by whether a person hid them. Hiding only
// leaves a session out of lists: totals, `show` and `log` still see it.
type Visibility int

const (
	Visible    Visibility = iota // sessions not hidden: what lists show by default
	HiddenOnly                   // only hidden sessions
	AnyHidden                    // every session
)

func (v Visibility) String() string {
	switch v {
	case HiddenOnly:
		return "hidden"
	case AnyHidden:
		return "all"
	}
	return "visible"
}

// MarshalText writes the visibility by name in JSON.
func (v Visibility) MarshalText() ([]byte, error) { return []byte(v.String()), nil }

func (v Visibility) keeps(hidden bool) bool {
	switch v {
	case HiddenOnly:
		return hidden
	case AnyHidden:
		return true
	}
	return !hidden
}

// markHidden sets Hidden on rows whose session was hidden after its last
// activity. A session used again after it was hidden shows again.
func (d *DB) markHidden(ctx context.Context, rows []SessionRow) error {
	marks, err := d.st.Hidden(ctx)
	if err != nil || len(marks) == 0 {
		return err
	}
	for i := range rows {
		if at, ok := marks[rows[i].ID]; ok && at >= rows[i].Last.UnixMilli() {
			rows[i].Hidden = true
		}
	}
	return nil
}

// HideResult is what Hide or Unhide did with one session.
type HideResult struct {
	Ref     string `json:"ref"`
	Account string `json:"account,omitempty"`
	ID      string `json:"id,omitempty"`
	Title   string `json:"title,omitempty"`
	Done    bool   `json:"done"`
	Reason  string `json:"reason,omitempty"` // why it was left as it was
}

// Hide sets sessions aside, so lists leave them out until they are used
// again. A session a Claude Code process is running is left alone: it would
// show again with its next line. Hiding marks the session id, so its copies
// in other accounts are hidden with it.
func (d *DB) Hide(ctx context.Context, refs []string, now time.Time) ([]HideResult, error) {
	return d.setHidden(ctx, refs, func(r *HideResult, row SessionRow) {
		switch {
		case row.Live:
			r.Reason = "running; hide it once it has ended"
		case row.Hidden:
			r.Reason = "already hidden"
		default:
			r.Done = true
		}
	}, func(ids []string) error { return d.st.Hide(ctx, ids, now) })
}

// Unhide brings hidden sessions back into lists.
func (d *DB) Unhide(ctx context.Context, refs []string) ([]HideResult, error) {
	return d.setHidden(ctx, refs, func(r *HideResult, row SessionRow) {
		if !row.Hidden {
			r.Reason = "not hidden"
			return
		}
		r.Done = true
	}, func(ids []string) error { return d.st.Unhide(ctx, ids) })
}

// setHidden resolves refs, lets decide fill in each result, and applies
// write to the ids of the sessions it marked done.
func (d *DB) setHidden(ctx context.Context, refs []string, decide func(*HideResult, SessionRow), write func([]string) error) ([]HideResult, error) {
	rows, err := d.loadSessions(ctx, Filter{})
	if err != nil {
		return nil, err
	}
	byRef := map[skey]SessionRow{}
	for _, r := range rows {
		byRef[skey{r.Account, r.ID}] = r
	}
	var out []HideResult
	var ids []string
	seen := map[string]bool{}
	for _, ref := range refs {
		res := HideResult{Ref: ref}
		acct, id, err := d.Resolve(ctx, ref)
		if errors.Is(err, ErrNotFound) {
			res.Reason = "no such session"
			out = append(out, res)
			continue
		}
		if err != nil {
			return nil, err
		}
		row, ok := byRef[skey{acct, id}]
		if !ok {
			return nil, fmt.Errorf("session %s is in the index but has no files", ShortID(id))
		}
		res.Account, res.ID, res.Title = acct, id, row.Title
		decide(&res, row)
		if res.Done && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
		out = append(out, res)
	}
	if len(ids) > 0 {
		if err := write(ids); err != nil {
			return nil, err
		}
		d.forget(ids)
	}
	return out, nil
}

// forget drops cached details of sessions whose state changed.
func (d *DB) forget(ids []string) {
	if d.cache == nil {
		return
	}
	gone := map[string]bool{}
	for _, id := range ids {
		gone[id] = true
	}
	d.cacheMu.Lock()
	defer d.cacheMu.Unlock()
	for ref, det := range d.cache {
		if gone[det.ID] {
			delete(d.cache, ref)
		}
	}
}
