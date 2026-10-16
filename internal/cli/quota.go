package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/term"

	"github.com/lr00rl/ccli/internal/account"
	"github.com/lr00rl/ccli/internal/quota"
	"github.com/lr00rl/ccli/internal/render"
	"github.com/lr00rl/ccli/internal/store"
)

func init() {
	commands["quota"] = cmdQuota
	commands["statusline"] = cmdStatusline
}

func cmdQuota(ctx context.Context, a *app, args []string) error {
	var c common
	var accts accountsFlag
	fs := newFlags("quota", a, &c)
	fs.Var(&accts, "a", "account (repeat or comma separate; default all)")
	offline := fs.Bool("offline", false, "show the last saved figures instead of asking Claude Code and Codex now")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	pick := func(all []account.Account) []account.Account {
		var out []account.Account
		for _, ac := range all {
			if len(accts) == 0 || contains(accts, ac.Name) {
				out = append(out, ac)
			}
		}
		return out
	}
	// The live checks run while the index is brought up to date; stopping
	// early (an unknown account, an index error) cancels them.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var checks chan map[string]liveCheck
	if !*offline {
		found, err := account.Discover()
		if err != nil {
			return err
		}
		checks = make(chan map[string]liveCheck, 1)
		go func() { checks <- checkLive(ctx, pick(found), store.DataDir()) }()
	}
	svc, err := a.prepare(ctx, c, accts)
	if err != nil {
		return err
	}
	defer a.close()
	snaps, err := quota.Load(store.DataDir())
	if err != nil {
		return err
	}
	chosen := pick(a.accts)
	var live map[string]liveCheck
	if checks != nil {
		// The tools take a few seconds to start and answer; say so on a
		// terminal, and clear the line once they have.
		f, ok := a.errw.(*os.File)
		note := ok && !c.json && term.IsTerminal(int(f.Fd()))
		if note {
			fmt.Fprint(f, "Asking each account's tool for its current limits (--offline skips this)…")
		}
		live = <-checks
		if note {
			fmt.Fprint(f, "\r\x1b[K")
		}
	}
	now := time.Now()
	var rows []render.QuotaRow
	for _, ac := range chosen {
		r := render.QuotaRow{Account: ac.Name, Email: ac.Email, Tool: ac.Kind(), Offline: *offline}
		var status, cc *quota.Snapshot
		if s, ok := snaps[ac.Name]; ok {
			status = &s
		}
		if l, ok := live[ac.Name]; ok {
			if l.err != nil {
				r.LiveError = l.err.Error()
			} else {
				status, r.Live = &l.snap, true
			}
		}
		if status != nil {
			r.Plan, r.Limits, r.Extra = status.Plan, status.Limits, status.Extra
			r.LimitsAt, r.LimitsFrom = status.CapturedAt, quota.SourceName(status.Source)
		}
		if ac.Kind() == account.Claude {
			if s, ok := quota.CclineCache(ac.Dir); ok {
				cc = &s
			}
		}
		r.FiveHour, r.SevenDay = quota.Latest(status, cc)
		// Usage is counted from the start of each window as the server
		// defines it: the reset time minus the window's length.
		for _, w := range []struct {
			rd     *quota.Reading
			length time.Duration
			use    *render.WindowUse
		}{{r.FiveHour, 5 * time.Hour, &r.Use5h}, {r.SevenDay, 7 * 24 * time.Hour, &r.Use7d}} {
			since := now.Add(-w.length)
			if rd := w.rd; rd != nil && !rd.ResetsAt.IsZero() {
				since = rd.ResetsAt.Add(-w.length)
				if rd.Expired(now) {
					since = rd.ResetsAt // a new window started at the reset
				}
				w.use.Exact = true
			}
			w.use.Since = since
			if w.use.Tokens, w.use.Cost, err = svc.Usage(ctx, ac.Name, since); err != nil {
				return err
			}
		}
		rows = append(rows, r)
	}
	if c.json {
		return a.printJSON(rows)
	}
	a.term(c).Quota(rows)
	return nil
}

// liveCheck is one account's answer to a live check.
type liveCheck struct {
	snap quota.Snapshot
	err  error
}

// checkLive asks each account's tool for its limits at once, in parallel,
// and saves each answer as the account's latest snapshot. The tools run in
// an empty folder of ccli's own (see quota.Checker).
func checkLive(ctx context.Context, accts []account.Account, dataDir string) map[string]liveCheck {
	c := quota.Checker{WorkDir: filepath.Join(dataDir, "tool-runs"), Client: Version}
	now := time.Now()
	out := make(map[string]liveCheck, len(accts))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, ac := range accts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := c.Check(ctx, ac, now)
			if err == nil {
				// The figures are shown either way; a failed save only
				// means --offline will not have them.
				_ = quota.Save(dataDir, s)
			}
			mu.Lock()
			out[ac.Name] = liveCheck{s, err}
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

const statuslineHelp = `Usage: ccli statusline -- <command> [args]

Wraps your status line command. Claude Code passes the status line a JSON
object on stdin that includes your subscription's rate limits; ccli records
them for "ccli quota", then runs <command> with the same input and prints its
output unchanged. ccli never edits settings.json. To use it, change your
statusLine command, for example:

  "statusLine": {"type": "command", "command": "ccli statusline -- ~/.claude/ccline/ccline"}
`

// cmdStatusline is on the status line's hot path: no index, no database,
// no identity lookups.
func cmdStatusline(_ context.Context, a *app, args []string) error {
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	} else if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Fprint(a.out, statuslineHelp)
		return nil
	}
	accts, _ := account.DiscoverDirs()
	code := quota.Wrap(args, os.Stdin, a.out, a.errw, store.DataDir(), accts, os.Getenv("CLAUDE_CONFIG_DIR"), time.Now())
	if code != 0 {
		return exitCode(code)
	}
	return nil
}

// exitCode carries a wrapped command's exit status out of Main.
type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
