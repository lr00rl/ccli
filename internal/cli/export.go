package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
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
	outPath := fs.String("out", "", "write the transcript to this file instead of stdout")
	agents := fs.Bool("agents", false, "include each subagent's conversation after the turn that started it")
	thinking := fs.Bool("thinking", false, "include the model's thinking")
	context := fs.Bool("context", false, "include text the tool added (skills, reminders, peer messages)")
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
	opt := export.Options{Format: *format, Thinking: *thinking, Context: *context, Agents: *agents, Loc: time.Local}
	var w io.Writer = a.out
	var closeFile func() error
	if *outPath != "" {
		f, err := os.Create(*outPath)
		if err != nil {
			return err
		}
		w = f
		closeFile = f.Close
	}
	err = export.Write(w, doc, opt)
	if closeFile != nil {
		if cerr := closeFile(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		return err
	}
	if *outPath != "" {
		fmt.Fprintf(a.errw, "wrote %s\n", *outPath)
	}
	if gone != nil {
		fmt.Fprintln(a.errw, "ccli:", gone)
	}
	return nil
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
