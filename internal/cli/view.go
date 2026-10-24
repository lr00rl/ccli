package cli

import (
	"context"
	"errors"
	"os"

	"golang.org/x/term"

	"github.com/lr00rl/ccli/internal/query"
	"github.com/lr00rl/ccli/internal/tui"
)

func init() {
	commands["view"] = cmdView
}

// cmdView opens the interactive browser, at a session's turns when one is
// named.
func cmdView(ctx context.Context, a *app, args []string) error {
	var c common
	var scope query.Filter
	fs := newFlags("view", a, &c)
	dirFlag(fs, &scope)
	toolFlag(fs, &scope)
	tools := fs.Bool("tools", false, "expand tool calls inside each turn")
	in := fs.String("a", "", "with a session: its copy in this account (same as <account>:<id>); without one: list only this account")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return errors.New("usage: ccli view [-a account] [session]")
	}
	if len(pos) == 1 {
		if pos[0], err = withAccount(pos[0], *in); err != nil {
			return err
		}
	} else if *in != "" {
		scope.Accounts = []string{*in}
	}
	if len(pos) == 1 && (len(scope.Dirs) > 0 || len(scope.Tools) > 0) {
		return errors.New("-dir and -tool narrow the session list; leave them out when opening one session")
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return errors.New("ccli view needs a terminal; ccli log prints the same turns")
	}
	svc, err := a.prepare(ctx, c, accountsOf(*in))
	if err != nil {
		return err
	}
	defer a.close()
	ref := ""
	if len(pos) == 1 {
		acct, id, err := a.resolveReadSession(ctx, svc, pos[0], c)
		if err != nil {
			return err
		}
		ref = acct + ":" + id
		a.refreshCursorFor(ctx, []string{acct}, nil)
	} else {
		a.refreshCursorFor(ctx, nil, scope.Tools)
	}
	svc.CacheSessions()
	return tui.Run(ctx, svc, ref, scope, tui.Options{Tools: *tools || a.verbose})
}
