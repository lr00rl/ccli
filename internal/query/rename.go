package query

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// MaxTitle is the longest name Rename accepts, in characters.
const MaxTitle = 200

// RenameResult is what Rename did.
type RenameResult struct {
	Account   string `json:"account"`
	ID        string `json:"id"`
	Title     string `json:"title"`                // the title shown now
	ToolTitle string `json:"tool_title,omitempty"` // the tool's own title
	Cleared   bool   `json:"cleared,omitempty"`    // the name was removed
}

// Rename names a session in ccli, or with an empty name removes the name, so
// the tool's own title shows again. The name lives only in ccli's index: no
// transcript is written and the session's last activity does not move. It
// is kept by session id, so it names the session's copies in other accounts
// too, and it shows instead of the tool's title while it is set.
func (d *DB) Rename(ctx context.Context, ref, name string, now time.Time) (RenameResult, error) {
	name = strings.Join(strings.Fields(name), " ")
	if n := utf8.RuneCountInString(name); n > MaxTitle {
		return RenameResult{}, fmt.Errorf("a title is at most %d characters; this one has %d", MaxTitle, n)
	}
	acct, id, err := d.Resolve(ctx, ref)
	if err != nil {
		return RenameResult{}, err
	}
	if name == "" {
		err = d.st.ClearTitle(ctx, id)
	} else {
		err = d.st.SetTitle(ctx, id, name, now)
	}
	if err != nil {
		return RenameResult{}, err
	}
	d.forget() // cached details show the old title
	rows, err := d.querySessions(ctx, Filter{Accounts: []string{acct}}, &skey{acct, id})
	if err != nil {
		return RenameResult{}, err
	}
	if len(rows) == 0 {
		return RenameResult{}, fmt.Errorf("%w %q", ErrNotFound, ref)
	}
	r := RenameResult{Account: acct, ID: id, Title: rows[0].Title, ToolTitle: rows[0].ToolTitle, Cleared: name == ""}
	return r, nil
}
