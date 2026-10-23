package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/lr00rl/ccli/internal/account"
	"github.com/lr00rl/ccli/internal/query"
	"github.com/lr00rl/ccli/internal/render"
	"github.com/lr00rl/ccli/internal/store"
	"github.com/lr00rl/ccli/internal/transfer"
)

func init() {
	commands["diff"] = cmdDiff
	commands["copy"] = func(ctx context.Context, a *app, args []string) error {
		return cmdTransfer(ctx, a, args, transfer.Copy)
	}
	commands["move"] = func(ctx context.Context, a *app, args []string) error {
		return cmdTransfer(ctx, a, args, transfer.Move)
	}
	commands["fork"] = cmdFork
}

// claudeAccount finds a Claude Code account by name: copying, moving, forking
// and syncing work on Claude Code transcripts only.
func (a *app) claudeAccount(name string) (account.Account, error) {
	ac, err := account.Find(a.accts, name)
	if err == nil && ac.Kind() != account.Claude {
		err = fmt.Errorf("%s is a %s home; diff, copy, move, fork and resume -a work between Claude Code accounts", ac.Name, ac.Kind())
	}
	return ac, err
}

// sessionInfo maps account/session to its list row, for titles and activity.
func sessionInfo(ctx context.Context, svc *query.DB, accts ...string) (map[string]query.SessionRow, error) {
	page, err := svc.Sessions(ctx, query.Filter{Accounts: accts})
	if err != nil {
		return nil, err
	}
	out := map[string]query.SessionRow{}
	for _, s := range page.Sessions {
		out[s.Account+"/"+s.ID] = s
	}
	return out, nil
}

func cmdDiff(ctx context.Context, a *app, args []string) error {
	var c common
	fs := newFlags("diff", a, &c)
	project := fs.String("project", "", "only projects whose folder name contains this")
	all := fs.Bool("all", false, "include identical sessions")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return errors.New("usage: ccli diff <account a> <account b>")
	}
	svc, err := a.prepare(ctx, c, pos)
	if err != nil {
		return err
	}
	defer a.close()
	acA, err := a.claudeAccount(pos[0])
	if err != nil {
		return err
	}
	acB, err := a.claudeAccount(pos[1])
	if err != nil {
		return err
	}
	entries, err := transfer.Compare(acA, acB)
	if err != nil {
		return err
	}
	info, err := sessionInfo(ctx, svc, acA.Name, acB.Name)
	if err != nil {
		return err
	}
	var rows []render.DiffRow
	for _, e := range entries {
		if *project != "" && !strings.Contains(strings.ToLower(e.ProjectKey), strings.ToLower(*project)) {
			continue
		}
		if e.State == transfer.Same && !*all {
			continue
		}
		r := render.DiffRow{Entry: e}
		for _, acct := range []string{acA.Name, acB.Name} {
			if s, ok := info[acct+"/"+e.SessionID]; ok {
				if r.Title == "" {
					r.Title = s.Title
				}
				if s.Last.After(r.Last) {
					r.Last = s.Last
				}
				r.Project = s.Project
				r.Live = r.Live || s.Live
			}
		}
		rows = append(rows, r)
	}
	if c.json {
		return a.printJSON(rows)
	}
	a.term(c).Diff(acA.Name, acB.Name, rows, len(entries))
	return nil
}

func cmdTransfer(ctx context.Context, a *app, args []string, mode transfer.Mode) error {
	name := map[transfer.Mode]string{transfer.Copy: "copy", transfer.Move: "move"}[mode]
	var c common
	fs := newFlags(name, a, &c)
	to := fs.String("to", "", "target account (required)")
	from := fs.String("from", "", "source account (required for a batch; found automatically for named sessions)")
	project := fs.String("project", "", "batch: only projects whose folder name contains this")
	include := fs.String("include", "missing,ahead", "batch: which sessions, any of missing (only in source), ahead (source is longer)")
	days := fs.Int("days", 0, "batch: only sessions active in the last N days (0 = any)")
	apply := fs.Bool("apply", false, "carry out the plan (without it nothing is written)")
	ids, err := parse(fs, args)
	if err != nil {
		return err
	}
	if *to == "" {
		return fmt.Errorf("usage: ccli %s <session>... --to <account> [--apply]\n       ccli %s --from <account> --to <account> [--project p] [--apply]", name, name)
	}
	svc, err := a.prepare(ctx, c, nil)
	if err != nil {
		return err
	}
	defer a.close()
	target, err := a.claudeAccount(*to)
	if err != nil {
		return err
	}
	opt := transfer.Options{
		// Read afresh for every plan and apply: a session may start while a
		// batch runs.
		Live: func(id string) bool {
			for _, l := range account.LiveSessions(a.accts) {
				if l.SessionID == id {
					return true
				}
			}
			return false
		},
		ArchiveDir: filepath.Join(store.DataDir(), "archive"),
	}

	type pick struct {
		id     string
		source account.Account
	}
	var picks []pick
	if len(ids) > 0 {
		for _, ref := range ids {
			acct, id, err := svc.Resolve(ctx, ref)
			if err != nil {
				return err
			}
			src := acct
			if *from != "" {
				src = *from
			} else if acct == target.Name {
				// The newest copy is already in the target; look for it elsewhere.
				src = ""
				for _, ac := range a.accts {
					if ac.Kind() != account.Claude {
						continue
					}
					if ac.Name == target.Name {
						continue
					}
					if _, err := transfer.Locate(ac, id); err == nil {
						if src != "" {
							return fmt.Errorf("%s is in several accounts; name the source with --from", query.ShortID(id))
						}
						src = ac.Name
					}
				}
				if src == "" {
					return fmt.Errorf("%s is only in %s", query.ShortID(id), target.Name)
				}
			}
			ac, err := a.claudeAccount(src)
			if err != nil {
				return err
			}
			picks = append(picks, pick{id, ac})
		}
	} else {
		if *from == "" {
			return errors.New("name sessions, or give --from for a batch")
		}
		src, err := a.claudeAccount(*from)
		if err != nil {
			return err
		}
		entries, err := transfer.Compare(src, target)
		if err != nil {
			return err
		}
		want := map[transfer.State]bool{}
		for _, w := range strings.Split(*include, ",") {
			switch strings.TrimSpace(w) {
			case "missing":
				want[transfer.OnlyA] = true
			case "ahead":
				want[transfer.AAhead] = true
			case "":
			default:
				return fmt.Errorf("--include: unknown value %q (use missing, ahead)", w)
			}
		}
		info, err := sessionInfo(ctx, svc, src.Name)
		if err != nil {
			return err
		}
		cutoff := time.Time{}
		if *days > 0 {
			cutoff = time.Now().AddDate(0, 0, -*days)
		}
		for _, e := range entries {
			if !want[e.State] {
				continue
			}
			if *project != "" && !strings.Contains(strings.ToLower(e.ProjectKey), strings.ToLower(*project)) {
				continue
			}
			if !cutoff.IsZero() && info[src.Name+"/"+e.SessionID].Last.Before(cutoff) {
				continue
			}
			picks = append(picks, pick{e.SessionID, src})
		}
	}
	if len(picks) == 0 {
		fmt.Fprintln(a.out, "Nothing to "+name+".")
		return nil
	}
	sort.SliceStable(picks, func(i, j int) bool { return picks[i].id < picks[j].id })
	// The same session named twice (two prefixes of one id) is done once.
	uniq := picks[:0]
	for i, p := range picks {
		if i == 0 || p.id != picks[i-1].id || p.source.Name != picks[i-1].source.Name {
			uniq = append(uniq, p)
		}
	}
	picks = uniq

	var ops []transfer.Op
	for _, p := range picks {
		op, err := transfer.Plan(p.id, p.source, target, mode, opt)
		if err != nil {
			return err
		}
		ops = append(ops, op)
	}
	info, err := sessionInfo(ctx, svc)
	if err != nil {
		return err
	}
	t := a.term(c)
	if !*apply {
		if c.json {
			return a.printJSON(ops)
		}
		t.Plan(name, ops, info, opt.ArchiveDir)
		return nil
	}
	var results []transfer.Result
	var failed int
	for _, op := range ops {
		if op.Action == transfer.Refuse {
			results = append(results, transfer.Result{Op: op})
			continue
		}
		l := a.ledger(ctx, name, op.SessionID, "", op.From, op.To)
		o := opt
		o.Intend = l.intend
		res, err := transfer.Apply(op, o)
		// Whatever was written is recorded, even when a later step failed.
		if rerr := l.finish(res.Imports); rerr != nil && err == nil {
			err = rerr
		}
		if err != nil {
			failed++
			res.Op = op
			res.Op.Reason = err.Error()
			res.Op.Action = transfer.Refuse
		}
		results = append(results, res)
	}
	// The files are written either way; report them before any index error.
	_, rerr := a.refresh(ctx, false)
	if c.json {
		if err := a.printJSON(results); err != nil {
			return err
		}
	} else {
		t.Results(name, results, info)
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d sessions failed", failed, len(ops))
	}
	return rerr
}

// ledger records what a copy, move or fork writes into an account, so the
// index leaves that usage with the account it came from. Each transcript
// range is recorded before its bytes are put in place (transfer.Intend):
// neither a failed write nor an index run in between can leave copied bytes
// counted as the target's own.
type ledger struct {
	ctx                       context.Context
	a                         *app
	at                        time.Time // one time for the whole operation, so show groups its files
	mode, id, newID, from, to string
	done                      map[string]int64 // path -> end of the range recorded for it
}

func (a *app) ledger(ctx context.Context, mode, id, newID, from, to string) *ledger {
	return &ledger{ctx: ctx, a: a, at: time.Now(), mode: mode, id: id, newID: newID, from: from, to: to, done: map[string]int64{}}
}

func (l *ledger) entry(im transfer.Import) store.Transfer {
	return store.Transfer{At: l.at, Mode: l.mode, SessionID: l.id, NewSessionID: l.newID,
		FromAccount: l.from, ToAccount: l.to, Path: im.Path, From: im.From, To: im.To}
}

// intend is the transfer.Intend hook.
func (l *ledger) intend(im transfer.Import) (func(), error) {
	id, err := l.a.st.RecordTransfer(l.ctx, l.entry(im))
	if err != nil {
		return nil, err
	}
	l.done[im.Path] = im.To
	return func() {
		delete(l.done, im.Path)
		if err := l.a.st.DeleteTransfer(l.ctx, id); err != nil {
			fmt.Fprintf(l.a.errw, "ccli: the ledger still lists bytes %d to %d of %s, which were not written: %v\n", im.From, im.To, im.Path, err)
		}
	}, nil
}

// finish records written bytes the hook did not cover: a source that grew
// while it was being copied.
func (l *ledger) finish(imports []transfer.Import) error {
	var rest []store.Transfer
	for _, im := range imports {
		if to, ok := l.done[im.Path]; ok && to > im.From {
			im.From = to
		}
		if im.To > im.From {
			rest = append(rest, l.entry(im))
		}
	}
	return l.a.st.RecordTransfers(l.ctx, rest)
}

// cmdFork copies a session into an account under a new id, so the copy and
// the original can each go on. With --turn the copy ends at that turn.
func cmdFork(ctx context.Context, a *app, args []string) error {
	var c common
	fs := newFlags("fork", a, &c)
	in := fs.String("a", "", "account the fork goes into and continues in (required; may be the session's own)")
	to := fs.String("to", "", "same as -a")
	from := fs.String("from", "", "source account (default: the copy with the latest activity)")
	turn := fs.Int("turn", 0, "keep the conversation through this turn of ccli log, leave out the rest (default: all of it)")
	apply := fs.Bool("apply", false, "carry out the fork (without it nothing is written)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if *in != "" && *to != "" && *in != *to {
		return fmt.Errorf("-a %s and --to %s name different accounts; give one", *in, *to)
	}
	if *in == "" {
		in = to
	}
	if len(pos) != 1 || *in == "" || *turn < 0 {
		return errors.New("usage: ccli fork <session> -a <account> [--turn N] [--from <account>] [--apply]")
	}
	svc, err := a.prepare(ctx, c, nil)
	if err != nil {
		return err
	}
	defer a.close()
	acct, id, err := svc.Resolve(ctx, pos[0])
	if err != nil {
		return err
	}
	if *from != "" {
		acct = *from
	}
	src, err := a.claudeAccount(acct)
	if err != nil {
		return err
	}
	dst, err := a.claudeAccount(*in)
	if err != nil {
		return err
	}
	cut, last, err := forkCut(ctx, svc, src.Name, id, *turn)
	if err != nil {
		return err
	}
	newID, err := transfer.NewID()
	if err != nil {
		return err
	}
	p, err := transfer.PlanFork(id, src, dst, newID, cut)
	if err != nil {
		return err
	}
	info, err := sessionInfo(ctx, svc, src.Name)
	if err != nil {
		return err
	}
	t := a.term(c)
	if !*apply {
		if c.json {
			return a.printJSON(p)
		}
		t.ForkPlan(p, info[src.Name+"/"+id], last)
		return nil
	}
	l := a.ledger(ctx, "fork", id, newID, src.Name, dst.Name)
	res, ferr := transfer.Fork(p, l.intend)
	if err := l.finish(res.Imports); err != nil && ferr == nil {
		ferr = err
	}
	_, rerr := a.refresh(ctx, false)
	if c.json {
		if err := a.printJSON(map[string]any{"plan": p, "result": res}); err != nil {
			return err
		}
	} else if ferr == nil {
		t.Forked(p, res)
	}
	if ferr != nil {
		return ferr
	}
	return rerr
}

// forkCut works out where a fork that keeps the conversation through turn
// ends: at the prompt of the turn after it, without the agents started
// later. Turn 0, and the last turn, keep everything.
func forkCut(ctx context.Context, svc *query.DB, acct, id string, turn int) (transfer.Cut, query.TurnRow, error) {
	if turn == 0 {
		return transfer.Cut{}, query.TurnRow{}, nil
	}
	ref := acct + ":" + id
	tl, err := svc.Turns(ctx, ref)
	if err != nil {
		return transfer.Cut{}, query.TurnRow{}, err
	}
	short := query.ShortID(id)
	if len(tl.Turns) == 0 {
		return transfer.Cut{}, query.TurnRow{}, fmt.Errorf("%s has no turns to fork from", short)
	}
	first, final := tl.Turns[0].Seq, tl.Turns[len(tl.Turns)-1].Seq
	i := slices.IndexFunc(tl.Turns, func(t query.TurnRow) bool { return t.Seq == turn })
	if i < 0 {
		return transfer.Cut{}, query.TurnRow{}, fmt.Errorf("%s has no turn %d; its turns run %d to %d (ccli log %s --all lists them)", short, turn, first, final, short)
	}
	last := tl.Turns[i]
	cut := transfer.Cut{Turn: turn, Turns: final, Upto: last.Next}
	if last.Next == 0 {
		return cut, last, nil // the last turn: nothing is left out
	}
	det, err := svc.Session(ctx, ref)
	if err != nil {
		return transfer.Cut{}, query.TurnRow{}, err
	}
	// An agent whose spawning turn the index could not place (turn 0) goes
	// by when it started.
	next := tl.Turns[i+1].At
	for _, f := range det.Files {
		if f.Kind == "main" {
			continue
		}
		if f.Turn > turn || f.Turn == 0 && !f.Started.IsZero() && !f.Started.Before(next) {
			cut.Leave = append(cut.Leave, f.Path)
		}
	}
	return cut, last, nil
}

// syncFor makes sure account `in` holds the newest copy of a session before
// it is resumed there. A copy that is behind (a prefix of a newer one) is
// brought up to date when apply is set; diverged copies are never merged.
func (a *app) syncFor(ctx context.Context, id, in string, apply bool) error {
	target, err := a.claudeAccount(in)
	if err != nil {
		return err
	}
	type cp struct {
		acct account.Account
		path string
	}
	var mine *cp
	var others []cp
	for _, ac := range a.accts {
		if ac.Kind() != account.Claude {
			continue
		}
		f, err := transfer.Locate(ac, id)
		if err != nil {
			if transfer.IsNotFound(err) {
				continue
			}
			// Skipping it could pick a stale copy, or write a second
			// transcript next to one that could not be read.
			return fmt.Errorf("%s: %w", ac.Name, err)
		}
		if f.Transcript == "" {
			continue
		}
		if ac.Name == target.Name {
			mine = &cp{ac, f.Transcript}
		} else {
			others = append(others, cp{ac, f.Transcript})
		}
	}
	short := query.ShortID(id)
	// Copies ahead of this account's own (it is a prefix of them) make it
	// behind. A copy that diverged from it is a separate branch: it does
	// not stop this account resuming its own.
	var ahead []*cp
	for i := range others {
		o := &others[i]
		if mine == nil {
			ahead = append(ahead, o)
			continue
		}
		st, err := transfer.Relation(o.path, mine.path)
		if err != nil {
			return err
		}
		switch st {
		case transfer.AAhead:
			ahead = append(ahead, o)
		case transfer.Diverged:
			fmt.Fprintf(a.errw, "ccli: the copy of %s in %s went on separately from %s's; resuming %s's (ccli fork %s --from %s -a %s continues that one as a new session)\n",
				short, o.acct.Name, target.Name, target.Name, short, o.acct.Name, target.Name)
		}
	}
	if len(ahead) == 0 {
		if mine == nil {
			return fmt.Errorf("%s has no transcript in any account", short)
		}
		return nil
	}
	// The newest is the copy every other newer copy is a prefix of.
	newest := ahead[0]
	for _, o := range ahead[1:] {
		st, err := transfer.Relation(o.path, newest.path)
		if err != nil {
			return err
		}
		switch st {
		case transfer.AAhead:
			newest = o
		case transfer.Diverged:
			return fmt.Errorf("the copies of %s in %s and %s are both newer than %s's and have diverged; continue one with ccli fork %s --from <account> -a %s",
				short, newest.acct.Name, o.acct.Name, target.Name, short, target.Name)
		}
	}
	what := "has no copy of"
	if mine != nil {
		what = "has an older copy of"
	}
	if !apply {
		return fmt.Errorf("%s %s %s than %s. Leave out --no-sync to bring it up to date first, or resume it in %s", target.Name, what, short, newest.acct.Name, newest.acct.Name)
	}
	opt := transfer.Options{Live: func(sid string) bool {
		for _, l := range account.LiveSessions(a.accts) {
			if l.SessionID == sid {
				return true
			}
		}
		return false
	}}
	op, err := transfer.Plan(id, newest.acct, target, transfer.Copy, opt)
	if err != nil {
		return err
	}
	if op.Action == transfer.Refuse {
		// Say where it runs: the copy can be brought over once Claude Code
		// has exited there.
		for _, l := range account.LiveSessions(a.accts) {
			if l.SessionID == id {
				return fmt.Errorf("%s is running in %s; exit Claude Code there, then run this again to bring %s's copy up to date", short, l.Account, target.Name)
			}
		}
		return fmt.Errorf("cannot bring %s up to date in %s: %s", short, target.Name, op.Reason)
	}
	l := a.ledger(ctx, "copy", id, "", newest.acct.Name, target.Name)
	opt.Intend = l.intend
	res, err := transfer.Apply(op, opt)
	if rerr := l.finish(res.Imports); rerr != nil && err == nil {
		err = rerr
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(a.errw, "ccli: brought %s up to date in %s from %s (%s)\n", short, target.Name, newest.acct.Name, op.Action)
	_, err = a.refresh(ctx, false)
	return err
}
