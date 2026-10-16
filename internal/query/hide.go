package query

import (
	"context"
	"fmt"
	"time"

	"github.com/lr00rl/ccli/internal/account"
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
	ID      string `json:"id,omitempty"` // empty when the reference matched no single session
	Title   string `json:"title,omitempty"`
	Done    bool   `json:"done"`
	Reason  string `json:"reason,omitempty"` // why it was left as it was
}

// Hide sets sessions aside, so lists leave them out until they are used
// again. A session a Claude Code process is running is left alone: it would
// show again with its next line. Hiding marks the session id, so its copies
// in other accounts are hidden with it.
func (d *DB) Hide(ctx context.Context, refs []string, now time.Time) ([]HideResult, error) {
	running := map[string]bool{}
	for _, l := range account.LiveSessions(d.accts) {
		running[l.SessionID] = true
	}
	marks := map[string]time.Time{}
	rs, err := d.forRefs(ctx, refs, func(r *HideResult, s session) {
		switch {
		case running[s.id]:
			r.Reason = "running; hide it once it has ended"
		case s.hidden:
			r.Reason = "already hidden"
		default:
			r.Done = true
			// A transcript dated ahead of this clock (a copy from another
			// machine) would otherwise stay newer than its mark.
			marks[s.id] = maxTime(now, s.last)
		}
	})
	if err != nil {
		return nil, err
	}
	if len(marks) > 0 {
		if err := d.st.Hide(ctx, marks); err != nil {
			return nil, err
		}
		d.forget()
	}
	return rs, nil
}

// Unhide brings hidden sessions back into lists.
func (d *DB) Unhide(ctx context.Context, refs []string) ([]HideResult, error) {
	var ids []string
	rs, err := d.forRefs(ctx, refs, func(r *HideResult, s session) {
		if !s.hidden {
			r.Reason = "not hidden"
			return
		}
		r.Done = true
		ids = append(ids, s.id)
	})
	if err != nil {
		return nil, err
	}
	if len(ids) > 0 {
		if err := d.st.Unhide(ctx, ids); err != nil {
			return nil, err
		}
		d.forget()
	}
	return rs, nil
}

// session is what deciding to hide or unhide needs to know about one
// session id, over all its copies.
type session struct {
	id     string
	hidden bool      // every copy is hidden
	last   time.Time // the latest activity of any copy
}

// forRefs resolves each reference and calls decide once per session id; a
// reference that matches nothing, matches several sessions or repeats an
// earlier one gets a reason instead.
func (d *DB) forRefs(ctx context.Context, refs []string, decide func(*HideResult, session)) ([]HideResult, error) {
	rows, err := d.loadSessions(ctx, Filter{})
	if err != nil {
		return nil, err
	}
	byRef := map[skey]SessionRow{}
	byID := map[string]*session{}
	for _, r := range rows {
		byRef[skey{r.Account, r.ID}] = r
		s := byID[r.ID]
		if s == nil {
			s = &session{id: r.ID, hidden: true}
			byID[r.ID] = s
		}
		s.hidden = s.hidden && r.Hidden
		s.last = maxTime(s.last, r.Last)
	}
	var out []HideResult
	seen := map[string]bool{}
	for _, ref := range refs {
		res := HideResult{Ref: ref}
		acct, id, err := d.Resolve(ctx, ref)
		if err != nil {
			// A reference that is too short, ambiguous or unknown fails on
			// its own; the others still go ahead.
			res.Reason = err.Error()
			out = append(out, res)
			continue
		}
		row, ok := byRef[skey{acct, id}]
		if !ok {
			return nil, fmt.Errorf("session %s is in the index but has no files", ShortID(id))
		}
		res.Account, res.ID, res.Title = acct, id, row.Title
		if seen[id] {
			res.Reason = "named more than once"
		} else {
			seen[id] = true
			decide(&res, *byID[id])
		}
		out = append(out, res)
	}
	return out, nil
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// forget drops every cached session detail, since a detail shows whether
// its session is hidden, and stops details loaded before now from being
// cached.
func (d *DB) forget() {
	if d.cache == nil {
		return
	}
	d.cacheMu.Lock()
	defer d.cacheMu.Unlock()
	clear(d.cache)
	d.cacheGen++
}
