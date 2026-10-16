package query

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// MaxTag is the longest tag Tag accepts, in characters. A tag is a label
// shown in a column next to the title, not a second title.
const MaxTag = 40

// TagResult is what Tag did.
type TagResult struct {
	Account string `json:"account"`
	ID      string `json:"id"`
	Tag     string `json:"tag,omitempty"` // the tag now; empty once cleared
	Title   string `json:"title"`         // the session's own title
	Cleared bool   `json:"cleared,omitempty"`
}

// Tag labels a session in ccli, or with an empty tag removes the label. The
// session keeps its own title; the tag shows next to it. The tag lives only
// in ccli's index: no transcript is written and the session's last activity
// does not move. It is kept by session id, so it labels the session's copies
// in other accounts too.
func (d *DB) Tag(ctx context.Context, ref, tag string, now time.Time) (TagResult, error) {
	tag = strings.Join(strings.Fields(tag), " ")
	if n := utf8.RuneCountInString(tag); n > MaxTag {
		return TagResult{}, fmt.Errorf("a tag is at most %d characters; this one has %d", MaxTag, n)
	}
	acct, id, err := d.Resolve(ctx, ref)
	if err != nil {
		return TagResult{}, err
	}
	if tag == "" {
		err = d.st.ClearTag(ctx, id)
	} else {
		err = d.st.SetTag(ctx, id, tag, now)
	}
	if err != nil {
		return TagResult{}, err
	}
	d.forget() // cached details show the old tag
	rows, err := d.querySessions(ctx, Filter{Accounts: []string{acct}}, &skey{acct, id})
	if err != nil {
		return TagResult{}, err
	}
	if len(rows) == 0 {
		return TagResult{}, fmt.Errorf("%w %q", ErrNotFound, ref)
	}
	return TagResult{Account: acct, ID: id, Tag: rows[0].Tag, Title: rows[0].Title, Cleared: tag == ""}, nil
}
