package index

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lr00rl/ccli/internal/account"
	"github.com/lr00rl/ccli/internal/pricing"
	"github.com/lr00rl/ccli/internal/store"
	"github.com/lr00rl/ccli/internal/transcript"
)

func at(min float64) string {
	return time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC).Add(time.Duration(min * float64(time.Minute))).Format(time.RFC3339Nano)
}

func asst(min float64, id string, out int64) string {
	return fmt.Sprintf(`{"type":"assistant","timestamp":%q,"cwd":"/work/proj","message":{"id":%q,"model":"claude-opus-5-5","usage":{"input_tokens":1000000,"output_tokens":%d}}}`+"\n", at(min), id, out)
}

func prompt(min float64, text string) string {
	return fmt.Sprintf(`{"type":"user","timestamp":%q,"cwd":"/work/proj","message":{"content":%q}}`+"\n", at(min), text)
}

type env struct {
	t      *testing.T
	accts  []account.Account
	st     *store.Store
	prices *pricing.Table
}

func newEnv(t *testing.T, names ...string) *env {
	t.Helper()
	root := t.TempDir()
	var accts []account.Account
	for _, n := range names {
		dir := filepath.Join(root, ".claude")
		if n != "default" {
			dir += "-" + n
		}
		os.MkdirAll(filepath.Join(dir, "projects"), 0o755)
		accts = append(accts, account.Account{Name: n, Dir: dir})
	}
	st, err := store.Open(filepath.Join(root, "ccli.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	p, _ := pricing.Load("")
	return &env{t: t, accts: accts, st: st, prices: p}
}

func (e *env) path(acct int, rel string) string {
	return filepath.Join(e.accts[acct].Dir, "projects", rel)
}

func (e *env) write(acct int, rel, content string) {
	p := e.path(acct, rel)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) appendTo(acct int, rel, content string) {
	f, err := os.OpenFile(e.path(acct, rel), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		e.t.Fatal(err)
	}
	f.WriteString(content)
	f.Close()
}

func (e *env) run() Stats {
	e.t.Helper()
	s, err := Run(context.Background(), e.st, e.accts, e.prices, Options{Workers: 2})
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

func (e *env) int(q string, args ...any) int64 {
	e.t.Helper()
	var n int64
	if err := e.st.DB().QueryRow(q, args...).Scan(&n); err != nil {
		e.t.Fatalf("%s: %v", q, err)
	}
	return n
}

func TestFirstRunAndHierarchy(t *testing.T) {
	e := newEnv(t, "default")
	e.write(0, "-work-proj/s1.jsonl", prompt(0, "hello")+asst(1, "m1", 10)+asst(1, "m1", 10)+asst(2, "m2", 20))
	e.write(0, "-work-proj/s1/subagents/agent-a1.jsonl", asst(1.5, "sub1", 5))
	e.write(0, "-work-proj/s1/subagents/agent-a1.meta.json", `{"agentType":"code-reviewer","description":"review","toolUseId":"tu_9","spawnDepth":1,"requestShape":"background"}`)
	e.write(0, "-work-proj/s1/subagents/workflows/run1/agent-w1.jsonl", asst(3, "wf1", 7))
	e.write(0, "-work-proj/s1/subagents/workflows/run1/journal.jsonl", `{"type":"launched"}`+"\n"+`{"type":"started"}`+"\n"+`{"type":"result"}`+"\n")
	e.write(0, "-work-proj/s1/tool-results/big.txt", "ignored")
	s := e.run()
	if s.Files != 3 || s.Parsed != 3 {
		t.Fatalf("stats %+v", s)
	}
	if n := e.int("SELECT COUNT(*) FROM responses WHERE session_id='s1'"); n != 4 {
		t.Fatalf("responses = %d, want 4", n)
	}
	if n := e.int("SELECT SUM(output) FROM responses"); n != 42 {
		t.Fatalf("output = %d", n)
	}
	if n := e.int("SELECT COUNT(*) FROM files WHERE kind='subagent' AND agent_type='code-reviewer' AND tool_use_id='tu_9' AND spawn_depth=1"); n != 1 {
		t.Fatal("subagent meta not stored")
	}
	if n := e.int("SELECT COUNT(*) FROM files WHERE kind='workflow' AND run_id='run1' AND agent_id='w1'"); n != 1 {
		t.Fatal("workflow file not stored")
	}
	if n := e.int("SELECT launched+started+results FROM runs WHERE run_id='run1'"); n != 3 {
		t.Fatalf("journal = %d", n)
	}
	if n := e.int("SELECT typed_prompts FROM files WHERE kind='main'"); n != 1 {
		t.Fatalf("prompts = %d", n)
	}
	// A second run with nothing new reads nothing.
	if s := e.run(); s.Parsed != 0 || s.Bytes != 0 {
		t.Fatalf("idle run parsed %+v", s)
	}
}

func TestAppendReadsOnlyNewBytes(t *testing.T) {
	e := newEnv(t, "default")
	e.write(0, "-p/s1.jsonl", prompt(0, "a")+asst(2, "m1", 1))
	e.run()
	more := asst(4, "m2", 1) + asst(4.5, "m2", 1)
	e.appendTo(0, "-p/s1.jsonl", more)
	s := e.run()
	if s.Parsed != 1 || s.Bytes != int64(len(more)) || s.Reset != 0 {
		t.Fatalf("append stats %+v (want %d bytes)", s, len(more))
	}
	if n := e.int("SELECT COUNT(*) FROM responses"); n != 2 {
		t.Fatalf("responses = %d", n)
	}
	// Active time continues across runs: 2 min + 2 min + 0.5 min.
	if n := e.int("SELECT active_ms FROM files"); n != 270_000 {
		t.Fatalf("active = %d", n)
	}
}

func TestPartialLineWaits(t *testing.T) {
	e := newEnv(t, "default")
	line := asst(1, "m1", 1)
	e.write(0, "-p/s1.jsonl", line[:30])
	e.run()
	if n := e.int("SELECT COUNT(*) FROM responses"); n != 0 {
		t.Fatal("partial line parsed")
	}
	e.write(0, "-p/s1.jsonl", line)
	e.run()
	if n := e.int("SELECT COUNT(*) FROM responses"); n != 1 {
		t.Fatal("completed line not parsed")
	}
}

func TestRewriteResets(t *testing.T) {
	e := newEnv(t, "default")
	e.write(0, "-p/s1.jsonl", asst(1, "m1", 5)+asst(2, "m2", 5))
	e.run()
	// Same length or longer, different head: a rewrite.
	e.write(0, "-p/s1.jsonl", asst(1, "x1", 6)+asst(2, "x2", 6)+asst(3, "x3", 6))
	s := e.run()
	if s.Reset != 1 {
		t.Fatalf("stats %+v", s)
	}
	if n := e.int("SELECT SUM(output) FROM responses"); n != 18 {
		t.Fatalf("output after rewrite = %d", n)
	}
	// Shrinking also resets.
	e.write(0, "-p/s1.jsonl", asst(1, "y1", 1))
	e.run()
	if n := e.int("SELECT COUNT(*) FROM responses"); n != 1 {
		t.Fatalf("responses after shrink = %d", n)
	}
}

func TestDeletedTranscriptKeepsHistory(t *testing.T) {
	e := newEnv(t, "default")
	e.write(0, "-p/s1.jsonl", asst(1, "m1", 9))
	e.run()
	os.Remove(e.path(0, "-p/s1.jsonl"))
	if s := e.run(); s.Missing != 1 {
		t.Fatalf("stats %+v", s)
	}
	if n := e.int("SELECT COUNT(*) FROM responses"); n != 1 {
		t.Fatal("history dropped")
	}
	if n := e.int("SELECT missing FROM files"); n != 1 {
		t.Fatal("not marked missing")
	}
	e.write(0, "-p/s1.jsonl", asst(1, "m1", 9))
	e.run()
	if n := e.int("SELECT missing FROM files"); n != 0 {
		t.Fatal("reappeared file still missing")
	}
}

func TestCopiedSessionCountedOnce(t *testing.T) {
	e := newEnv(t, "default", "work")
	content := asst(1, "m1", 10) + asst(2, "m2", 10)
	e.write(0, "-p/s1.jsonl", content)
	e.run()
	e.write(1, "-p/s1.jsonl", content+asst(3, "m3", 10))
	e.run()
	if n := e.int("SELECT COUNT(*) FROM responses"); n != 3 {
		t.Fatalf("responses = %d", n)
	}
	if n := e.int("SELECT COUNT(*) FROM responses WHERE account='work'"); n != 1 {
		t.Fatalf("work owns %d, want only the new one", n)
	}
}

func TestRepriceOnTableChange(t *testing.T) {
	e := newEnv(t, "default")
	e.write(0, "-p/s1.jsonl", asst(1, "m1", 0))
	e.run()
	if n := e.int("SELECT CAST(cost * 100 AS INTEGER) FROM responses"); n != 400 {
		t.Fatalf("cost cents = %d", n)
	}
	over := filepath.Join(t.TempDir(), "prices.json")
	os.WriteFile(over, []byte(`{"claude-opus-5-5":{"input":1,"output":1}}`), 0o644)
	e.prices, _ = pricing.Load(over)
	e.run()
	if n := e.int("SELECT CAST(cost * 100 AS INTEGER) FROM responses"); n != 100 {
		t.Fatalf("repriced cents = %d", n)
	}
}

func costState(start int64, cost float64, out, read int64) string {
	return fmt.Sprintf(`{"type":"cost-state","startTime":%d,"totalCostUSD":%v,"modelUsage":{"claude-opus-5-5":{"inputTokens":2000000,"outputTokens":%d,"cacheReadInputTokens":%d,"cacheCreationInputTokens":0,"costUSD":%v}}}`+"\n", start, cost, out, read, cost)
}

// withMissing is the cost in cents of the two recorded calls ($8) plus the
// tokens Claude Code's costState fixture shows beyond them, at list price.
func withMissing(e *env) int64 {
	w, _ := e.prices.Weight("claude-opus-5-5", transcript.Usage{Output: 50_000, CacheRead: 1_000_000})
	return int64(math.Round((8 + w) * 100))
}

func TestReconcileWithCostState(t *testing.T) {
	e := newEnv(t, "default")
	start := time.Date(2026, 10, 1, 8, 59, 0, 0, time.UTC).UnixMilli()
	// Two recorded calls: 2M input at $4/M = $8. Claude Code's own total for
	// the process says $10 with extra output and cache reads.
	e.write(0, "-p/s1.jsonl", asst(1, "m1", 0)+asst(2, "m2", 0)+costState(start, 10, 50_000, 1_000_000))
	e.run()
	if n := e.int("SELECT COUNT(*) FROM responses WHERE kind='adjustment'"); n != 1 {
		t.Fatalf("adjustments = %d", n)
	}
	if n := e.int("SELECT CAST(ROUND(SUM(cost) * 100) AS INTEGER) FROM responses WHERE session_id='s1'"); n != withMissing(e) {
		t.Fatalf("session cost cents = %d, want %d", n, withMissing(e))
	}
	if n := e.int("SELECT output FROM responses WHERE kind='adjustment'"); n != 50_000 {
		t.Fatalf("adjustment output = %d", n)
	}
	if n := e.int("SELECT input FROM responses WHERE kind='adjustment'"); n != 0 {
		t.Fatalf("adjustment input = %d (recorded input already matches)", n)
	}
	// Re-running does not stack adjustments.
	e.appendTo(0, "-p/s1.jsonl", prompt(10, "more"))
	e.run()
	if n := e.int("SELECT CAST(ROUND(SUM(cost) * 100) AS INTEGER) FROM responses WHERE session_id='s1'"); n != withMissing(e) {
		t.Fatalf("after rerun cents = %d", n)
	}
	// A process whose own total is below the recorded calls adds nothing.
	e.write(0, "-p/s2.jsonl", asst(1, "n1", 0)+asst(2, "n2", 0)+costState(start, 1, 0, 0))
	e.run()
	if n := e.int("SELECT COUNT(*) FROM responses WHERE kind='adjustment' AND session_id='s2'"); n != 0 {
		t.Fatalf("negative shortfall stored")
	}
}

// A copy repeats its original's cost-state line, while its responses belong to
// the original. The process's shortfall must be added once, whichever file is
// indexed first.
func TestCopiedCostStateCountedOnce(t *testing.T) {
	start := time.Date(2026, 10, 1, 8, 59, 0, 0, time.UTC).UnixMilli()
	content := asst(1, "m1", 0) + asst(2, "m2", 0) + costState(start, 10, 50_000, 1_000_000)
	for _, order := range []string{"original first", "copy first", "same run"} {
		t.Run(order, func(t *testing.T) {
			e := newEnv(t, "default", "work")
			e.write(0, "-p/s1.jsonl", content)
			time.Sleep(20 * time.Millisecond) // distinct creation times
			e.write(1, "-p/s1.jsonl", content)
			all := e.accts
			switch order {
			case "original first":
				e.accts = all[:1]
				e.run()
			case "copy first":
				e.accts = all[1:]
				e.run()
			}
			e.accts = all
			e.run()
			e.run() // and stays put on a rerun
			if n := e.int("SELECT CAST(ROUND(SUM(cost) * 100) AS INTEGER) FROM responses"); n != withMissing(e) {
				t.Fatalf("total cents = %d, want %d", n, withMissing(e))
			}
			if n := e.int("SELECT COUNT(*) FROM responses WHERE kind='adjustment' AND account='default'"); n != 1 {
				t.Fatalf("the original's session holds %d adjustments, want 1", n)
			}
		})
	}
}

// A file that cannot be read must not stop the rest of the run, including the
// reconciliation of the files that were read.
func TestUnreadableFileDoesNotStopReconcile(t *testing.T) {
	e := newEnv(t, "default")
	start := time.Date(2026, 10, 1, 8, 59, 0, 0, time.UTC).UnixMilli()
	e.write(0, "-p/s1.jsonl", asst(1, "m1", 0)+asst(2, "m2", 0)+costState(start, 10, 50_000, 1_000_000))
	e.write(0, "-p/s2.jsonl", asst(1, "n1", 0))
	if err := os.Chmod(e.path(0, "-p/s2.jsonl"), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(e.path(0, "-p/s2.jsonl"), 0o644) })
	s := e.run()
	if s.Errors != 1 || s.FirstError == "" {
		t.Fatalf("errors = %d %q, want 1", s.Errors, s.FirstError)
	}
	if n := e.int("SELECT COUNT(*) FROM responses WHERE kind='adjustment'"); n != 1 {
		t.Fatalf("adjustments = %d, want 1", n)
	}
	// Once readable, the file is picked up.
	os.Chmod(e.path(0, "-p/s2.jsonl"), 0o644)
	if s := e.run(); s.Errors != 0 {
		t.Fatalf("errors after fix = %d", s.Errors)
	}
	if n := e.int("SELECT COUNT(*) FROM responses WHERE session_id='s2'"); n != 1 {
		t.Fatalf("s2 responses = %d", n)
	}
}

// Claude Code's figure for a model ccli has no price for is not added as
// cost, matching how unpriced calls are left out everywhere else.
func TestUnpricedModelGetsNoAdjustmentCost(t *testing.T) {
	e := newEnv(t, "default")
	start := time.Date(2026, 10, 1, 8, 59, 0, 0, time.UTC).UnixMilli()
	line := fmt.Sprintf(`{"type":"cost-state","startTime":%d,"totalCostUSD":3,"modelUsage":{"some-new-model":{"inputTokens":0,"outputTokens":500,"cacheReadInputTokens":0,"cacheCreationInputTokens":0,"costUSD":3}}}`+"\n", start)
	e.write(0, "-p/s1.jsonl", asst(1, "m1", 0)+line)
	e.run()
	if n := e.int("SELECT COUNT(*) FROM responses WHERE kind='adjustment' AND output=500 AND cost IS NULL"); n != 1 {
		t.Fatalf("want one unpriced adjustment with the missing tokens")
	}
}

// A fast-forward copy back replaces the original's file with a newer one; the
// original must keep its usage and its process adjustment all the same.
func TestOwnershipSurvivesReplacedFile(t *testing.T) {
	e := newEnv(t, "default", "work")
	start := time.Date(2026, 10, 1, 8, 59, 0, 0, time.UTC).UnixMilli()
	content := asst(1, "m1", 0) + asst(2, "m2", 0) + costState(start, 10, 50_000, 1_000_000)
	e.write(0, "-p/s1.jsonl", content)
	e.run()
	time.Sleep(20 * time.Millisecond)
	e.write(1, "-p/s1.jsonl", content)
	e.run()
	time.Sleep(20 * time.Millisecond)
	tmp := e.path(0, "-p/.s1.tmp")
	if err := os.WriteFile(tmp, []byte(content+prompt(10, "more")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, e.path(0, "-p/s1.jsonl")); err != nil {
		t.Fatal(err)
	}
	e.run()
	if n := e.int("SELECT COUNT(*) FROM responses WHERE account='default' AND kind <> 'adjustment'"); n != 2 {
		t.Fatalf("default owns %d responses, want 2", n)
	}
	if n := e.int("SELECT COUNT(*) FROM responses WHERE kind='adjustment' AND account='default'"); n != 1 {
		t.Fatalf("default holds %d adjustments, want 1", n)
	}
}

// A session started in one account, continued in a second, then brought back
// and continued in the first: each account keeps exactly the work it did.
func TestPingPongBetweenAccountsKeepsOwnership(t *testing.T) {
	e := newEnv(t, "default", "work")
	ctx := context.Background()
	record := func(to int, from, upto int64) {
		t.Helper()
		names := []string{"default", "work"}
		err := e.st.RecordTransfers(ctx, []store.Transfer{{At: time.Now(), Mode: "copy", SessionID: "s1",
			FromAccount: names[1-to], ToAccount: names[to], Path: e.path(to, "-p/s1.jsonl"), From: from, To: upto}})
		if err != nil {
			t.Fatal(err)
		}
	}
	replace := func(acct int, content string) {
		t.Helper()
		tmp := e.path(acct, "-p/.s1.tmp")
		os.MkdirAll(filepath.Dir(tmp), 0o755)
		if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, e.path(acct, "-p/s1.jsonl")); err != nil {
			t.Fatal(err)
		}
	}
	startB := time.Date(2026, 10, 1, 9, 2, 30, 0, time.UTC).UnixMilli()

	a1 := prompt(0, "start") + asst(1, "m1", 10) + asst(2, "m2", 10)
	e.write(0, "-p/s1.jsonl", a1)
	e.run()
	time.Sleep(20 * time.Millisecond)

	// Copy to work, which continues the session.
	replace(1, a1)
	record(1, 0, int64(len(a1)))
	e.run()
	b1 := a1 + prompt(3, "go on in work") + asst(4, "m3", 10) + asst(5, "m4", 10) + costState(startB, 100, 1_000_000, 0)
	replace(1, b1)
	e.run()

	// Bring it back to default (a fast-forward) and continue there.
	replace(0, b1)
	record(0, int64(len(a1)), int64(len(b1)))
	e.run()
	e.appendTo(0, "-p/s1.jsonl", prompt(6, "back home")+asst(7, "m5", 10))
	e.run()

	owners := map[string]string{}
	rows, err := e.st.DB().Query("SELECT key, account FROM responses WHERE kind <> 'adjustment'")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var k, a string
		rows.Scan(&k, &a)
		owners[k] = a
	}
	rows.Close()
	want := map[string]string{"m1": "default", "m2": "default", "m3": "work", "m4": "work", "m5": "default"}
	for k, a := range want {
		if owners[k] != a {
			t.Fatalf("%s owned by %q, want %q (all: %v)", k, owners[k], a, owners)
		}
	}
	if n := e.int("SELECT COUNT(*) FROM responses WHERE kind = 'adjustment' AND account = 'work'"); n != 1 {
		t.Fatalf("work holds %d adjustments for its own process, want 1", n)
	}
	if n := e.int("SELECT COUNT(*) FROM responses WHERE kind = 'adjustment' AND account = 'default'"); n != 0 {
		t.Fatalf("default holds %d adjustments for work's process, want 0", n)
	}
	// Turns are numbered per file: default sees all three prompts.
	if n := e.int("SELECT COUNT(*) FROM turns t JOIN files f ON f.id = t.file_id WHERE f.account = 'default' AND t.kind = 'human'"); n != 3 {
		t.Fatalf("default has %d turns, want 3", n)
	}
}

func TestWorkflowStateAndJournal(t *testing.T) {
	e := newEnv(t, "default")
	e.write(0, "-p/s1.jsonl", prompt(0, "run it")+asst(1, "m1", 10))
	e.write(0, "-p/s1/workflows/wf_00000001-abc.json", `{"runId":"wf_00000001-abc","script":"x","workflowName":"lanes","status":"completed","summary":"Build   the lanes","startTime":1790000000000,"durationMs":61000,"agentCount":3}`)
	e.write(0, "-p/s1/subagents/workflows/wf_00000001-abc/journal.jsonl", `{"type":"launched"}`+"\n"+`{"type":"started","agentId":"a1"}`+"\n"+`{"type":"started","agentId":"a2"}`+"\n"+`{"type":"result","agentId":"a1"}`+"\n")
	e.run()
	var name, status, summary string
	var started, results int
	var dur int64
	if err := e.st.DB().QueryRow("SELECT name, status, summary, duration_ms, started, results FROM runs WHERE run_id = 'wf_00000001-abc'").Scan(&name, &status, &summary, &dur, &started, &results); err != nil {
		t.Fatal(err)
	}
	if name != "lanes" || status != "completed" || summary != "Build the lanes" || dur != 61000 || started != 2 || results != 1 {
		t.Fatalf("run = %q %q %q %d %d %d", name, status, summary, dur, started, results)
	}
	// Unchanged files are not read again, and a second run keeps the values.
	e.run()
	if n := e.int("SELECT started FROM runs WHERE run_id = 'wf_00000001-abc'"); n != 2 {
		t.Fatalf("started = %d after a second run", n)
	}
	e.appendTo(0, "-p/s1/subagents/workflows/wf_00000001-abc/journal.jsonl", `{"type":"failed","agentId":"a2"}`+"\n")
	e.run()
	if n := e.int("SELECT failed FROM runs WHERE run_id = 'wf_00000001-abc'"); n != 1 {
		t.Fatalf("failed = %d after the journal grew", n)
	}
	if s := e.int("SELECT COUNT(*) FROM runs WHERE name = 'lanes' AND status = 'completed'"); s != 1 {
		t.Fatalf("state fields lost when only the journal was read again")
	}
}

func owners(t *testing.T, e *env) map[string]string {
	t.Helper()
	out := map[string]string{}
	rows, err := e.st.DB().Query("SELECT key, account FROM responses WHERE kind <> 'adjustment'")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var k, a string
		rows.Scan(&k, &a)
		out[k] = a
	}
	return out
}

// A fast-forward copied back with its ledger entry recorded only afterwards
// (the copy was indexed in between): the work done in the other account goes
// back to it once the entry exists, although its own file never changed.
func TestLateLedgerHandsResponsesBack(t *testing.T) {
	e := newEnv(t, "default", "work")
	a1 := prompt(0, "start") + asst(1, "m1", 10) + asst(2, "m2", 10)
	b1 := a1 + prompt(3, "go on in work") + asst(4, "m3", 10) + asst(5, "m4", 10)
	e.write(0, "-p/s1.jsonl", a1)
	e.run()
	time.Sleep(20 * time.Millisecond)
	e.write(1, "-p/s1.jsonl", b1)
	e.run()

	// Back to default with no ledger entry yet: default's file is the older
	// one, so it takes work's responses.
	tmp := e.path(0, "-p/.s1.tmp")
	if err := os.WriteFile(tmp, []byte(b1), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, e.path(0, "-p/s1.jsonl")); err != nil {
		t.Fatal(err)
	}
	e.run()
	if o := owners(t, e); o["m3"] != "default" {
		t.Fatalf("setup: m3 owned by %q, want default before the ledger entry", o["m3"])
	}

	if err := e.st.RecordTransfers(context.Background(), []store.Transfer{{At: time.Now(), Mode: "copy", SessionID: "s1",
		FromAccount: "work", ToAccount: "default", Path: e.path(0, "-p/s1.jsonl"), From: int64(len(a1)), To: int64(len(b1))}}); err != nil {
		t.Fatal(err)
	}
	e.run() // default is read again; work is marked to be read again
	e.run() // work takes its responses back
	want := map[string]string{"m1": "default", "m2": "default", "m3": "work", "m4": "work"}
	o := owners(t, e)
	for k, a := range want {
		if o[k] != a {
			t.Fatalf("%s owned by %q, want %q (all: %v)", k, o[k], a, o)
		}
	}
	// Settled: further runs read nothing again.
	if s := e.run(); s.Parsed != 0 {
		t.Fatalf("a settled index parsed %d files, want 0", s.Parsed)
	}
}

// An index run that planned before a transfer was recorded must not mark
// the file as read with the current parser: it may have counted the copied
// bytes as the file's own.
func TestStaleLedgerUpdateForcesReread(t *testing.T) {
	e := newEnv(t, "default")
	e.write(0, "-p/s1.jsonl", prompt(0, "start")+asst(1, "m1", 10))
	e.run()
	path := e.path(0, "-p/s1.jsonl")
	_, mark, err := e.st.Imported()
	if err != nil {
		t.Fatal(err)
	}
	id, err := e.st.RecordTransfer(context.Background(), store.Transfer{At: time.Now(), Mode: "copy", SessionID: "s1",
		FromAccount: "work", ToAccount: "default", Path: path, From: 0, To: 10})
	if err != nil || id == 0 {
		t.Fatalf("record: id %d, %v", id, err)
	}
	var fid, size int64
	if err := e.st.DB().QueryRow("SELECT id, size FROM files WHERE path = ?", path).Scan(&fid, &size); err != nil {
		t.Fatal(err)
	}
	noCost := func(string, string, transcript.Usage) (float64, bool) { return 0, false }
	up := func(ledger int64) int64 {
		t.Helper()
		u := store.Update{File: store.File{ID: fid, Path: path, Account: "default", SessionID: "s1", Kind: "main", Size: size, Offset: size, Parser: ParserVersion},
			Reset: true, Ledger: ledger}
		if err := e.st.Apply(context.Background(), []store.Update{u}, noCost); err != nil {
			t.Fatal(err)
		}
		return e.int("SELECT parser FROM files WHERE id = ?", fid)
	}
	if p := up(mark); p != 0 {
		t.Fatalf("parse that predates the transfer stored parser %d, want 0", p)
	}
	if p := up(id); p != ParserVersion {
		t.Fatalf("parse that knew the transfer stored parser %d, want %d", p, ParserVersion)
	}
	if err := e.st.DeleteTransfer(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if n := e.int("SELECT COUNT(*) FROM transfers"); n != 0 {
		t.Fatalf("%d ledger entries left after delete", n)
	}
}

// A home that cannot be scanned is reported and skipped: the other accounts
// are still indexed, and its own files are not marked missing.
func TestUnreadableHomeDoesNotStopTheRun(t *testing.T) {
	e := newEnv(t, "default")
	e.write(0, "-p/s1.jsonl", prompt(0, "hello")+asst(1, "m1", 10))
	ide := filepath.Join(filepath.Dir(e.accts[0].Dir), "Cursor", "User")
	db := filepath.Join(ide, "globalStorage", "state.vscdb")
	if err := os.MkdirAll(filepath.Dir(db), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(db, []byte("not a database, just bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.accts = append(e.accts, account.Account{Name: "cursor-ide", Tool: account.CursorIDE, Dir: ide})
	if _, err := e.st.DB().Exec(`INSERT INTO files(path, account, session_id, project_key, kind) VALUES(?, 'cursor-ide', 'c1', '', 'main')`, db+"#c1"); err != nil {
		t.Fatal(err)
	}
	s := e.run()
	if s.Errors != 1 || !strings.HasPrefix(s.FirstError, "cursor-ide: ") {
		t.Fatalf("errors = %d %q, want one from cursor-ide", s.Errors, s.FirstError)
	}
	if n := e.int("SELECT COUNT(*) FROM responses WHERE session_id='s1'"); n != 1 {
		t.Fatalf("s1 responses = %d, want 1", n)
	}
	if n := e.int("SELECT missing FROM files WHERE path = ?", db+"#c1"); n != 0 {
		t.Fatal("the unreadable home's composer was marked missing")
	}
}

// An account renamed between runs, as when a new home takes its name, keeps
// its sessions: its files and responses move to the new name.
func TestRenamedAccountKeepsItsSessions(t *testing.T) {
	e := newEnv(t, "default")
	e.write(0, "-p/s1.jsonl", prompt(0, "hello")+asst(1, "m1", 10))
	e.write(0, "-p/s1/subagents/agent-a1.jsonl", asst(1.5, "sub1", 5))
	e.run()
	e.accts[0].Name = "default-claude"
	e.run()
	if n := e.int("SELECT COUNT(*) FROM files WHERE account <> 'default-claude'"); n != 0 {
		t.Fatalf("%d files kept the old account name", n)
	}
	if n := e.int("SELECT COUNT(*) FROM responses WHERE account = 'default-claude'"); n != 2 {
		t.Fatalf("responses under the new name = %d, want 2", n)
	}
	if s := e.run(); s.Parsed != 0 {
		t.Fatalf("a third run read %d files again", s.Parsed)
	}
}

// Claude Code keeps one running total per session and carries it on when
// the session resumes, in another account too once ccli copied it there.
// Each account's shortfall is the growth of that total it produced itself,
// whichever file is newer and whatever the sync did to creation times.
func TestRunningTotalSplitBetweenAccounts(t *testing.T) {
	e := newEnv(t, "default", "work")
	ctx := context.Background()
	start := time.Date(2026, 10, 1, 8, 59, 0, 0, time.UTC).UnixMilli()
	total := func(in, out int64) string {
		return fmt.Sprintf(`{"type":"cost-state","startTime":%d,"modelUsage":{"claude-opus-5-5":{"inputTokens":%d,"outputTokens":%d,"cacheReadInputTokens":0,"cacheCreationInputTokens":0}}}`+"\n", start, in, out)
	}
	record := func(to int, from, upto int64) {
		t.Helper()
		names := []string{"default", "work"}
		if err := e.st.RecordTransfers(ctx, []store.Transfer{{At: time.Now(), Mode: "copy", SessionID: "s1",
			FromAccount: names[1-to], ToAccount: names[to], Path: e.path(to, "-p/s1.jsonl"), From: from, To: upto}}); err != nil {
			t.Fatal(err)
		}
	}
	replace := func(acct int, content string) {
		t.Helper()
		tmp := e.path(acct, "-p/.s1.tmp")
		os.MkdirAll(filepath.Dir(tmp), 0o755)
		if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, e.path(acct, "-p/s1.jsonl")); err != nil {
			t.Fatal(err)
		}
	}
	missing := func(acct string) int64 {
		return e.int("SELECT COALESCE(SUM(output), 0) FROM responses WHERE kind = 'adjustment' AND account = ?", acct)
	}

	// default records 20 output tokens; its total says 50,020.
	a1 := prompt(0, "start") + asst(1, "m1", 10) + asst(2, "m2", 10) + total(2_000_000, 50_020)
	e.write(0, "-p/s1.jsonl", a1)
	e.run()
	time.Sleep(20 * time.Millisecond)
	// Copied to work, which records 20 more and carries the total to 80,040.
	replace(1, a1)
	record(1, 0, int64(len(a1)))
	e.run()
	b1 := a1 + prompt(3, "go on in work") + asst(4, "m3", 10) + asst(5, "m4", 10) + total(4_000_000, 80_040)
	replace(1, b1)
	e.run()
	if d, w := missing("default"), missing("work"); d != 50_000 || w != 30_000 {
		t.Fatalf("after work went on: default missing %d, work %d; want 50000 and 30000", d, w)
	}
	// Synced back by replacing default's file, which makes it the newer one.
	replace(0, b1)
	record(0, int64(len(a1)), int64(len(b1)))
	e.run()
	if d, w := missing("default"), missing("work"); d != 50_000 || w != 30_000 {
		t.Fatalf("after the sync back: default missing %d, work %d; want 50000 and 30000", d, w)
	}
	// default goes on: 10 recorded, the total grows by 5,010.
	e.appendTo(0, "-p/s1.jsonl", prompt(6, "back home")+asst(7, "m5", 10)+total(5_000_000, 85_050))
	e.run()
	if d, w := missing("default"), missing("work"); d != 55_000 || w != 30_000 {
		t.Fatalf("after default went on: default missing %d, work %d; want 55000 and 30000", d, w)
	}
}

// A session that continues another one starts its own transcript with the
// running total as it stood, plus its own work, and no earlier line to
// measure from. Its first snapshot only sets the base, so the parent's part
// is not counted twice; its later growth counts.
func TestContinuedSessionFirstSnapshotOnlySetsBase(t *testing.T) {
	e := newEnv(t, "default")
	start := time.Date(2026, 10, 1, 8, 59, 0, 0, time.UTC).UnixMilli()
	total := func(out int64) string {
		return fmt.Sprintf(`{"type":"cost-state","startTime":%d,"modelUsage":{"claude-opus-5-5":{"inputTokens":0,"outputTokens":%d,"cacheReadInputTokens":0,"cacheCreationInputTokens":0}}}`+"\n", start, out)
	}
	missing := func(sess string) int64 {
		return e.int("SELECT COALESCE(SUM(output), 0) FROM responses WHERE kind = 'adjustment' AND session_id = ?", sess)
	}
	e.write(0, "-p/s1.jsonl", prompt(0, "start")+asst(1, "m1", 10)+total(1_010))
	e.run()
	time.Sleep(20 * time.Millisecond)
	e.write(0, "-p/s2.jsonl", prompt(2, "continued")+asst(3, "m2", 10)+total(1_520)+asst(4, "m3", 10)+total(2_030))
	e.run()
	if p, c := missing("s1"), missing("s2"); p != 1_000 || c != 500 {
		t.Fatalf("parent missing %d, continuation %d; want 1000 and 500", p, c)
	}
	// A total that dips (two writers, or a damaged line) never counts growth twice.
	start++
	e.write(0, "-p/s3.jsonl", prompt(0, "dip")+total(5_000)+total(1_000)+total(6_000))
	e.run()
	if n := missing("s3"); n != 6_000 {
		t.Fatalf("dipping total counted %d, want 6000", n)
	}
}

// A copy whose original is not indexed (its account was removed, say)
// still counts the snapshots that came in with it: no other file can.
func TestCopiedSnapshotsCountWhenTheOriginalIsAbsent(t *testing.T) {
	e := newEnv(t, "work")
	start := time.Date(2026, 10, 1, 8, 59, 0, 0, time.UTC).UnixMilli()
	total := func(out int64) string {
		return fmt.Sprintf(`{"type":"cost-state","startTime":%d,"modelUsage":{"claude-opus-5-5":{"inputTokens":0,"outputTokens":%d,"cacheReadInputTokens":0,"cacheCreationInputTokens":0}}}`+"\n", start, out)
	}
	copied := prompt(0, "start") + total(1_000)
	e.write(0, "-p/s1.jsonl", copied+prompt(2, "more")+total(1_500))
	if err := e.st.RecordTransfers(context.Background(), []store.Transfer{{At: time.Now(), Mode: "copy", SessionID: "s1",
		FromAccount: "default", ToAccount: "work", Path: e.path(0, "-p/s1.jsonl"), From: 0, To: int64(len(copied))}}); err != nil {
		t.Fatal(err)
	}
	e.run()
	if n := e.int("SELECT COALESCE(SUM(output), 0) FROM responses WHERE kind = 'adjustment'"); n != 1_500 {
		t.Fatalf("missing %d, want 1500", n)
	}
}
