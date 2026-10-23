package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/lr00rl/ccli/internal/account"
	"github.com/lr00rl/ccli/internal/cursorusage"
	"github.com/lr00rl/ccli/internal/query"
	"github.com/lr00rl/ccli/internal/render"
	"github.com/lr00rl/ccli/internal/store"
)

func init() {
	commands["accounts"] = cmdAccounts
	commands["sessions"] = cmdSessions
	commands["ls"] = cmdSessions
	commands["show"] = cmdShow
	commands["stats"] = cmdStats
	commands["resume"] = cmdResume
}

// common holds the flags every view takes.
type common struct {
	json    bool
	noIndex bool
	color   string
}

func (c *common) register(fs *flag.FlagSet) {
	fs.BoolVar(&c.json, "json", false, "print JSON")
	fs.BoolVar(&c.noIndex, "no-index", false, "skip updating the index first")
	fs.StringVar(&c.color, "color", "auto", "color: auto, always or never")
}

// accountsFlag collects repeated -a values (comma separated allowed).
type accountsFlag []string

func (a *accountsFlag) String() string { return strings.Join(*a, ",") }
func (a *accountsFlag) Set(v string) error {
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			*a = append(*a, s)
		}
	}
	return nil
}

func newFlags(name string, a *app, c *common) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(a.errw)
	c.register(fs)
	a.registerGlobals(fs)
	return fs
}

// prepare opens everything and refreshes the index. It does not ask Cursor
// for usage; commands that show Cursor cost call refreshCursorFor.
func (a *app) prepare(ctx context.Context, c common, accts []string) (*query.DB, error) {
	return a.prepareQuery(ctx, c, accts, false)
}

// prepareQuery is prepare. cursorEvents loads the usage-event ledger. quota
// does not: a billing-cycle check is a different, smaller request.
func (a *app) prepareQuery(ctx context.Context, c common, accts []string, cursorEvents bool) (*query.DB, error) {
	if err := a.open(); err != nil {
		return nil, err
	}
	for _, n := range accts {
		found := false
		for _, x := range a.accts {
			if x.Name == n {
				found = true
			}
		}
		if !found {
			a.close()
			return nil, fmt.Errorf("no account named %q", n)
		}
	}
	if !c.noIndex {
		if _, err := a.refresh(ctx, false); err != nil {
			a.close()
			return nil, err
		}
	}
	if cursorEvents {
		a.refreshCursor(ctx)
	}
	return query.New(a.st, a.accts), nil
}

// refreshCursorFor reads the usage-event ledger when the command can show a
// Cursor or Cursor IDE account. A Claude-only filter does not ask Cursor.
func (a *app) refreshCursorFor(ctx context.Context, names []string, tools []account.Tool) {
	if !a.needsCursor(names, tools) {
		a.logf(2, "cursor usage: not needed")
		return
	}
	a.refreshCursor(ctx)
}

func (a *app) needsCursor(names []string, tools []account.Tool) bool {
	if len(tools) > 0 {
		keep := false
		for _, t := range tools {
			if t == account.Cursor || t == account.CursorIDE {
				keep = true
			}
		}
		if !keep {
			return false
		}
	}
	pool := a.accts
	if len(names) > 0 {
		pool = nil
		for _, n := range names {
			for _, ac := range a.accts {
				if ac.Name == n {
					pool = append(pool, ac)
				}
			}
		}
	}
	for _, ac := range pool {
		if ac.Kind() == account.Cursor || ac.Kind() == account.CursorIDE {
			return true
		}
	}
	return false
}

// pullCursor reads the event ledger once per command. A fresh cache is not
// fetched again. Debug only reports that choice.
func (a *app) pullCursor(ctx context.Context, dataDir string) (*cursorusage.Ledger, error) {
	a.cursorOnce.Do(func() {
		opt := cursorusage.Option{Asking: func() {
			fmt.Fprintln(a.errw, "Reading Cursor usage. This asks Cursor and can take a little while.")
		}}
		if a.debug >= 2 {
			opt.Trace = func(format string, args ...any) { a.logf(2, format, args...) }
		}
		if a.debug >= 4 {
			opt.Body = func(format string, args ...any) { a.logf(4, format, args...) }
		}
		a.cursorLed, a.cursorErr = cursorusage.RefreshOption(ctx, dataDir, opt)
	})
	return a.cursorLed, a.cursorErr
}

func (a *app) refreshCursor(ctx context.Context) {
	led, err := a.pullCursor(ctx, store.DataDir())
	if led != nil {
		from := "fetched"
		if led.FromCache {
			from = "cache"
		}
		note := ""
		if led.Truncated {
			note = ", truncated"
		}
		a.logf(1, "cursor usage: %s, %d conversations, %d events%s", from, len(led.Sessions), led.EventCount, note)
		if led.Truncated && !led.FromCache && err == nil {
			fmt.Fprintf(a.errw, "ccli: Cursor's usage list stopped at %d events, so Cursor figures for the last year are incomplete\n", led.EventCount)
		}
		if p := led.Period; p != nil {
			a.logf(2, "cursor cycle: %s included $%.2f of $%.2f (%.1f%%), API %.1f%%, total %.1f%%",
				p.Plan, p.UsedUSD, p.LimitUSD, p.Percent, p.APIPercent, p.TotalPercent)
		}
	}
	if err != nil {
		a.logf(1, "cursor usage: %s", err)
	}
	// Without an IDE login there is nothing to ask; that is not an error.
	if err == nil || led == nil || errors.Is(err, cursorusage.ErrNoLogin) {
		return
	}
	if len(led.Sessions) > 0 || led.Period != nil {
		return
	}
	fmt.Fprintf(a.errw, "ccli: %s\n", err)
}

func (a *app) term(c common) render.Term {
	t := render.NewTerm(a.out, c.color)
	t.Verbose = a.verbose
	return t
}

func (a *app) printJSON(v any) error {
	enc := json.NewEncoder(a.out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func cmdAccounts(ctx context.Context, a *app, args []string) error {
	var c common
	fs := newFlags("accounts", a, &c)
	days := fs.Int("days", 30, "usage window in days (0 = all time)")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	svc, err := a.prepare(ctx, c, nil)
	if err != nil {
		return err
	}
	defer a.close()
	f := query.Filter{Loc: time.Local}
	f.Since, f.Until = query.RangeDays(*days, time.Local)
	a.refreshCursorFor(ctx, nil, nil)
	rows, err := svc.Accounts(ctx, f)
	if err != nil {
		return err
	}
	if c.json {
		return a.printJSON(rows)
	}
	a.term(c).Accounts(rows, *days)
	return nil
}

func sessionFlags(fs *flag.FlagSet, f *query.Filter, accts *accountsFlag) (*int, *string) {
	fs.Var(accts, "a", "account (repeat or comma separate; default all)")
	days := fs.Int("days", 30, "sessions active in the last N days (0 = all time)")
	since := fs.String("since", "", "sessions active since a date (YYYY-MM-DD); overrides -days")
	fs.StringVar(&f.Project, "project", "", "project path contains")
	dirFlag(fs, f)
	toolFlag(fs, f)
	fs.StringVar(&f.Model, "model", "", "model contains")
	fs.StringVar(&f.Search, "q", "", "title, path or id prefix contains")
	return days, since
}

func applyRange(f *query.Filter, days int, since string) error {
	f.Loc = time.Local
	if since != "" {
		t, err := time.ParseInLocation("2006-01-02", since, time.Local)
		if err != nil {
			return fmt.Errorf("-since: %w", err)
		}
		f.Since = t
		return nil
	}
	f.Since, f.Until = query.RangeDays(days, time.Local)
	return nil
}

func cmdSessions(ctx context.Context, a *app, args []string) error {
	var c common
	var f query.Filter
	var accts accountsFlag
	fs := newFlags("sessions", a, &c)
	days, since := sessionFlags(fs, &f, &accts)
	fs.StringVar(&f.Sort, "sort", "last", "sort by last, start, cost, tokens or active")
	fs.IntVar(&f.Limit, "n", 40, "show at most N sessions (0 = all)")
	hiddenFlag(fs, &f)
	fs.BoolVar(&f.Drafts, "drafts", false, "include empty Cursor IDE drafts")
	if _, err := parse(fs, args); err != nil {
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
	a.refreshCursorFor(ctx, f.Accounts, f.Tools)
	page, err := svc.Sessions(ctx, f)
	if err != nil {
		return err
	}
	if c.json {
		return a.printJSON(page)
	}
	a.term(c).Sessions(page, len(accts) != 1 && len(a.accts) > 1)
	return nil
}

func cmdShow(ctx context.Context, a *app, args []string) error {
	var c common
	fs := newFlags("show", a, &c)
	in := copyFlag(fs)
	all := fs.Bool("all", false, "list every subagent, and every file a Cursor agent wrote")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: ccli show [-a account] <session id or prefix>")
	}
	ref, err := withAccount(pos[0], *in)
	if err != nil {
		return err
	}
	svc, err := a.prepare(ctx, c, accountsOf(*in))
	if err != nil {
		return err
	}
	defer a.close()
	acct, id, err := svc.Resolve(ctx, ref)
	if err != nil {
		return err
	}
	a.noteCopy(ctx, svc, ref, acct, id)
	a.refreshCursorFor(ctx, []string{acct}, nil)
	d, err := svc.Session(ctx, ref)
	if err != nil {
		return err
	}
	if c.json {
		return a.printJSON(d)
	}
	a.term(c).Session(d, *all)
	return nil
}

func cmdStats(ctx context.Context, a *app, args []string) error {
	var c common
	var f query.Filter
	var accts accountsFlag
	fs := newFlags("stats", a, &c)
	days, since := sessionFlags(fs, &f, &accts)
	if _, err := parse(fs, args); err != nil {
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
	a.refreshCursorFor(ctx, f.Accounts, f.Tools)
	o, err := svc.Overview(ctx, f)
	if err != nil {
		return err
	}
	if c.json {
		return a.printJSON(o)
	}
	shown := *days
	if *since != "" {
		shown = -1
	}
	a.term(c).Overview(o, shown)
	return nil
}

// cmdResume runs `claude --resume` for the session, in its folder and with
// its account's CLAUDE_CONFIG_DIR. With -a it first brings that account's
// copy up to date when another account holds a newer one, so the resumed
// conversation is never an old copy.
func cmdResume(ctx context.Context, a *app, args []string) error {
	var c common
	fs := newFlags("resume", a, &c)
	printOnly := fs.Bool("print", false, "print the command instead of running it (with -a, the copy is still brought up to date)")
	fork := fs.Bool("fork", false, "resume into a new session id (--fork-session)")
	in := fs.String("a", "", "resume in this account, first bringing its copy up to date from a newer one")
	noSync := fs.Bool("no-sync", false, "with -a: resume that account's copy only if it is already up to date")
	syncCopy := fs.Bool("sync", true, "with -a: bring that account's copy up to date first (the default)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: ccli resume [-a account [--no-sync]] [-print] [-fork] <session id or prefix>")
	}
	if *noSync && flagSet(fs, "sync") {
		return errors.New("give --sync or --no-sync, not both")
	}
	svc, err := a.prepare(ctx, c, nil)
	if err != nil {
		return err
	}
	ref := pos[0]
	if *in != "" {
		_, id, err := svc.Resolve(ctx, ref)
		if err != nil {
			a.close()
			return err
		}
		if err := a.syncFor(ctx, id, *in, *syncCopy && !*noSync); err != nil {
			a.close()
			return err
		}
		svc = query.New(a.st, a.accts)
		ref = *in + ":" + id
	}
	d, err := svc.Session(ctx, ref)
	a.close()
	if err != nil {
		return err
	}
	r := d.Resume
	if *fork && r.ForkArg == "" {
		return fmt.Errorf("%s has no way to resume a session under a new id", r.Program)
	}
	cmdline := r.Command
	if *fork {
		cmdline += " " + r.ForkArg
	}
	if *printOnly || c.json {
		if c.json {
			return a.printJSON(r)
		}
		fmt.Fprintln(a.out, cmdline)
		return nil
	}
	if !r.Available && !*fork {
		return fmt.Errorf("cannot resume %s: %s (use -fork to branch it, or -print)", query.ShortID(d.ID), r.Reason)
	}
	if d.Missing {
		return fmt.Errorf("cannot resume %s: %s", query.ShortID(d.ID), r.Reason)
	}
	var others []string
	for _, l := range d.Links {
		if l.Kind == "copy" {
			others = append(others, l.Account)
		}
	}
	if len(others) > 0 {
		fmt.Fprintf(a.errw, "ccli: %s is also in %s; resuming the copy in %s (latest activity); use <account>:<id> for another\n",
			query.ShortID(d.ID), strings.Join(others, ", "), d.Account)
	}
	bin, err := exec.LookPath(r.Program)
	if err != nil {
		return fmt.Errorf("%s is not on PATH", r.Program)
	}
	argv := append([]string{r.Program}, r.Args...)
	if *fork {
		argv = append(argv, r.ForkArg)
	}
	env := withoutVar(os.Environ(), r.Env)
	if r.EnvValue != "" {
		env = append(env, r.Env+"="+r.EnvValue)
	}
	if r.Cwd != "" {
		if err := os.Chdir(r.Cwd); err != nil {
			return fmt.Errorf("cd %s: %w", r.Cwd, err)
		}
	}
	return syscall.Exec(bin, argv, env)
}

func withoutVar(env []string, name string) []string {
	out := env[:0:0]
	for _, kv := range env {
		if !strings.HasPrefix(kv, name+"=") {
			out = append(out, kv)
		}
	}
	return out
}

// copyFlag adds -a to a command that reads one session: its copy in that
// account, when the session is in more than one. It means the same as
// writing the reference as <account>:<id>.
func copyFlag(fs *flag.FlagSet) *string {
	return fs.String("a", "", "the session's copy in this account, when it is in more than one (same as <account>:<id>)")
}

// withAccount writes an -a account into a session reference. It refuses a
// reference that already names another account.
func withAccount(ref, acct string) (string, error) {
	if acct == "" {
		return ref, nil
	}
	if i := strings.IndexByte(ref, ':'); i > 0 {
		if ref[:i] != acct {
			return "", fmt.Errorf("-a %s and %s name different accounts; give one", acct, ref)
		}
		return ref, nil
	}
	return acct + ":" + ref, nil
}

// accountsOf is the account list prepare checks: the -a account, if any.
func accountsOf(acct string) []string {
	if acct == "" {
		return nil
	}
	return []string{acct}
}

// noteCopy says on stderr which copy a reference that names no account
// resolved to, when the session is in more than one account.
func (a *app) noteCopy(ctx context.Context, svc *query.DB, ref, acct, id string) {
	if strings.Contains(ref, ":") {
		return
	}
	all, err := svc.CopiesOf(ctx, id)
	if err != nil {
		return
	}
	var others []string
	for _, o := range all {
		if o != acct {
			others = append(others, o)
		}
	}
	if len(others) == 0 {
		return
	}
	pick := others[0]
	if len(others) > 1 {
		pick = "<account>"
	}
	fmt.Fprintf(a.errw, "ccli: %s is also in %s; this is the copy in %s, the one used last (-a %s picks another)\n",
		query.ShortID(id), strings.Join(others, ", "), acct, pick)
}

// flagSet reports whether the command line gave the flag.
func flagSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) { set = set || f.Name == name })
	return set
}

// parse accepts flags before and after positional arguments ("show ID
// --json"), stopping at "--".
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		if len(args) > len(rest) && args[len(args)-len(rest)-1] == "--" {
			return append(pos, rest...), nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}
