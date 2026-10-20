// Package index keeps the store in step with the transcripts on disk. It reads
// only bytes added since the last run and never writes into a config dir.
package index

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/lr00rl/ccli/internal/account"
	"github.com/lr00rl/ccli/internal/codex"
	"github.com/lr00rl/ccli/internal/cursorcli"
	"github.com/lr00rl/ccli/internal/cursoride"
	"github.com/lr00rl/ccli/internal/pi"
	"github.com/lr00rl/ccli/internal/pricing"
	"github.com/lr00rl/ccli/internal/store"
	"github.com/lr00rl/ccli/internal/transcript"
)

// ParserVersion changes when parsing changes; files parsed by an older version
// are read again from the start.
const ParserVersion = 3

const headBytes = 4096

// Options tune a run.
type Options struct {
	Rebuild  bool                               // re-read every file from the start
	Workers  int                                // parse goroutines (default: CPUs)
	Progress func(done, total int, bytes int64) // called from the writer goroutine
	// Trace reports each file. read is false when the file was left unchanged.
	Trace func(path string, read, reset bool)
	// QuotaDir is where rate-limit readings found in logs (Codex writes them)
	// are saved as quota snapshots; empty saves none.
	QuotaDir string
}

// Stats describe a run.
type Stats struct {
	Files      int           `json:"files"`
	Parsed     int           `json:"parsed"`
	Reset      int           `json:"reset"`
	Missing    int           `json:"missing"`
	Bytes      int64         `json:"bytes"`
	Reconciled int           `json:"reconciled"`
	Errors     int           `json:"errors"`                // files that could not be read; retried next run
	FirstError string        `json:"first_error,omitempty"` // the first such error
	Duration   time.Duration `json:"duration"`
}

type job struct {
	file                            store.File
	reset                           bool
	from                            int64
	state                           transcript.State
	meta                            string // path of the subagent's .meta.json, if any
	tool                            account.Tool
	cwd, title, model               string
	started, ended                  int64
	agentType, toolUse, parentAgent string
	imported                        []transcript.Range // byte ranges ccli copied in from another account
	ledger                          int64              // the newest ledger entry when imported was read
}

// Run brings the store up to date for the given accounts.
func Run(ctx context.Context, st *store.Store, accts []account.Account, prices *pricing.Table, opt Options) (Stats, error) {
	start := time.Now()
	var stats Stats
	known, err := st.Files()
	if err != nil {
		return stats, err
	}
	knownRuns, err := st.Runs()
	if err != nil {
		return stats, err
	}
	imported, ledger, err := st.Imported()
	if err != nil {
		return stats, err
	}
	var jobs []job
	var runs []store.Run
	seen := map[string]bool{}
	failed := map[string]bool{} // accounts whose scan failed keep their files as they were
	for _, a := range accts {
		var found []discovered
		var rs []store.Run
		var moves []moved
		switch a.Kind() {
		case account.Codex:
			found, moves, err = scanCodex(a, known)
		case account.Pi:
			found, err = scanPi(a)
		case account.Cursor:
			found, err = scanCursor(a)
		case account.CursorIDE:
			found, err = scanCursorIDE(a)
		default:
			found, rs, err = scan(a, knownRuns, opt.Rebuild)
		}
		if err != nil {
			// One unreadable home must not stop the others.
			failed[a.Name] = true
			stats.Errors++
			if stats.FirstError == "" {
				stats.FirstError = a.Name + ": " + err.Error()
			}
			continue
		}
		for _, m := range moves {
			if err := st.MovePath(ctx, m.ID, m.To); err != nil {
				return stats, err
			}
			f := known[m.From]
			delete(known, m.From)
			f.Path = m.To
			known[m.To] = f
		}
		runs = append(runs, rs...)
		for _, d := range found {
			seen[d.file.Path] = true
			stats.Files++
			if j, ok := plan(d, known[d.file.Path], opt.Rebuild); ok {
				if opt.Trace != nil {
					opt.Trace(d.file.Path, true, j.reset)
				}
				j.imported, j.ledger = imported[d.file.Path], ledger
				if j.file.Kind == "main" && !j.reset && j.file.ID != 0 {
					if j.state.Prompts, err = st.PromptIDs(j.file.ID); err != nil {
						return stats, err
					}
				}
				jobs = append(jobs, j)
			} else if opt.Trace != nil {
				opt.Trace(d.file.Path, false, false)
			}
		}
	}
	// Files that vanished keep their history; mark them missing.
	accSet := map[string]bool{}
	for _, a := range accts {
		accSet[a.Name] = !failed[a.Name]
	}
	var gone []int64
	for p, f := range known {
		if !seen[p] && !f.Missing && accSet[f.Account] {
			gone = append(gone, f.ID)
		}
	}
	stats.Missing = len(gone)
	if err := st.MarkMissing(ctx, gone, true); err != nil {
		return stats, err
	}
	if err := st.PutRuns(ctx, runs); err != nil {
		return stats, err
	}
	for _, j := range jobs {
		if j.reset {
			stats.Reset++
		}
	}
	// Each applied update marks its session in the same transaction, so if
	// this stops here the next run reconciles what this one changed.
	limits := map[string]*transcript.Limits{}
	if err := parseAll(ctx, st, jobs, prices, opt, &stats, limits); err != nil {
		return stats, err
	}
	if opt.QuotaDir != "" {
		saveLimits(opt.QuotaDir, accts, limits)
	}
	if h := prices.Hash(); h != mustMeta(st, "prices") {
		if err := st.Reprice(ctx, prices.Cost, h); err != nil {
			return stats, err
		}
	}
	n, err := st.Reconcile(ctx, pricing.Normalize, prices.Weight)
	if err != nil {
		return stats, err
	}
	stats.Reconciled = n
	stats.Duration = time.Since(start)
	return stats, nil
}

func mustMeta(st *store.Store, k string) string {
	v, _ := st.Meta(k)
	return v
}

type discovered struct {
	file               store.File
	meta               string
	tool               account.Tool
	cwd, title, model  string
	started, ended     int64
	agentType, toolUse string
	parentAgent        string
	side               int64 // Cursor CLI: newest change to the chat's meta.json or store.db, Unix ms
}

// scan lists an account's transcript files, and reads the workflow journals
// and run state files that changed since they were last read.
func scan(a account.Account, known map[[3]string]store.Run, rebuild bool) ([]discovered, []store.Run, error) {
	root := filepath.Join(a.Dir, "projects")
	projects, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	var out []discovered
	runs := map[[3]string]*store.Run{}
	run := func(session, id string) (*store.Run, store.Run) {
		k := [3]string{a.Name, session, id}
		r := runs[k]
		if r == nil {
			r = &store.Run{Account: a.Name, SessionID: session, RunID: id}
			runs[k] = r
		}
		return r, known[k]
	}
	changed := func(path string, last int64) (int64, bool) {
		st, err := os.Stat(path)
		if err != nil || !st.Mode().IsRegular() {
			return 0, false
		}
		mt := st.ModTime().UnixNano()
		return mt, rebuild || mt != last
	}
	add := func(path, session, key, kind, agent, run, meta string) {
		st, err := os.Stat(path)
		if err != nil || !st.Mode().IsRegular() {
			return
		}
		out = append(out, discovered{file: store.File{
			Path: path, Account: a.Name, SessionID: session, ProjectKey: key, Kind: kind,
			AgentID: agent, RunID: run, Size: st.Size(), MTime: st.ModTime().UnixMilli(), Born: birth(path, st),
		}, meta: meta})
	}
	for _, p := range projects {
		if !p.IsDir() {
			continue
		}
		key := p.Name()
		dir := filepath.Join(root, key)
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if !e.IsDir() && strings.HasSuffix(name, ".jsonl") {
				add(filepath.Join(dir, name), strings.TrimSuffix(name, ".jsonl"), key, "main", "", "", "")
				continue
			}
			if !e.IsDir() {
				continue
			}
			session := name
			// Run state files sit beside the subagents folder, under whichever
			// project folder the session was in when the run started.
			if states, err := os.ReadDir(filepath.Join(dir, session, "workflows")); err == nil {
				for _, s := range states {
					id, ok := strings.CutSuffix(s.Name(), ".json")
					if s.IsDir() || !ok || !strings.HasPrefix(id, "wf_") {
						continue
					}
					r, old := run(session, id)
					path := filepath.Join(dir, session, "workflows", s.Name())
					if mt, ok := changed(path, old.StateMTime); ok {
						readState(path, r)
						r.StateMTime = mt
					}
				}
			}
			sub := filepath.Join(dir, session, "subagents")
			agents, err := os.ReadDir(sub)
			if err != nil {
				continue
			}
			for _, ae := range agents {
				an := ae.Name()
				if !ae.IsDir() && strings.HasPrefix(an, "agent-") && strings.HasSuffix(an, ".jsonl") {
					id := strings.TrimSuffix(strings.TrimPrefix(an, "agent-"), ".jsonl")
					add(filepath.Join(sub, an), session, key, "subagent", id, "", filepath.Join(sub, "agent-"+id+".meta.json"))
				}
			}
			wfRoot := filepath.Join(sub, "workflows")
			wfs, err := os.ReadDir(wfRoot)
			if err != nil {
				continue
			}
			for _, w := range wfs {
				if !w.IsDir() {
					continue
				}
				runID := w.Name()
				rdir := filepath.Join(wfRoot, runID)
				files, err := os.ReadDir(rdir)
				if err != nil {
					continue
				}
				for _, fe := range files {
					fn := fe.Name()
					switch {
					case fn == "journal.jsonl":
						r, old := run(session, runID)
						path := filepath.Join(rdir, fn)
						if mt, ok := changed(path, old.JournalMTime); ok {
							journal(path, r)
							r.JournalMTime = mt
						}
					case strings.HasPrefix(fn, "agent-") && strings.HasSuffix(fn, ".jsonl"):
						id := strings.TrimSuffix(strings.TrimPrefix(fn, "agent-"), ".jsonl")
						add(filepath.Join(rdir, fn), session, key, "workflow", id, runID, filepath.Join(rdir, "agent-"+id+".meta.json"))
					}
				}
			}
		}
	}
	list := make([]store.Run, 0, len(runs))
	for _, r := range runs {
		if r.JournalMTime != 0 || r.StateMTime != 0 {
			list = append(list, *r)
		}
	}
	return out, list, nil
}

// readState reads what a workflow run's state file says about it.
func readState(path string, r *store.Run) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var v struct {
		Name       string `json:"workflowName"`
		Status     string `json:"status"`
		Summary    string `json:"summary"`
		StartTime  int64  `json:"startTime"`
		DurationMS int64  `json:"durationMs"`
		AgentCount int    `json:"agentCount"`
	}
	if json.Unmarshal(b, &v) != nil {
		return
	}
	r.Name, r.Status, r.Summary = v.Name, v.Status, clean(v.Summary, 300)
	r.StartMS, r.DurationMS, r.AgentCount = v.StartTime, v.DurationMS, v.AgentCount
}

func clean(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		s = string(r[:n])
	}
	return s
}

// journal counts a workflow run's events.
func journal(path string, r *store.Run) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	r.Launched, r.Started, r.Results, r.Failed = 0, 0, 0, 0
	for _, ln := range strings.Split(string(b), "\n") {
		var v struct {
			Type string `json:"type"`
		}
		if json.Unmarshal([]byte(ln), &v) != nil {
			continue
		}
		switch v.Type {
		case "launched":
			r.Launched++
		case "started":
			r.Started++
		case "result":
			r.Results++
		case "failed":
			r.Failed++
		}
	}
}

// plan decides what to read from a file.
func plan(d discovered, old store.File, rebuild bool) (job, bool) {
	f := d.file
	j := job{file: f, meta: d.meta, tool: d.tool, cwd: d.cwd, title: d.title, model: d.model, started: d.started, ended: d.ended,
		agentType: d.agentType, toolUse: d.toolUse, parentAgent: d.parentAgent}
	if d.tool == account.CursorIDE {
		return planIDE(j, old, rebuild)
	}
	if old.ID == 0 {
		return j, true
	}
	j.file.ID = old.ID
	// Pi and Cursor CLI rewrite or branch in place. A changed file is read
	// again from the start so an abandoned branch is not left in the index
	// as if it were still the conversation.
	if rebuild || old.Parser != fileParser(d.tool) || f.Size < old.Offset || refiled(d.tool, f, old) ||
		(fullReread(d.tool) && (f.Size != old.Offset || f.MTime != old.MTime)) {
		j.reset = true
		return j, true
	}
	if f.Size == old.Offset {
		if old.Missing {
			j.from, j.state = old.Offset, continueFrom(old)
			return j, true // reappeared unchanged: clear the missing flag
		}
		return j, false
	}
	if old.Offset > 0 {
		h, err := headHash(f.Path, old.Offset)
		if err != nil || h != old.HeadHash {
			j.reset = true
			return j, true
		}
	}
	j.from, j.state = old.Offset, continueFrom(old)
	return j, true
}

func fullReread(t account.Tool) bool {
	return t == account.Pi || t == account.Cursor
}

// fileParser is the parser stamp stored on a file. Cursor's clock and model
// are read by a newer parser than Claude's; bumping only these keeps a
// Claude transcript from being read again.
func fileParser(t account.Tool) int {
	switch t {
	case account.Cursor, account.CursorIDE:
		return 5
	default:
		return ParserVersion
	}
}

// planIDE keys a composer on its lastUpdatedAt, stored as the file's size
// and mtime, and on its bubble count and newest bubble row, stored as the
// head hash: Cursor writes a reply's bubbles minutes after it last moves
// lastUpdatedAt. The path is not a real file, so there is nothing to hash.
func planIDE(j job, old store.File, rebuild bool) (job, bool) {
	if old.ID == 0 {
		return j, true
	}
	j.file.ID = old.ID
	if rebuild || old.Parser != fileParser(j.tool) || old.Missing || refiled(j.tool, j.file, old) ||
		j.file.Size != old.Size || j.file.MTime != old.MTime || j.file.HeadHash != old.HeadHash {
		j.reset = true
		return j, true
	}
	return j, false
}

// refiled reports a Cursor transcript that its side data now files under
// another session or kind, as when store.db or a composer header shows late
// that a chat is a subagent. The file is read again so all its rows move.
func refiled(t account.Tool, f, old store.File) bool {
	return (t == account.Cursor || t == account.CursorIDE) && (f.SessionID != old.SessionID || f.Kind != old.Kind)
}

// continueFrom is the parse state an incremental parse of a file starts with.
func continueFrom(old store.File) transcript.State {
	return transcript.State{PrevTS: old.PrevTS, Turn: old.Turn, Replay: old.Replay, Carry: old.Carry}
}

// headHash hashes the first headBytes of a file, or fewer when only that many
// had been parsed.
func headHash(path string, upto int64) (string, error) {
	n := int64(headBytes)
	if upto < n {
		n = upto
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.CopyN(h, f, n); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}

type parsed struct {
	up    store.Update
	bytes int64
	err   error
}

// parseAll parses the jobs and applies the results; limits receives the
// latest rate-limit reading found per account.
func parseAll(ctx context.Context, st *store.Store, jobs []job, prices *pricing.Table, opt Options, stats *Stats, limits map[string]*transcript.Limits) error {
	if len(jobs) == 0 {
		return nil
	}
	workers := opt.Workers
	if workers <= 0 {
		workers = runtime.NumCPU()
	}
	in := make(chan job)
	out := make(chan parsed, workers*2)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range in {
				out <- parseOne(j)
			}
		}()
	}
	go func() {
		defer close(in)
		for _, j := range jobs {
			select {
			case in <- j:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		wg.Wait()
		close(out)
	}()

	var batch []store.Update
	var batchBytes int64
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		err := st.Apply(ctx, batch, prices.Cost)
		batch, batchBytes = batch[:0], 0
		return err
	}
	done := 0
	var firstErr error // only store errors and cancellation stop a run
	for p := range out {
		done++
		if p.err != nil {
			// An unreadable file keeps its old offset and is tried again.
			if !errors.Is(p.err, os.ErrNotExist) {
				stats.Errors++
				if stats.FirstError == "" {
					stats.FirstError = p.err.Error()
				}
			}
			continue
		}
		stats.Parsed++
		stats.Bytes += p.bytes
		if l := p.up.Result.Limits; l != nil {
			if cur := limits[p.up.File.Account]; cur == nil || l.At > cur.At {
				limits[p.up.File.Account] = l
			}
		}
		batch = append(batch, p.up)
		batchBytes += p.bytes
		if len(batch) >= 64 || batchBytes >= 64<<20 {
			if err := flush(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		if opt.Progress != nil {
			opt.Progress(done, len(jobs), stats.Bytes)
		}
	}
	if err := flush(); err != nil && firstErr == nil {
		firstErr = err
	}
	if firstErr == nil {
		firstErr = ctx.Err()
	}
	return firstErr
}

func parseOne(j job) parsed {
	if j.tool == account.CursorIDE {
		return parseIDE(j)
	}
	f, err := os.Open(j.file.Path)
	if err != nil {
		return parsed{err: err}
	}
	defer f.Close()
	from := j.from
	if j.reset {
		from = 0
		j.state = transcript.State{}
	}
	if _, err := f.Seek(from, io.SeekStart); err != nil {
		return parsed{err: err}
	}
	kind := transcript.Main
	switch j.file.Kind {
	case "subagent":
		kind = transcript.Subagent
	case "workflow":
		kind = transcript.Workflow
	}
	j.state.Base, j.state.Imported = from, j.imported
	var res transcript.Result
	var agent *store.AgentMeta
	switch j.tool {
	case account.Codex:
		thread := j.file.SessionID
		if kind == transcript.Subagent {
			thread = j.file.AgentID
		}
		var meta *codex.Meta
		if res, meta, err = codex.Parse(f, kind, thread, j.state); err == nil && meta != nil && kind == transcript.Subagent {
			agent = codexAgent(meta, j.file.SessionID)
		}
	case account.Pi:
		thread := j.file.SessionID
		if kind == transcript.Subagent && j.file.AgentID != "" {
			thread = j.file.AgentID
		}
		res, err = pi.Parse(f, thread, j.state)
	case account.Cursor:
		side := cursorcli.Side{Cwd: j.cwd, Title: j.title, Created: j.started, Updated: j.ended}
		if j.file.Kind == "main" {
			side.Model = j.model
		}
		res, err = cursorcli.Parse(f, j.file.SessionID, side)
		if err == nil && j.file.Kind == "subagent" {
			agent = &store.AgentMeta{AgentType: j.agentType, ToolUseID: j.toolUse, ParentAgent: j.parentAgent}
			if agent.AgentType == "" {
				agent.AgentType = "subagent"
			}
			if res.FirstPrompt != "" {
				agent.Description = clean(res.FirstPrompt, 200)
			}
		}
	default:
		res, err = transcript.Parse(f, kind, j.state)
	}
	if err != nil {
		return parsed{err: err}
	}
	if j.tool == account.Cursor {
		if res.Cwd == "" {
			res.Cwd = j.cwd
		}
		if res.CustomTitle == "" {
			res.CustomTitle = j.title
		}
		if res.FirstTS == 0 {
			res.FirstTS = j.started
		}
		if j.ended > res.LastTS {
			res.LastTS = j.ended
		}
		if res.FirstTS == 0 {
			res.FirstTS = j.file.MTime
		}
		if res.LastTS == 0 {
			res.LastTS = j.file.MTime
		}
	}
	up := store.Update{File: j.file, Reset: j.reset, From: from, Result: res, Ledger: j.ledger, Agent: agent}
	up.File.Offset = from + res.Consumed
	up.File.Parser = fileParser(j.tool)
	if up.File.Offset > 0 {
		if h, err := headHash(j.file.Path, up.File.Offset); err == nil {
			up.File.HeadHash = h
		}
	}
	if j.meta != "" {
		up.Agent = readMeta(j.meta)
	}
	return parsed{up: up, bytes: res.Consumed}
}

func parseIDE(j job) parsed {
	from := j.from
	if j.reset {
		from = 0
	}
	res, agent, err := cursoride.Parse(j.file.Path)
	if err != nil {
		return parsed{err: err}
	}
	if res.Cwd == "" {
		res.Cwd = j.cwd
	}
	up := store.Update{File: j.file, Reset: j.reset, From: from, Result: res, Ledger: j.ledger}
	up.File.Offset = j.file.Size
	up.File.Parser = fileParser(j.tool)
	if agent != nil {
		up.Agent = &store.AgentMeta{AgentType: agent.Type, Description: agent.Description, ToolUseID: agent.ToolUseID}
	}
	return parsed{up: up, bytes: res.Consumed}
}

func readMeta(path string) *store.AgentMeta {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var v struct {
		AgentType    string `json:"agentType"`
		Description  string `json:"description"`
		ToolUseID    string `json:"toolUseId"`
		SpawnDepth   int    `json:"spawnDepth"`
		RequestShape string `json:"requestShape"`
		Name         string `json:"name"`
		Parent       string `json:"parentAgentId"`
		Phase        string `json:"workflowPhase"`
	}
	if json.Unmarshal(b, &v) != nil {
		return nil
	}
	return &store.AgentMeta{AgentType: v.AgentType, Description: v.Description, ToolUseID: v.ToolUseID, SpawnDepth: v.SpawnDepth,
		RequestShape: v.RequestShape, Skill: v.Name, ParentAgent: v.Parent, Phase: v.Phase}
}
