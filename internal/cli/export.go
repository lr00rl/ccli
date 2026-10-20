package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/lr00rl/ccli/internal/export"
	"github.com/lr00rl/ccli/internal/query"
)

func init() {
	commands["export"] = cmdExport
}

func cmdExport(ctx context.Context, a *app, args []string) error {
	var c common
	fs := newFlags("export", a, &c)
	format := fs.String("format", "md", "md or jsonl")
	detail := fs.String("detail", "full", "full writes each step; reply writes your prompt and the turn's last reply")
	outPath := fs.String("out", "", "write the transcript to this file (mode 0600) instead of stdout")
	agents := fs.Bool("agents", false, "include each subagent's conversation after the turn that started it")
	thinking := fs.Bool("thinking", false, "include the model's thinking")
	withContext := fs.Bool("context", false, "include text the tool added (skills, reminders, peer messages)")
	all := fs.Bool("all", false, "include turns started by tasks, other sessions and the system")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 1 || len(pos) > 2 {
		return errors.New("usage: ccli export <session> [turn | from-to | last] [--out file] [--format md|jsonl]")
	}
	svc, err := a.prepare(ctx, c, nil)
	if err != nil {
		return err
	}
	defer a.close()
	acct, _, err := svc.Resolve(ctx, pos[0])
	if err != nil {
		return err
	}
	a.refreshCursorFor(ctx, []string{acct}, nil)
	ref := pos[0]
	list, err := svc.Turns(ctx, ref)
	if err != nil {
		return err
	}
	if len(list.Turns) == 0 {
		return fmt.Errorf("session %s has no turns", query.ShortID(list.Session.ID))
	}
	from, to := list.Turns[0].Seq, list.Turns[len(list.Turns)-1].Seq
	if len(pos) == 2 {
		from, to, err = turnRange(pos[1])
		if err != nil {
			return err
		}
	}
	details, err := svc.TurnRange(ctx, ref, from, to, *all)
	if err != nil && !(errors.Is(err, query.ErrNoTranscript) && len(details) > 0) {
		return err
	}
	gone := err
	doc := export.Doc{Session: list.Session, Turns: details}
	if *agents {
		doc.Agents, err = agentLogs(ctx, svc, ref, details)
		if err != nil {
			return err
		}
	}
	opt := export.Options{Format: *format, Detail: *detail, Thinking: *thinking, Context: *withContext, Agents: *agents, Loc: time.Local}
	if *outPath == "" || *outPath == "-" {
		err = export.Write(a.out, doc, opt)
	} else {
		err = a.writeFile(*outPath, func(w io.Writer) error { return export.Write(w, doc, opt) })
	}
	if err != nil {
		return err
	}
	if *outPath != "" && *outPath != "-" {
		fmt.Fprintf(a.errw, "wrote %s\n", *outPath)
	}
	if gone != nil {
		fmt.Fprintln(a.errw, "ccli:", gone)
	}
	return nil
}

// writeFile writes through a temporary file in the target's folder, so a
// failed write leaves nothing behind and an existing file is replaced only
// by a complete one. The file is private, because an export holds prompts and
// tool output. A path inside an agent's home is refused: ccli never writes
// there, and a jsonl export placed in one would be read back as a session.
func (a *app) writeFile(path string, write func(io.Writer) error) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	real := resolveExisting(abs)
	for _, ac := range a.accts {
		if home := resolveExisting(ac.Dir); query.Under(real, home) {
			return fmt.Errorf("%s is inside the %s home %s, and ccli does not write there", path, ac.Name, home)
		}
	}
	f, err := os.CreateTemp(filepath.Dir(abs), ".ccli-export-*")
	if err != nil {
		return err
	}
	err = write(f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), abs)
	}
	if err != nil {
		os.Remove(f.Name())
	}
	return err
}

func agentLogs(ctx context.Context, svc *query.DB, ref string, details []*query.TurnDetail) (map[string]*query.AgentLog, error) {
	out := map[string]*query.AgentLog{}
	var walk func([]query.AgentNode) error
	walk = func(ns []query.AgentNode) error {
		for _, n := range ns {
			if n.AgentID != "" && out[n.AgentID] == nil {
				log, err := svc.Agent(ctx, ref, n.AgentID)
				if err != nil && log == nil {
					return err
				}
				if log != nil {
					out[log.Agent.AgentID] = log
				}
			}
			if err := walk(n.Children); err != nil {
				return err
			}
		}
		return nil
	}
	for _, d := range details {
		if d == nil {
			continue
		}
		if err := walk(d.Agents); err != nil {
			return nil, err
		}
	}
	return out, nil
}
