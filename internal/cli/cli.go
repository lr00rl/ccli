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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lr00rl/ccli/internal/cursorusage"

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
	debug     debugLevel
	verbose   bool

	cursorOnce sync.Once
	cursorLed  *cursorusage.Ledger
	cursorErr  error
}

// debugLevel is how much of ccli's own work to print on stderr.
// 1 is the steps, 2 names each request, 3 adds every file, 4 prints
// the request and the response.
type debugLevel int

func (d *debugLevel) String() string   { return strconv.Itoa(int(*d)) }
func (d *debugLevel) IsBoolFlag() bool { return true }
func (d *debugLevel) Set(s string) error {
	if s == "false" {
		return nil
	}
	if s == "true" {
		if *d < 1 {
			*d = 1
		}
		return nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 || n > 4 {
		return errors.New("--debug: use 1, 2, 3 or 4")
	}
	if debugLevel(n) > *d {
		*d = debugLevel(n)
	}
	return nil
}

func (a *app) logf(level int, format string, args ...any) {
	if int(a.debug) < level {
		return
	}
	text := fmt.Sprintf(format, args...)
	text = strings.TrimRight(text, "\n")
	for line := range strings.SplitSeq(text, "\n") {
		fmt.Fprintf(a.errw, "debug: %s\n", line)
	}
}

func (a *app) registerGlobals(fs *flag.FlagSet) {
	fs.Var(&a.debug, "debug", "what ccli did, on stderr: 1 steps, 2 each request, 3 every file, 4 request and response")
	fs.BoolVar(&a.verbose, "verbose", a.verbose, "show more of what was requested: token split, paths, models, daily figures")
}

// peelGlobals takes --debug and --verbose that appear before the command.
func (a *app) peelGlobals(args []string) ([]string, error) {
	i := 0
	for i < len(args) {
		s := args[i]
		switch {
		case s == "--":
			return args, nil
		case s == "--debug" || s == "-debug":
			if err := a.debug.Set("true"); err != nil {
				return nil, err
			}
		case strings.HasPrefix(s, "--debug="):
			if err := a.debug.Set(strings.TrimPrefix(s, "--debug=")); err != nil {
				return nil, err
			}
		case s == "--verbose" || s == "-verbose" || s == "--verbose=true":
			a.verbose = true
		case s == "--verbose=false":
		default:
			return args[i:], nil
		}
		i++
	}
	return args[i:], nil
}

// Main runs ccli and returns the exit code.
func Main(args []string, out, errw io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	a := &app{out: out, errw: errw}
	peeled, err := a.peelGlobals(args)
	if err != nil {
		fmt.Fprintln(errw, "ccli:", err)
		return 2
	}
	args = peeled
	start := time.Now()
	defer func() {
		a.logf(1, "finished in %s", time.Since(start).Round(time.Millisecond))
	}()
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
	a.logf(1, "command %s", cmd)
	if err := run(ctx, a, rest); err != nil {
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

const usage = `ccli: Claude Code, Codex, Pi and Cursor sessions across local accounts

Usage:
  ccli accounts                      accounts, identity, sessions, 30-day usage, live sessions
  ccli sessions [flags]              sessions with tokens and list-price cost (--dir . for this folder)
  ccli projects [folder]             folders sessions started in, at or below this one (/ for all)
  ccli show <id>                     one session: totals, models, subagents, links, files
  ccli log <id> [turn|N-M|last]      the session turn by turn: each prompt, what it cost, what it did
  ccli export <id> [turn|N-M|last]   write a readable transcript (md or jsonl) for handoff
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

  --debug[=1|2|3|4]   what ccli did, on stderr. 1 steps, 2 each request, 3 every file, 4 the request and its response
  --verbose         more of the data you asked about: token split, paths, models, daily figures

Run "ccli <command> -h" for a command's flags.
`

// open discovers accounts and opens the store and prices.
func (a *app) open() error {
	accts, err := account.Discover()
	if err != nil {
		return err
	}
	if len(accts) == 0 {
		return errors.New("no agent home found (looked for ~/.claude, ~/.claude-*, ~/.codex, ~/.codex-*, ~/.pi/agent, ~/.cursor and the Cursor IDE user dir)")
	}
	a.accts = accts
	a.logf(1, "%d accounts", len(accts))
	for _, ac := range accts {
		a.logf(2, "account %s (%s) %s", ac.Name, ac.Kind(), ac.Dir)
	}
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
	if tty && a.debug == 0 {
		opt.Progress = func(done, total int, bytes int64) {
			if total < 20 && !shown {
				return
			}
			shown = true
			fmt.Fprintf(a.errw, "\rindexing %d/%d files, %s ", done, total, humanBytes(bytes))
		}
	}
	if a.debug >= 3 {
		opt.Trace = func(path string, read, reset bool) {
			if read {
				a.logf(3, "read %s reset=%t", path, reset)
				return
			}
			a.logf(3, "unchanged %s", path)
		}
	}
	s, err := index.Run(ctx, a.st, a.accts, a.prices, opt)
	if shown {
		fmt.Fprint(a.errw, "\r\033[K")
	}
	if s.Errors > 0 {
		fmt.Fprintf(a.errw, "ccli: %d transcript file(s) could not be read and will be retried (first: %s)\n", s.Errors, s.FirstError)
	}
	a.logf(1, "index: %d files, %d read, %d re-read, %d gone, %d errors, %s",
		s.Files, s.Parsed, s.Reset, s.Missing, s.Errors, s.Duration.Round(time.Millisecond))
	return s, err
}

func cmdIndex(ctx context.Context, a *app, args []string) error {
	fs := flag.NewFlagSet("index", flag.ContinueOnError)
	fs.SetOutput(a.errw)
	rebuild := fs.Bool("rebuild", false, "re-read every transcript from the start")
	a.registerGlobals(fs)
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
