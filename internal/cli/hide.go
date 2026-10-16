package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"time"

	"github.com/lr00rl/ccli/internal/query"
	"github.com/lr00rl/ccli/internal/render"
)

func init() {
	commands["hide"] = func(ctx context.Context, a *app, args []string) error { return cmdHide(ctx, a, args, true) }
	commands["unhide"] = func(ctx context.Context, a *app, args []string) error { return cmdHide(ctx, a, args, false) }
}

// cmdHide sets sessions aside so lists leave them out, or brings them back.
// It writes only a mark in ccli's own index; transcripts are not touched.
func cmdHide(ctx context.Context, a *app, args []string, hide bool) error {
	name, verb := "hide", "hidden"
	if !hide {
		name, verb = "unhide", "brought back"
	}
	var c common
	fs := newFlags(name, a, &c)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return fmt.Errorf("usage: ccli %s <session id or prefix>...", name)
	}
	svc, err := a.prepare(ctx, c, nil)
	if err != nil {
		return err
	}
	defer a.close()
	var rs []query.HideResult
	if hide {
		rs, err = svc.Hide(ctx, pos, time.Now())
	} else {
		rs, err = svc.Unhide(ctx, pos)
	}
	if err != nil {
		return err
	}
	if c.json {
		if err := a.printJSON(rs); err != nil {
			return err
		}
	} else {
		a.term(c).HideResults(verb, rs)
	}
	missing := 0
	for _, r := range rs {
		if r.ID == "" {
			missing++
		}
	}
	if missing > 0 {
		return errors.New(render.Plural(missing, "session") + " not found")
	}
	return nil
}

// hiddenFlag adds --hidden, which lists only the sessions set aside with
// `ccli hide` instead of leaving them out.
func hiddenFlag(fs *flag.FlagSet, f *query.Filter) {
	fs.BoolFunc("hidden", "list only sessions set aside with ccli hide", func(string) error {
		f.Show = query.HiddenOnly
		return nil
	})
}
