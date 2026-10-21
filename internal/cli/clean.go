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
	f.Accounts = accts
	svc, err := a.prepare(ctx, c, accts)
	if err != nil {
		return err
	}
	defer a.close()
	if *undo != "" {
		return a.cleanUndo(ctx, c, *undo)
	}
	now := time.Now()
	rows, err := svc.Empty(ctx, f, *unanswered, *idle, now)
	if err != nil {
		return err
	}
	plan := a.cleanPlan(rows, *idle, now)
	t := a.term(c)
	if !*apply {
		if c.json {
			return a.printJSON(cleanRows(plan, false))
		}
		t.CleanPlan(cleanRows(plan, false))
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
		if jerr := a.printJSON(map[string]any{"sessions": cleanRows(plan, true), "undo": stamp}); err == nil {
			err = jerr
		}
		return err
	}
	t.CleanDone(cleanRows(plan, true), stamp)
	return err
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
			if s.Hidden {
				continue
			}
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
// session that fails a check is left as it was, with a note. It returns the
// stamp --undo takes, or "" when nothing can be undone.
func (a *app) cleanApply(ctx context.Context, plan []cleanItem, now time.Time) (string, error) {
	// Each run gets a folder of its own, named for the second it started.
	if _, err := os.Stat(filepath.Join(cleanDir(), now.Format(stampLayout))); err == nil {
		return "", errors.New("another clean run started this second; run it again")
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
		case live[it.ID]:
			it.Note = "running now; left as it was"
		default:
			if err = a.cleanOne(ctx, it, now); err != nil {
				it.Note = err.Error()
			}
		}
	}
	stamp, merr := a.writeManifest(plan, now)
	if err == nil {
		err = merr
	}
	return stamp, err
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

// writeManifest records what was done beside the archive. Forgotten
// sessions have nothing to put back, so a run that only forgot gets no stamp.
func (a *app) writeManifest(plan []cleanItem, now time.Time) (string, error) {
	done := cleanRows(plan, true)
	undoable := false
	for _, r := range done {
		if r.Note == "" && r.Action != "forget" {
			undoable = true
		}
	}
	if !undoable {
		return "", nil
	}
	stamp := now.Format(stampLayout)
	dir := filepath.Join(cleanDir(), stamp)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	b, err := json.MarshalIndent(cleanManifest{At: now, Sessions: done}, "", "  ")
	if err != nil {
		return "", err
	}
	return stamp, os.WriteFile(filepath.Join(dir, "manifest.json"), b, 0o600)
}

// cleanUndo puts a run's archived transcripts back into their accounts and
// brings back the sessions it hid. A transcript an account has again since
// stays in the archive.
func (a *app) cleanUndo(ctx context.Context, c common, stamp string) error {
	if _, err := time.Parse(stampLayout, stamp); err != nil {
		return fmt.Errorf("--undo %q: give the stamp clean printed, such as 20261020-153000", stamp)
	}
	dir := filepath.Join(cleanDir(), stamp)
	b, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return fmt.Errorf("no clean run %s in %s", stamp, cleanDir())
	}
	var m cleanManifest
	if err := json.Unmarshal(b, &m); err != nil {
		return fmt.Errorf("read the record of clean run %s: %w", stamp, err)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var restored, conflicts []string
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		root := filepath.Join(dir, e.Name())
		acct, ok := a.accountNamed(e.Name())
		if !ok || acct.Kind() != account.Claude {
			conflicts = append(conflicts, root)
			fmt.Fprintf(a.errw, "ccli: no Claude Code account is named %s now; its sessions stay in %s\n", e.Name(), root)
			continue
		}
		r, cf, err := transfer.Restore(root, acct)
		restored, conflicts = append(restored, r...), append(conflicts, cf...)
		if err != nil {
			return err
		}
	}
	var hidden []string
	for _, r := range m.Sessions {
		if r.Action == "hide" && r.Note == "" {
			hidden = append(hidden, r.ID)
		}
	}
	if err := a.st.Unhide(ctx, hidden); err != nil {
		return err
	}
	if len(conflicts) == 0 {
		os.Remove(filepath.Join(dir, "manifest.json"))
		os.Remove(dir)
	}
	if _, err := a.refresh(ctx, false); err != nil {
		return err
	}
	transcripts := 0
	for _, p := range restored {
		if strings.HasSuffix(p, ".jsonl") {
			transcripts++
		}
	}
	if c.json {
		return a.printJSON(map[string]any{"restored": restored, "unhidden": hidden, "left_in_archive": conflicts})
	}
	fmt.Fprintf(a.out, "Put back %s and brought back %s.\n", render.Plural(transcripts, "transcript"), render.Plural(len(hidden), "hidden session"))
	for _, p := range conflicts {
		fmt.Fprintf(a.out, "  left in the archive, the account has it again: %s\n", p)
	}
	return nil
}

// cleanRows is the plan as rows; done keeps the notes apply left.
func cleanRows(plan []cleanItem, done bool) []render.CleanRow {
	out := make([]render.CleanRow, len(plan))
	for i, it := range plan {
		out[i] = it.CleanRow
		if !done {
			out[i].Note = ""
		}
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
