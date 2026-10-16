package cli

import (
	"context"
	"errors"
	"strings"
	"time"
)

func init() {
	commands["tag"] = cmdTag
}

// cmdTag labels a session in ccli, or with --clear removes the label. It
// writes only to ccli's own index; transcripts are not touched.
func cmdTag(ctx context.Context, a *app, args []string) error {
	var c common
	fs := newFlags("tag", a, &c)
	clearTag := fs.Bool("clear", false, "remove the tag")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	switch {
	case len(pos) == 0, !*clearTag && len(pos) < 2, *clearTag && len(pos) > 1:
		return errors.New("usage: ccli tag <session> <tag>, or ccli tag <session> --clear")
	}
	tag := strings.Join(pos[1:], " ")
	if !*clearTag && strings.TrimSpace(tag) == "" {
		return errors.New("the tag is empty; ccli tag <session> --clear removes a tag")
	}
	svc, err := a.prepare(ctx, c, nil)
	if err != nil {
		return err
	}
	defer a.close()
	r, err := svc.Tag(ctx, pos[0], tag, time.Now())
	if err != nil {
		return err
	}
	if c.json {
		return a.printJSON(r)
	}
	a.term(c).Tagged(r)
	return nil
}
