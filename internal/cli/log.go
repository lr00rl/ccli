package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/term"

	"github.com/lr00rl/ccli/internal/query"
	"github.com/lr00rl/ccli/internal/render"
)

func init() {
	commands["log"] = cmdLog
	commands["turns"] = cmdLog
	commands["prompts"] = cmdPrompts
}

// cmdLog lists a session's turns, or prints turns, an agent or a workflow
// run in full.
func cmdLog(ctx context.Context, a *app, args []string) error {
	var c common
	var opt render.LogOptions
	fs := newFlags("log", a, &c)
	all := fs.Bool("all", false, "include turns started by tasks, other sessions and the system")
	sortBy := fs.String("sort", "turn", "list order: turn or cost")
	fs.BoolVar(&opt.Tools, "tools", false, "show tool inputs and outputs")
	fs.BoolVar(&opt.Thinking, "thinking", false, "show the model's thinking")
	fs.BoolVar(&opt.Context, "context", false, "show text Claude Code added (skills, reminders, peer messages)")
	fs.BoolVar(&opt.Raw, "raw", false, "print prompts and replies as recorded, without markdown rendering")
	agent := fs.String("agent", "", "print a subagent's transcript (agent id or prefix)")
	run := fs.String("run", "", "list a workflow run's agents (run id or prefix)")
	noPager := fs.Bool("no-pager", false, "do not page long output")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 1 || len(pos) > 2 {
		return errors.New("usage: ccli log <session> [turn | from-to | last] [--tools] [--thinking] [--agent id] [--run id]")
	}
	if a.verbose {
		opt.Tools = true
	}
	svc, err := a.prepare(ctx, c, nil)
	if err != nil {
		return err
	}
	defer a.close()
	if acct, _, err := svc.Resolve(ctx, pos[0]); err == nil {
		a.refreshCursorFor(ctx, []string{acct}, nil)
	}
	ref := pos[0]
	t := a.term(c)
	switch {
	case *agent != "":
		log, err := svc.Agent(ctx, ref, *agent)
		if err != nil && !(errors.Is(err, query.ErrNoTranscript) && log != nil) {
			return err
		}
		if c.json {
			return a.printJSON(log)
		}
		return a.page(t, *noPager, func(t render.Term) {
			t.AgentLog(log, opt)
			if err != nil {
				fmt.Fprintln(t.W, err)
			}
		})
	case *run != "":
		info, nodes, status, err := svc.RunAgents(ctx, ref, *run)
		if err != nil {
			return err
		}
		if c.json {
			return a.printJSON(map[string]any{"run": info, "agents": nodes, "status": status})
		}
		return a.page(t, *noPager, func(t render.Term) { t.RunAgents(info, nodes, status, ref) })
	case len(pos) == 1:
		l, err := svc.Turns(ctx, ref)
		if err != nil {
			return err
		}
		if *sortBy == "cost" {
			sort.SliceStable(l.Turns, func(i, j int) bool { return l.Turns[i].Total() > l.Turns[j].Total() })
		}
		if c.json {
			return a.printJSON(l)
		}
		return a.page(t, *noPager, func(t render.Term) { t.Turns(l, *all) })
	}
	from, to, err := turnRange(pos[1])
	if err != nil {
		return err
	}
	details, err := svc.TurnRange(ctx, ref, from, to, *all)
	if err != nil && !(errors.Is(err, query.ErrNoTranscript) && len(details) > 0) {
		return err
	}
	if err != nil {
		fmt.Fprintln(a.errw, "ccli:", err)
	}
	if c.json {
		if len(details) == 1 {
			return a.printJSON(details[0])
		}
		return a.printJSON(details)
	}
	return a.page(t, *noPager, func(t render.Term) {
		for i, d := range details {
			if i > 0 {
				fmt.Fprintln(t.W)
				fmt.Fprintln(t.W, strings.Repeat("─", min(t.Width, 100)))
				fmt.Fprintln(t.W)
			}
			t.Turn(d, opt)
		}
	})
}

// turnRange parses "12", "12-15" or "last" (-1).
func turnRange(s string) (int, int, error) {
	if s == "last" {
		return -1, -1, nil
	}
	if a, b, ok := strings.Cut(s, "-"); ok {
		from, err1 := strconv.Atoi(a)
		to, err2 := strconv.Atoi(b)
		if err1 != nil || err2 != nil || from < 0 || to < from {
			return 0, 0, fmt.Errorf("turn range %q: use N, N-M or last", s)
		}
		return from, to, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, 0, fmt.Errorf("turn %q: use N, N-M or last", s)
	}
	return n, n, nil
}

func cmdPrompts(ctx context.Context, a *app, args []string) error {
	var c common
	var f query.Filter
	var accts accountsFlag
	fs := newFlags("prompts", a, &c)
	days, since := sessionFlags(fs, &f, &accts)
	fs.StringVar(&f.Sort, "sort", "last", "sort by last, cost, tokens, tools, duration or length")
	fs.Lookup("q").Usage = "prompt text contains"
	fs.IntVar(&f.Limit, "n", 40, "show at most N prompts (0 = all)")
	hiddenFlag(fs, &f)
	noPager := fs.Bool("no-pager", false, "do not page long output")
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
	p, err := svc.Prompts(ctx, f)
	if err != nil {
		return err
	}
	if c.json {
		return a.printJSON(p)
	}
	return a.page(a.term(c), *noPager, func(t render.Term) { t.Prompts(p) })
}

// page renders into a buffer and, when stdout is a terminal and the output
// is taller than it, shows it through $PAGER (default less -FRX).
func (a *app) page(t render.Term, off bool, draw func(render.Term)) error {
	var buf bytes.Buffer
	out := t.W
	t.W = &buf
	draw(t)
	f, ok := out.(*os.File)
	if off || !ok || !term.IsTerminal(int(f.Fd())) {
		_, err := io.Copy(out, &buf)
		return err
	}
	_, rows, err := term.GetSize(int(f.Fd()))
	if err != nil || bytes.Count(buf.Bytes(), []byte("\n")) < rows-1 {
		_, err := io.Copy(out, &buf)
		return err
	}
	pager := os.Getenv("PAGER")
	if pager == "" {
		pager = "less -FRX"
	}
	cmd := exec.Command("sh", "-c", pager)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = bytes.NewReader(buf.Bytes()), f, a.errw
	cmd.Env = append(os.Environ(), "LESSCHARSET=utf-8")
	if err := cmd.Run(); err != nil {
		// sh exits 126 or 127 when the pager cannot run at all; any other
		// exit is the pager quitting, after it showed the output.
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() != 126 && ee.ExitCode() != 127 {
			return nil
		}
		_, err := io.Copy(out, &buf)
		return err
	}
	return nil
}
