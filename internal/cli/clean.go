package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lr00rl/ccli/internal/account"
	"github.com/lr00rl/ccli/internal/query"
	"github.com/lr00rl/ccli/internal/render"
	"github.com/lr00rl/ccli/internal/store"
	"github.com/lr00rl/ccli/internal/transfer"
)

func init() {
	commands["clean"] = cmdClean
}

// stampLayout names one clean run's archive folder; transfer.Archive uses
// the same layout for the folder it creates.
const stampLayout = "20060102-150405"

func cleanDir() string { return filepath.Join(store.DataDir(), "archive", "clean") }

// cleanItem is one session of a plan, with what apply checks again.
type cleanItem struct {
	render.CleanRow
	acct  account.Account
	files transfer.Files
	size  int64
	mtime time.Time
}

// cleanManifest records an applied run in its archive folder, so --undo
// knows what it hid.
type cleanManifest struct {
	At       time.Time         `json:"at"`
	Sessions []render.CleanRow `json:"sessions"`
}

// cmdClean finds empty sessions and, once confirmed, sets them aside:
// Claude Code transcripts move into ccli's archive and leave the index, and
// sessions of other tools, whose homes ccli does not write, are hidden.
func cmdClean(ctx context.Context, a *app, args []string) error {
	var c common
	var f query.Filter
	var accts accountsFlag
	fs := newFlags("clean", a, &c)
	fs.Var(&accts, "a", "account (repeat or comma separate; default all)")
	dirFlag(fs, &f)
	toolFlag(fs, &f)
	unanswered := fs.Bool("unanswered", false, "also sessions whose prompts never got a reply")
	idle := fs.Duration("idle", time.Hour, "leave sessions active more recently than this")
	apply := fs.Bool("apply", false, "do it without asking")
	undo := fs.String("undo", "", "put back what the clean run with this stamp archived and hid")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return errors.New("usage: ccli clean [-a account] [--dir folder] [--tool name] [--unanswered] [--idle 1h] [--apply], or ccli clean --undo <stamp>")
	}
	if *idle < 0 {
		return errors.New("--idle: use a duration such as 1h or 30m")
	}
	if c.noIndex {
		return errors.New("--no-index: clean decides from what the transcripts hold now, so it always updates the index first")
	}
	f.Accounts = accts
	svc, err := a.prepare(ctx, c, accts)
	if err != nil {
		return err
	}
	defer a.close()
	if *undo != "" {
		return a.cleanUndo(ctx, c, *undo)
	}
	// Billed Cursor usage counts as content, as it does in every list.
	a.refreshCursorFor(ctx, f.Accounts, f.Tools)
	now := time.Now()
	rows, err := svc.Empty(ctx, f, *unanswered, *idle, now)
	if err != nil {
		return err
	}
	plan := a.cleanPlan(rows, *idle, now)
	t := a.term(c)
	if !*apply {
		if c.json {
			return a.printJSON(cleanOut{Sessions: cleanRows(plan)})
		}
		t.CleanPlan(cleanRows(plan))
		if len(plan) == 0 {
			return nil
		}
		if !stdinTTY() || !isTTY(a.out) {
			fmt.Fprintln(a.errw, "Nothing was changed. Run with --apply to do this.")
			return nil
		}
		if !ask(a.errw, cleanSummary(plan)+"? [y/N] ") {
			fmt.Fprintln(a.errw, "Nothing was changed.")
			return nil
		}
	}
	stamp, err := a.cleanApply(ctx, plan, now)
	if c.json {
		if jerr := a.printJSON(cleanOut{Sessions: cleanRows(plan), Undo: stamp}); err == nil {
			err = jerr
		}
		return err
	}
	t.CleanDone(cleanRows(plan), stamp)
	return err
}

// cleanOut is what clean prints with --json: the plan, or after --apply
// what was done and the stamp --undo takes.
type cleanOut struct {
	Sessions []render.CleanRow `json:"sessions"`
	Undo     string            `json:"undo,omitempty"`
}

// cleanPlan decides what to do with each empty session. A Claude Code
// session whose transcript changed within idle, or that has session folders
// but no transcript, is left out.
func (a *app) cleanPlan(rows []query.SessionRow, idle time.Duration, now time.Time) []cleanItem {
	var out []cleanItem
	for _, s := range rows {
		acct, ok := a.accountNamed(s.Account)
		if !ok {
			continue
		}
		it := cleanItem{acct: acct, CleanRow: render.CleanRow{
			Account: s.Account, ID: s.ID, Title: s.Title, Cwd: s.Cwd, Last: s.Last, Prompts: s.Prompts + s.OtherPrompts,
		}}
		if it.Cwd == "" {
			it.Cwd = s.ProjectKey // a transcript with no line that names its folder
		}
		if acct.Kind() != account.Claude {
			it.Action = "hide"
			out = append(out, it)
			continue
		}
		files, err := transfer.Locate(acct, s.ID)
		switch {
		case transfer.IsNotFound(err):
			it.Action = "forget" // the transcript is already gone; only the index remembers it
		case err != nil || files.Transcript == "":
			continue
		default:
			st, err := os.Stat(files.Transcript)
			if err != nil || now.Sub(st.ModTime()) < idle {
				continue
			}
			it.Action, it.files, it.size, it.mtime = "archive", files, st.Size(), st.ModTime()
			if it.Last.IsZero() {
				it.Last = st.ModTime() // no line carries a time; the file does
			}
		}
		out = append(out, it)
	}
	return out
}

// cleanApply carries out a plan, checking each session again first: it must
// not be running, and a transcript must be where and as the plan saw it. A
// session that fails a check is left as it was, with a note. The run's
// folder is claimed before anything moves, and its record is written before
// the first change and after each one, so a run that stops part way can be
// undone. It returns the stamp --undo takes, or "" when nothing changed.
func (a *app) cleanApply(ctx context.Context, plan []cleanItem, now time.Time) (string, error) {
	stamp := now.Format(stampLayout)
	dir := filepath.Join(cleanDir(), stamp)
	if err := os.MkdirAll(cleanDir(), 0o700); err != nil {
		return "", err
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		if errors.Is(err, os.ErrExist) {
			return "", errors.New("another clean run started this second; run it again")
		}
		return "", err
	}
	if err := writeManifest(dir, plan, now); err != nil {
		return "", err
	}
	live := map[string]bool{}
	for _, l := range account.LiveSessions(a.accts) {
		live[l.SessionID] = true
	}
	var err error
	for i := range plan {
		it := &plan[i]
		switch {
		case err != nil:
			it.Note = "not done: stopped at an earlier error"
		case ctx.Err() != nil:
			it.Note = "not done: interrupted"
		case live[it.ID]:
			it.Note = "running now; left as it was"
		default:
			if err = a.cleanOne(ctx, it, now); err != nil {
				it.Note = err.Error()
			}
			it.Done = err == nil && it.Note == ""
		}
		if werr := writeManifest(dir, plan, now); werr != nil && err == nil {
			err = werr
		}
	}
	if err == nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	// Nothing to put back: no file moved into the folder and nothing hidden.
	if ents, rerr := os.ReadDir(dir); rerr == nil && len(ents) == 1 && !hidAny(plan) {
		os.Remove(filepath.Join(dir, "manifest.json"))
		os.Remove(dir)
		return "", err
	}
	return stamp, err
}

func hidAny(plan []cleanItem) bool {
	for _, it := range plan {
		if it.Done && it.Action == "hide" {
			return true
		}
	}
	return false
}

func (a *app) cleanOne(ctx context.Context, it *cleanItem, now time.Time) error {
	switch it.Action {
	case "archive":
		files, err := transfer.Locate(it.acct, it.ID)
		if err != nil || files.Transcript != it.files.Transcript {
			it.Note = "moved since the plan; left as it was"
			return nil
		}
		st, err := os.Stat(files.Transcript)
		if err != nil || st.Size() != it.size || !st.ModTime().Equal(it.mtime) {
			it.Note = "changed since the plan; left as it was"
			return nil
		}
		if _, _, err := transfer.Archive(files, cleanDir(), now); err != nil {
			return err
		}
	case "forget":
		if _, err := transfer.Locate(it.acct, it.ID); !transfer.IsNotFound(err) {
			it.Note = "its transcript is back; left as it was"
			return nil
		}
	case "hide":
		return a.st.Hide(ctx, map[string]time.Time{it.ID: now})
	}
	_, err := a.st.Forget(ctx, it.Account, it.ID)
	return err
}

// writeManifest records the run beside its archive: every session of the
// plan, and which are done.
func writeManifest(dir string, plan []cleanItem, now time.Time) error {
	b, err := json.MarshalIndent(cleanManifest{At: now, Sessions: cleanRows(plan)}, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "manifest.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "manifest.json"))
}

// cleanUndo puts a run's archived transcripts back into their accounts and
// brings back the sessions it hid. It works from the run's folder, so a run
// that stopped before its record was complete can still be undone. A path
// an account has again, and anything that is not a session, stay in the
// archive. Sessions are unhidden and the index is updated even when one
// account fails.
func (a *app) cleanUndo(ctx context.Context, c common, stamp string) error {
	if _, err := time.Parse(stampLayout, stamp); err != nil {
		return fmt.Errorf("--undo %q: give the stamp clean printed, such as 20261020-153000", stamp)
	}
	dir := filepath.Join(cleanDir(), stamp)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("no clean run %s in %s", stamp, cleanDir())
	}
	var m cleanManifest
	if b, err := os.ReadFile(filepath.Join(dir, "manifest.json")); err == nil {
		if err := json.Unmarshal(b, &m); err != nil {
			return fmt.Errorf("read the record of clean run %s: %w", stamp, err)
		}
	}
	var restored, kept []string
	var errs []error
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		root := filepath.Join(dir, e.Name())
		acct, ok := a.accountNamed(e.Name())
		if !ok || acct.Kind() != account.Claude {
			kept = append(kept, root)
			fmt.Fprintf(a.errw, "ccli: no Claude Code account is named %s now; its sessions stay in %s\n", e.Name(), root)
			continue
		}
		r, conflicts, left, err := transfer.Restore(root, acct)
		restored = append(restored, r...)
		for _, p := range conflicts {
			fmt.Fprintf(a.errw, "ccli: %s is in the account again; the archived copy stays in %s\n", p, root)
		}
		for _, p := range left {
			fmt.Fprintf(a.errw, "ccli: %s is not part of a session and stays where it is\n", p)
		}
		kept = append(kept, conflicts...)
		kept = append(kept, left...)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", e.Name(), err))
		}
	}
	var hidden []string
	for _, r := range m.Sessions {
		if r.Action == "hide" && r.Done {
			hidden = append(hidden, r.ID)
		}
	}
	if err := a.st.Unhide(ctx, hidden); err != nil {
		errs = append(errs, err)
	}
	if len(kept) == 0 && len(errs) == 0 {
		os.Remove(filepath.Join(dir, "manifest.json"))
		os.Remove(dir)
	}
	if _, err := a.refresh(ctx, false); err != nil {
		errs = append(errs, err)
	}
	transcripts := 0
	for _, p := range restored {
		if strings.HasSuffix(p, ".jsonl") {
			transcripts++
		}
	}
	if c.json {
		if err := a.printJSON(map[string]any{"restored": restored, "unhidden": hidden, "left_in_archive": kept}); err != nil {
			errs = append(errs, err)
		}
	} else {
		fmt.Fprintf(a.out, "Put back %s and brought back %s.\n", render.Plural(transcripts, "transcript"), render.Plural(len(hidden), "hidden session"))
	}
	return errors.Join(errs...)
}

func cleanRows(plan []cleanItem) []render.CleanRow {
	out := make([]render.CleanRow, len(plan))
	for i, it := range plan {
		out[i] = it.CleanRow
	}
	return out
}

func cleanSummary(plan []cleanItem) string {
	n := map[string]int{}
	for _, it := range plan {
		n[it.Action]++
	}
	var parts []string
	for _, k := range []string{"archive", "hide", "forget"} {
		if n[k] > 0 {
			parts = append(parts, k+" "+render.Plural(n[k], "session"))
		}
	}
	s := strings.Join(parts, ", ")
	return strings.ToUpper(s[:1]) + s[1:]
}

func (a *app) accountNamed(name string) (account.Account, bool) {
	for _, ac := range a.accts {
		if ac.Name == name {
			return ac, true
		}
	}
	return account.Account{}, false
}

func stdinTTY() bool {
	st, err := os.Stdin.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// ask prints a yes-or-no question and reads the answer from stdin.
func ask(w io.Writer, question string) bool {
	fmt.Fprint(w, question)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	s := strings.ToLower(strings.TrimSpace(line))
	return s == "y" || s == "yes"
}
