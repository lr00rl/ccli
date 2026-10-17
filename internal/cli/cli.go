// Package cli parses commands and wires the packages together.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"

	"github.com/lr00rl/ccli/internal/account"
	"github.com/lr00rl/ccli/internal/index"
	"github.com/lr00rl/ccli/internal/pricing"
	"github.com/lr00rl/ccli/internal/store"
)

// Version is set at build time.
var Version = "dev"

type app struct {
	out, errw io.Writer
	accts     []account.Account
	st        *store.Store
	prices    *pricing.Table
}

// Main runs ccli and returns the exit code.
func Main(args []string, out, errw io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(out, usage)
		return 0
	}
	cmd, rest := args[0], args[1:]
	if cmd == "version" {
		fmt.Fprintln(out, "ccli", Version)
		return 0
	}
	run, ok := commands[cmd]
	if !ok {
		fmt.Fprintf(errw, "ccli: unknown command %q\n\n%s", cmd, usage)
		return 2
	}
	if err := run(ctx, &app{out: out, errw: errw}, rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		var ec exitCode
		if errors.As(err, &ec) {
			return int(ec)
		}
		fmt.Fprintln(errw, "ccli:", err)
		return 1
	}
	return 0
}

var commands = map[string]func(context.Context, *app, []string) error{
	"index": cmdIndex,
}

const usage = `ccli: Claude Code and Codex sessions across local accounts

Usage:
  ccli accounts                      accounts, identity, sessions, 30-day usage, live sessions
  ccli sessions [flags]              sessions with tokens and list-price cost (--dir . for this folder)
  ccli projects [folder]             folders sessions started in, at or below this one (/ for all)
  ccli show <id>                     one session: totals, models, subagents, links, files
  ccli log <id> [turn|N-M|last]      the session turn by turn: each prompt, what it cost, what it did
  ccli view [id]                     browse sessions, turns and agents interactively
  ccli prompts [flags]               your prompts across sessions, sortable by cost
  ccli stats [flags]                 overview: KPIs, daily chart, projects, models, hours, shape, skills
  ccli quota [-a account] [--offline] current limits, asked of Claude Code and Codex, and usage in them
  ccli diff <a> <b>                  sessions only in a, only in b, identical, ahead or diverged
  ccli copy|move <id>... --to <acct> copy a session (move also archives and removes the source)
  ccli copy|move --from a --to b     batch, dry run unless --apply
  ccli fork <id> --to <acct>         continue a session under a new id, in any account
  ccli resume <id>                   run claude --resume in the session's account and folder
  ccli resume <id> -a <acct> --sync  resume in another account, bringing its copy up to date first
  ccli hide|unhide <id>...           leave sessions out of lists, or bring them back (totals still count them)
  ccli tag <id> <tag>                label a session in ccli, shown next to its title (--clear removes it)
  ccli statusline -- <cmd> [args]    status line wrapper that records rate limits
  ccli index [--rebuild]             update the index (other commands do this first)
  ccli version

Run "ccli <command> -h" for a command's flags.
`

// open discovers accounts and opens the store and prices.
func (a *app) open() error {
	accts, err := account.Discover()
	if err != nil {
		return err
	}
	if len(accts) == 0 {
		return errors.New("no Claude Code or Codex home found (looked for ~/.claude, ~/.claude-*, ~/.codex and ~/.codex-*)")
	}
	a.accts = accts
	path, err := store.DefaultPath()
	if err != nil {
		return err
	}
	if a.st, err = store.Open(path); err != nil {
		return err
	}
	home, _ := os.UserHomeDir()
	cfg := os.Getenv("XDG_CONFIG_HOME")
	if cfg == "" {
		cfg = filepath.Join(home, ".config")
	}
	a.prices, err = pricing.Load(filepath.Join(cfg, "ccli", "prices.json"))
	return err
}

func (a *app) close() {
	if a.st != nil {
		a.st.Close()
	}
}

// refresh brings the index up to date, showing progress on a slow first run.
func (a *app) refresh(ctx context.Context, rebuild bool) (index.Stats, error) {
	tty := isTTY(a.errw)
	shown := false
	opt := index.Options{Rebuild: rebuild, QuotaDir: store.DataDir()}
	if tty {
		opt.Progress = func(done, total int, bytes int64) {
			if total < 20 && !shown {
				return
			}
			shown = true
			fmt.Fprintf(a.errw, "\rindexing %d/%d files, %s ", done, total, humanBytes(bytes))
		}
	}
	s, err := index.Run(ctx, a.st, a.accts, a.prices, opt)
	if shown {
		fmt.Fprint(a.errw, "\r\033[K")
	}
	if s.Errors > 0 {
		fmt.Fprintf(a.errw, "ccli: %d transcript file(s) could not be read and will be retried (first: %s)\n", s.Errors, s.FirstError)
	}
	return s, err
}

func cmdIndex(ctx context.Context, a *app, args []string) error {
	fs := flag.NewFlagSet("index", flag.ContinueOnError)
	fs.SetOutput(a.errw)
	rebuild := fs.Bool("rebuild", false, "re-read every transcript from the start")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := a.open(); err != nil {
		return err
	}
	defer a.close()
	s, err := a.refresh(ctx, *rebuild)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.out, "%d files, %d read (%s), %d re-read from the start, %d gone, in %s\n",
		s.Files, s.Parsed, humanBytes(s.Bytes), s.Reset, s.Missing, s.Duration.Round(1e7))
	return nil
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

func isTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}
