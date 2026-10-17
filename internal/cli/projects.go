package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/lr00rl/ccli/internal/account"
	"github.com/lr00rl/ccli/internal/query"
)

func init() {
	commands["projects"] = cmdProjects
}

// cmdProjects lists the folders sessions started in, at or below a folder
// (the current one by default), over all time unless -days or -since say
// otherwise.
func cmdProjects(ctx context.Context, a *app, args []string) error {
	var c common
	var f query.Filter
	var accts accountsFlag
	fs := newFlags("projects", a, &c)
	fs.Var(&accts, "a", "account (repeat or comma separate; default all)")
	days := fs.Int("days", 0, "projects with sessions active in the last N days (0 = all time)")
	since := fs.String("since", "", "projects with sessions active since a date (YYYY-MM-DD); overrides -days")
	toolFlag(fs, &f)
	fs.StringVar(&f.Sort, "sort", "last", "sort by last, cost, sessions or path")
	fs.IntVar(&f.Depth, "depth", 0, "group folders by their first N levels below the folder (0 = each folder)")
	fs.IntVar(&f.Limit, "n", 50, "show at most N projects (0 = all)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return errors.New("usage: ccli projects [folder]  (the current folder by default; / for all)")
	}
	switch f.Sort {
	case "last", "cost", "sessions", "path":
	default:
		return fmt.Errorf("-sort %q: use last, cost, sessions or path", f.Sort)
	}
	if f.Depth < 0 {
		return errors.New("-depth: use 0 for each folder, or a number of levels")
	}
	dir := "."
	if len(pos) == 1 {
		dir = pos[0]
	}
	if f.Dirs, err = scopeDirs(dir); err != nil {
		return err
	}
	if err := applyRange(&f, *days, *since); err != nil {
		return err
	}
	f.Accounts = accts
	svc, err := a.prepare(ctx, c, accts)
	if err != nil {
		return err
	}
	defer a.close()
	page, err := svc.Projects(ctx, f)
	if err != nil {
		return err
	}
	if c.json {
		return a.printJSON(page)
	}
	a.term(c).Projects(page)
	if page.Matched == 0 {
		// A deleted folder still has its sessions, so say when the folder
		// is missing too: it may be a typo.
		if _, err := os.Stat(f.Dirs[0]); errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "%s does not exist on disk either\n", f.Dirs[0])
		}
	}
	return nil
}

// dirFlag adds -dir: only sessions started in a folder or below it.
func dirFlag(fs *flag.FlagSet, f *query.Filter) {
	fs.Func("dir", "only sessions started in this folder or below (. for here; repeatable)", func(s string) error {
		dirs, err := scopeDirs(s)
		if err != nil {
			return err
		}
		f.Dirs = append(f.Dirs, dirs...)
		return nil
	})
}

// toolFlag adds -tool: only sessions of Claude Code or of Codex.
func toolFlag(fs *flag.FlagSet, f *query.Filter) {
	fs.Func("tool", "only sessions of this agent: claude or codex (repeat or comma separate)", func(s string) error {
		for _, t := range strings.Split(s, ",") {
			switch tool := account.Tool(strings.TrimSpace(t)); tool {
			case "":
			case account.Claude, account.Codex:
				f.Tools = append(f.Tools, tool)
			default:
				return fmt.Errorf("%q is not claude or codex", tool)
			}
		}
		return nil
	})
}

// scopeDirs turns a folder argument into the forms a recorded cwd may take:
// the absolute path as given, and the same path with symlinks resolved,
// since the agents record the physical path (macOS's /tmp is /private/tmp).
// The folder need not exist: sessions outlive the folders they ran in.
func scopeDirs(arg string) ([]string, error) {
	if arg == "" {
		return nil, errors.New("empty folder")
	}
	if strings.HasPrefix(arg, "~") && arg != "~" && !strings.HasPrefix(arg, "~/") {
		return nil, fmt.Errorf("%s: only ~ and ~/ are expanded, not another user's home", arg)
	}
	if arg == "~" || strings.HasPrefix(arg, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		arg = filepath.Join(home, strings.TrimPrefix(arg, "~"))
	}
	abs, err := filepath.Abs(arg)
	if err != nil {
		return nil, err
	}
	out := []string{abs}
	if real := resolveExisting(abs); real != abs {
		out = append(out, real)
	}
	return out, nil
}

// resolveExisting resolves symlinks in the longest part of p that exists
// and keeps the rest as written.
func resolveExisting(p string) string {
	rest := ""
	for d := p; ; d = filepath.Dir(d) {
		if real, err := filepath.EvalSymlinks(d); err == nil {
			return filepath.Join(real, rest)
		}
		if parent := filepath.Dir(d); parent == d {
			return p
		}
		rest = filepath.Join(filepath.Base(d), rest)
	}
}
