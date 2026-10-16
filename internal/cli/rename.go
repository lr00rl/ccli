package cli

import (
	"context"
	"errors"
	"strings"
	"time"
)

func init() {
	commands["rename"] = cmdRename
}

// cmdRename names a session in ccli, or with --clear removes the name. It
// writes only to ccli's own index; transcripts are not touched.
func cmdRename(ctx context.Context, a *app, args []string) error {
	var c common
	fs := newFlags("rename", a, &c)
	clearName := fs.Bool("clear", false, "remove the name, so the tool's own title shows again")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	switch {
	case len(pos) == 0, !*clearName && len(pos) < 2, *clearName && len(pos) > 1:
		return errors.New("usage: ccli rename <session> <title>, or ccli rename <session> --clear")
	}
	name := strings.Join(pos[1:], " ")
	if !*clearName && strings.TrimSpace(name) == "" {
		return errors.New("the title is empty; ccli rename <session> --clear removes a name")
	}
	svc, err := a.prepare(ctx, c, nil)
	if err != nil {
		return err
	}
	defer a.close()
	r, err := svc.Rename(ctx, pos[0], name, time.Now())
	if err != nil {
		return err
	}
	if c.json {
		return a.printJSON(r)
	}
	a.term(c).Renamed(r)
	return nil
}
