package codex

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lr00rl/ccli/internal/conv"
	"github.com/lr00rl/ccli/internal/transcript"
)

const (
	thread = "01a00000-0000-7000-b000-000000000001"
	parent = "01a00000-0000-7000-b000-000000000002"
)

var t0 = time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)

// rollout builds rollout lines.
type rollout struct {
	lines   []string
	at      time.Time
	ordinal int
	numbers bool
}

func (r *rollout) add(typ string, payload any) *rollout {
	l := map[string]any{"timestamp": r.at.Format("2006-01-02T15:04:05.000Z"), "type": typ, "payload": payload}
	if r.numbers {
		l["ordinal"] = r.ordinal
	}
	r.ordinal++
	b, _ := json.Marshal(l)
	r.lines = append(r.lines, string(b)+"\n")
	return r
}

func (r *rollout) wait(d time.Duration) *rollout { r.at = r.at.Add(d); return r }

func (r *rollout) meta(extra map[string]any) *rollout {
	m := map[string]any{"id": thread, "cwd": "/work/proj", "cli_version": "0.146.0", "originator": "codex-tui", "source": "cli"}
	for k, v := range extra {
		m[k] = v
	}
	return r.add("session_meta", m)
}

func (r *rollout) task() *rollout {
	return r.add("event_msg", map[string]any{"type": "task_started", "turn_id": fmt.Sprint(r.ordinal)})
}

func (r *rollout) context(model string) *rollout {
	return r.add("turn_context", map[string]any{"model": model, "cwd": "/work/proj"})
}

func (r *rollout) userMsg(text string) *rollout {
	return r.add("response_item", map[string]any{"type": "message", "role": "user",
		"content": []map[string]any{{"type": "input_text", "text": text}}})
}

func (r *rollout) userEvent(text string) *rollout {
	return r.add("event_msg", map[string]any{"type": "user_message", "message": text})
}

func (r *rollout) done() *rollout {
	return r.add("event_msg", map[string]any{"type": "task_complete", "duration_ms": 1500})
}

func u(in, cached, out int64) map[string]any {
	return map[string]any{"input_tokens": in, "cached_input_tokens": cached, "output_tokens": out, "reasoning_output_tokens": out / 2, "total_tokens": in + out}
}

func (r *rollout) tokens(total, last map[string]any) *rollout {
	return r.add("event_msg", map[string]any{"type": "token_count", "info": map[string]any{"total_token_usage": total, "last_token_usage": last}})
}

func (r *rollout) bytes() []byte { return []byte(strings.Join(r.lines, "")) }

func parse(t *testing.T, data []byte, kind transcript.Kind) transcript.Result {
	t.Helper()
	res, _, err := Parse(bytes.NewReader(data), kind, thread, transcript.State{})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func sum(rs []transcript.Response) (in, cached, out int64) {
	for _, r := range rs {
		in += r.Usage.Input
		cached += r.Usage.CacheRead
		out += r.Usage.Output
	}
	return
}

// Usage comes from each count's own last usage: repeats, the count after a
// compaction, a restarted counter, a missed count and a second writer are
// each handled.
func TestTokenAccounting(t *testing.T) {
	r := &rollout{at: t0}
	r.meta(nil).task().context("gpt-5.4").userEvent("go")
	r.tokens(u(100, 60, 10), u(100, 60, 10))   // first step
	r.tokens(u(100, 60, 10), u(100, 60, 10))   // repeated
	r.tokens(u(250, 160, 30), u(150, 100, 20)) // next step
	r.tokens(u(250, 160, 30), u(0, 0, 0))      // after a compaction: total unchanged
	r.tokens(u(400, 260, 40), u(50, 40, 5))    // a count went unwritten: credit the whole gap
	r.tokens(u(80, 50, 8), u(80, 50, 8))       // resumed: the counter restarts
	r.tokens(u(260, 150, 40), u(180, 100, 32)) // the restarted stream goes on
	r.tokens(u(500, 300, 50), u(100, 40, 10))  // the first stream goes on
	r.done()
	res := parse(t, r.bytes(), transcript.Main)
	in, cached, out := sum(res.Responses)
	// Streams: first 100 -> 250 -> 400 -> 500, second 80 -> 260.
	if want := int64(500 + 260); in+cached != want {
		t.Fatalf("input %d + cached %d = %d, want %d", in, cached, in+cached, want)
	}
	if want := int64(300 + 150); cached != want {
		t.Fatalf("cached %d, want %d", cached, want)
	}
	if want := int64(50 + 40); out != want {
		t.Fatalf("output %d, want %d", out, want)
	}
	for _, rs := range res.Responses {
		if rs.Model != "gpt-5.4" || rs.Turn != 1 {
			t.Fatalf("response %+v", rs)
		}
	}
}

// A person's prompt arrives as an event and as a message (up to CLI 0.146)
// or as a completed item (since); it opens the turn once. Injected context
// and messages sent while the model worked open no turn.
func TestPrompts(t *testing.T) {
	r := &rollout{at: t0}
	r.meta(nil)
	r.task().userMsg("<environment_context>\n<cwd>.</cwd>\n</environment_context>").context("gpt-5.4").
		userMsg("first question").userEvent("first question").userEvent("and also this").done()
	r.task().add("event_msg", map[string]any{"type": "item_completed", "item": map[string]any{"type": "UserMessage",
		"content": []map[string]any{{"type": "text", "text": "second question"}}}}).done()
	r.task().userMsg("third, only as a message").done()
	r.task().add("response_item", map[string]any{"type": "message", "role": "developer",
		"content": []map[string]any{{"type": "input_text", "text": "<permissions instructions>"}}}).done()
	res := parse(t, r.bytes(), transcript.Main)
	var got []string
	for _, tr := range res.Turns {
		got = append(got, tr.Text)
	}
	if strings.Join(got, "|") != "first question|second question|third, only as a message" || res.TypedPrompts != 3 {
		t.Fatalf("turns %q, typed %d", got, res.TypedPrompts)
	}
	if res.FirstPrompt != "first question" {
		t.Fatalf("first prompt %q", res.FirstPrompt)
	}
}

// An older fork copies its parent's history, every line at the time of the
// fork; only what follows counts, and the fork's counter continues from
// the parent's total.
func TestForkCopiedBurst(t *testing.T) {
	r := &rollout{at: t0}
	r.meta(map[string]any{"forked_from_id": parent})
	r.add("session_meta", map[string]any{"id": parent, "cwd": "/work/proj"}) // the parent's, copied
	r.task().context("gpt-5.4").userEvent("parent question")
	r.tokens(u(1000, 800, 100), u(1000, 800, 100))
	r.done()
	r.task().userEvent("forked question") // the fork's own first turn, started in the burst
	r.wait(3 * time.Second)
	r.tokens(u(1300, 1000, 130), u(300, 200, 30))
	r.done()
	res := parse(t, r.bytes(), transcript.Main)
	if len(res.Turns) != 1 || res.Turns[0].Text != "forked question" {
		t.Fatalf("turns %+v", res.Turns)
	}
	if in, cached, out := sum(res.Responses); in+cached != 300 || out != 30 {
		t.Fatalf("counted %d+%d in, %d out; want the fork's own 300 and 30", in, cached, out)
	}
}

// A newer fork marks the copied lines by ordinal.
func TestForkByOrdinal(t *testing.T) {
	r := &rollout{at: t0, numbers: true}
	r.meta(map[string]any{"forked_from_id": parent, "subagent_history_start_ordinal": 4})
	r.task().userEvent("copied question").done()
	r.task().userEvent("own question")
	r.tokens(u(200, 100, 20), u(200, 100, 20)).done()
	res := parse(t, r.bytes(), transcript.Main)
	if len(res.Turns) != 1 || res.Turns[0].Text != "own question" || len(res.Responses) != 1 {
		t.Fatalf("turns %+v, %d responses", res.Turns, len(res.Responses))
	}
}

// A spawned agent has no turns of its own; its first prompt is its task.
func TestSubagent(t *testing.T) {
	r := &rollout{at: t0}
	r.meta(map[string]any{"source": map[string]any{"subagent": map[string]any{"thread_spawn": map[string]any{
		"parent_thread_id": parent, "depth": 1, "agent_role": "explorer"}}}})
	r.task().context("gpt-5.6-luna").userMsg("find the config keys")
	r.tokens(u(50, 20, 5), u(50, 20, 5)).done()
	res, meta, err := Parse(bytes.NewReader(r.bytes()), transcript.Subagent, thread, transcript.State{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Turns) != 0 || res.FirstPrompt != "find the config keys" || res.AgentID != thread || res.Attribution != "explorer" {
		t.Fatalf("subagent result %+v", res)
	}
	if meta == nil || meta.Subagent != "spawn" || meta.Parent != parent || meta.Depth != 1 {
		t.Fatalf("meta %+v", meta)
	}
}

// Rate limits are told apart by window length, and only the main Codex
// limit counts.
func TestLimits(t *testing.T) {
	r := &rollout{at: t0}
	r.meta(nil).task().userEvent("go")
	r.add("event_msg", map[string]any{"type": "token_count", "rate_limits": map[string]any{"limit_id": "codex", "plan_type": "pro",
		"primary": map[string]any{"used_percent": 46.0, "window_minutes": 10080, "resets_at": 1760000000}}})
	r.wait(time.Minute)
	r.add("event_msg", map[string]any{"type": "token_count", "rate_limits": map[string]any{"limit_id": "codex_bengalfox",
		"primary": map[string]any{"used_percent": 99.0, "window_minutes": 300, "resets_at": 1760000000}}})
	res := parse(t, r.bytes(), transcript.Main)
	l := res.Limits
	if l == nil || l.Plan != "pro" || l.FiveHour != nil || l.Weekly == nil || l.Weekly.UsedPercent != 46 {
		t.Fatalf("limits %+v", l)
	}
}

// Counts written before any line named a model get the first one named.
func TestModelBackfill(t *testing.T) {
	r := &rollout{at: t0}
	r.meta(nil).task().userEvent("go")
	r.tokens(u(100, 50, 10), u(100, 50, 10))
	r.context("gpt-5.5")
	res := parse(t, r.bytes(), transcript.Main)
	if len(res.Responses) != 1 || res.Responses[0].Model != "gpt-5.5" {
		t.Fatalf("responses %+v", res.Responses)
	}
}

// An incremental parse in two pieces finds the same as one parse.
func TestSplitParse(t *testing.T) {
	r := &rollout{at: t0}
	r.meta(map[string]any{"forked_from_id": parent})
	r.task().context("gpt-5.4").userEvent("parent question")
	r.tokens(u(1000, 800, 100), u(1000, 800, 100)).done()
	r.task().userEvent("forked question")
	r.wait(3 * time.Second)
	r.tokens(u(1300, 1000, 130), u(300, 200, 30))
	r.tokens(u(1300, 1000, 130), u(300, 200, 30)).done()
	r.task().userEvent("next")
	r.tokens(u(1500, 1100, 150), u(200, 100, 20)).done()
	data := r.bytes()
	whole := parse(t, data, transcript.Main)
	for cut := 1; cut < len(r.lines); cut++ {
		n := len(strings.Join(r.lines[:cut], ""))
		a, _, err := Parse(bytes.NewReader(data[:n]), transcript.Main, thread, transcript.State{})
		if err != nil {
			t.Fatal(err)
		}
		st := a.State
		st.Base = int64(n)
		b, _, err := Parse(bytes.NewReader(data[n:]), transcript.Main, thread, st)
		if err != nil {
			t.Fatal(err)
		}
		if len(a.Responses)+len(b.Responses) != len(whole.Responses) || len(a.Turns)+len(b.Turns) != len(whole.Turns) {
			t.Fatalf("cut after line %d: %d+%d responses, %d+%d turns; whole %d, %d", cut,
				len(a.Responses), len(b.Responses), len(a.Turns), len(b.Turns), len(whole.Responses), len(whole.Turns))
		}
	}
}

// The reader shows a turn's prompt, reasoning, tool calls with their
// output, and reply; a fork's turn 0 is one notice.
func TestReadTurn(t *testing.T) {
	r := &rollout{at: t0}
	r.meta(nil)
	r.task().context("gpt-5.4").userMsg("fix the test").userEvent("fix the test")
	r.add("response_item", map[string]any{"type": "reasoning", "summary": []map[string]any{{"type": "summary_text", "text": "Reading the test first."}}})
	r.add("response_item", map[string]any{"type": "function_call", "name": "exec_command", "arguments": `{"cmd":"go test ./..."}`, "call_id": "c1"})
	r.add("response_item", map[string]any{"type": "function_call_output", "call_id": "c1", "output": "FAIL"})
	r.userEvent("use the table test")
	r.add("response_item", map[string]any{"type": "message", "role": "assistant", "content": []map[string]any{{"type": "output_text", "text": "Fixed."}}})
	r.done()
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	if err := os.WriteFile(path, r.bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	res := parse(t, r.bytes(), transcript.Main)
	tr, err := ReadTurn(path, res.Turns[0].Offset, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, e := range tr.Entries {
		kinds = append(kinds, e.Kind)
	}
	want := []string{conv.KindPrompt, conv.KindThinking, conv.KindTool, conv.KindQueued, conv.KindText}
	if strings.Join(kinds, " ") != strings.Join(want, " ") || tr.Speaker != Speaker {
		t.Fatalf("entries %v, speaker %q", kinds, tr.Speaker)
	}
	c := tr.Entries[2].Call
	if c.Name != "exec_command" || c.Summary != "go test ./..." || c.Output != "FAIL" || !c.Done {
		t.Fatalf("call %+v", c)
	}

	fork := &rollout{at: t0}
	fork.meta(map[string]any{"forked_from_id": parent}).task().userEvent("copied")
	if err := os.WriteFile(path, fork.bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	zero, err := ReadTurn(path, 0, 0, 0)
	if err != nil || len(zero.Entries) != 1 || zero.Entries[0].Kind != conv.KindNotice {
		t.Fatalf("fork turn 0: %+v %v", zero, err)
	}
}
