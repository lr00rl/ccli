package main

import (
	"encoding/json"
	"math/rand/v2"
	"os"
	"path/filepath"
	"time"
)

// The demo's Codex home: a few threads in the formats Codex has written,
// with a spawned agent, logged rate limits and a code-mode thread. It uses
// its own random source so the Claude Code sessions keep their ids.

type cxUsage struct{ in, cached, out, reasoning int64 }

type cxThread struct {
	g        *cxGen
	id       string
	t        time.Time
	lines    []string
	total    cxUsage
	model    string
	ordinals bool // CLI 0.147 and later number every line
	n        int
	turns    int
}

type cxGen struct {
	home string
	rng  *rand.Rand
	now  time.Time
}

func (g *gen) codex() {
	cg := &cxGen{home: filepath.Join(g.home, ".codex"), rng: rand.New(rand.NewPCG(13, 17)), now: g.now}

	gw := cg.thread("", cg.now.Add(-90*time.Minute), "/Users/alex/acme/api-gateway", "gpt-5.5", "codex-tui", "", false)
	gw.turn("Add per-client rate limiting to the public API. Keep the limits in config, not in code.",
		"I'll look at how requests reach the handlers first, then add a token bucket per client key.",
		[]cxCall{{"exec_command", `{"cmd":"rg -n \"func (s \\*Server) Handle\" internal/http"}`, "internal/http/server.go:42:func (s *Server) Handle(w http.ResponseWriter, r *http.Request) {"},
			{"exec_command", `{"cmd":"sed -n 1,80p internal/http/server.go"}`, "package http\n\n// Server routes public API requests."}},
		"Requests go through `Server.Handle`. I'll add the limiter as middleware there.", 3)
	agent, spawnAt := cg.newID(), gw.t
	gw.spawn(agent, "Check how other services configure rate limits, so the config keys match.",
		"Spawned an agent to check the existing config keys.")
	gw.turn("Good. Write it, with tests for burst and refill.",
		"Writing the middleware and its tests.",
		[]cxCall{{"apply_patch", "*** Begin Patch\n*** Add File: internal/http/limit.go\n+package http\n*** Add File: internal/http/limit_test.go\n+package http\n*** End Patch", "Done!"},
			{"exec_command", `{"cmd":"go test ./internal/http/..."}`, "ok  \tacme/api-gateway/internal/http\t0.31s"}},
		"Added `limit.go` with a token bucket per client key, read from `rate_limit.per_client` in the config. Both tests pass.", 5)
	gw.turn("Explain how a burst is handled, in two sentences.", "", nil,
		"A client can send up to the bucket size at once; after that each request waits for a token. Tokens refill at the configured rate, so short bursts pass and sustained floods are slowed.", 1)
	gw.write()

	sub := cg.thread(agent, spawnAt.Add(10*time.Second), "/Users/alex/acme/api-gateway", "gpt-5.6-luna", "codex-tui", gw.id, false)
	sub.turn("Check how other services configure rate limits, so the config keys match.",
		"", []cxCall{{"exec_command", `{"cmd":"rg -n rate_limit --glob '*.yaml'"}`, "deploy/billing.yaml:12:rate_limit:\ndeploy/search.yaml:8:rate_limit:"}},
		"Both services use `rate_limit.per_client` and `rate_limit.burst`. Use the same keys.", 1)
	sub.write()

	blog := cg.thread("", cg.now.Add(-3*24*time.Hour), "/Users/alex/code/blog", "gpt-5.6-sol", "codex-tui", "", true)
	blog.turn("Find broken links in the posts and fix the ones that moved.",
		"Checking every link in the posts.",
		[]cxCall{{"exec", "const out = await tools.exec_command({cmd: \"npm run check-links\"});", "3 broken links"}},
		"Fixed two links that moved; the third site is gone, so I removed that sentence.", 4)
	blog.write()

	quick := cg.thread("", cg.now.Add(-6*24*time.Hour), "/Users/alex/code/dotfiles", "gpt-5.4-mini", "codex_exec", "", false)
	quick.turn("Summarize what changed in this repo this week.", "", []cxCall{{"exec_command", `{"cmd":"git log --since=1.week --oneline"}`, "a1b2c3d Add tmux theme"}},
		"One change this week: a new tmux theme.", 1)
	quick.write()
}

func (cg *cxGen) newID() string {
	const digits = "0123456789abcdef"
	b := make([]byte, 32)
	for i := range b {
		b[i] = digits[cg.rng.IntN(16)]
	}
	h := "01a" + string(b[3:])
	return h[:8] + "-" + h[8:12] + "-7" + h[13:16] + "-b" + h[17:20] + "-" + h[20:32]
}

// thread starts a rollout with its session_meta line (id empty: a new one);
// parent makes it an agent spawned by that thread.
func (cg *cxGen) thread(id string, start time.Time, cwd, model, originator, parent string, ordinals bool) *cxThread {
	if id == "" {
		id = cg.newID()
	}
	t := &cxThread{g: cg, id: id, t: start, model: model, ordinals: ordinals}
	meta := map[string]any{"id": t.id, "timestamp": ts(start), "cwd": cwd, "originator": originator,
		"cli_version": "0.146.0", "model_provider": "openai", "source": "cli",
		"git": map[string]any{"branch": "main"}}
	if ordinals {
		meta["cli_version"], meta["history_mode"] = "0.150.1", "paginated"
	}
	if parent != "" {
		meta["source"] = map[string]any{"subagent": map[string]any{"thread_spawn": map[string]any{
			"parent_thread_id": parent, "depth": 1, "agent_nickname": "Ada", "agent_role": "explorer"}}}
		meta["parent_thread_id"], meta["session_id"], meta["thread_source"] = parent, parent, "subagent"
	}
	t.emit("session_meta", meta)
	return t
}

func (t *cxThread) emit(typ string, payload map[string]any) {
	line := map[string]any{"timestamp": ts(t.t), "type": typ, "payload": payload}
	if t.ordinals {
		line["ordinal"] = t.n
	}
	t.n++
	b, _ := json.Marshal(line)
	t.lines = append(t.lines, string(b)+"\n")
}

func (t *cxThread) tick(d time.Duration) { t.t = t.t.Add(d) }

type cxCall struct{ name, input, output string }

// tokens records one model call: its own usage and the running total, with
// the rate limits Codex logs alongside.
func (t *cxThread) tokens(in, out int64) {
	g := t.g
	cached := in * 9 / 10
	reasoning := out / 3
	t.total.in += in
	t.total.cached += cached
	t.total.out += out
	t.total.reasoning += reasoning
	u := func(i, c, o, r int64) map[string]any {
		return map[string]any{"input_tokens": i, "cached_input_tokens": c, "output_tokens": o, "reasoning_output_tokens": r, "total_tokens": i + o}
	}
	weekReset := g.now.Add(4*24*time.Hour + 3*time.Hour).Unix()
	t.emit("event_msg", map[string]any{"type": "token_count",
		"info": map[string]any{
			"total_token_usage":    u(t.total.in, t.total.cached, t.total.out, t.total.reasoning),
			"last_token_usage":     u(in, cached, out, reasoning),
			"model_context_window": 258400,
		},
		"rate_limits": map[string]any{"limit_id": "codex", "plan_type": "pro",
			"primary":   map[string]any{"used_percent": 18.0, "window_minutes": 300, "resets_at": g.now.Add(3*time.Hour + 10*time.Minute).Unix()},
			"secondary": map[string]any{"used_percent": 46.0, "window_minutes": 10080, "resets_at": weekReset}},
	})
}

// turn writes one turn: the prompt, the model's reasoning, its calls and
// their output, and its reply. In the newer format a prompt arrives as a
// completed item and commands run inside code-mode exec.
func (t *cxThread) turn(prompt, thinking string, calls []cxCall, reply string, steps int) {
	g := t.g
	t.turns++
	turnID := g.newID()
	start := t.t
	t.emit("event_msg", map[string]any{"type": "task_started", "turn_id": turnID, "started_at": t.t.Unix(), "model_context_window": 258400})
	if t.turns == 1 {
		t.emit("response_item", map[string]any{"type": "message", "role": "user", "content": []map[string]any{
			{"type": "input_text", "text": "<environment_context>\n  <cwd>.</cwd>\n</environment_context>"}}})
	}
	t.emit("turn_context", map[string]any{"turn_id": turnID, "cwd": ".", "model": t.model, "effort": "high", "summary": "auto"})
	t.tick(2 * time.Second)
	t.emit("response_item", map[string]any{"type": "message", "role": "user", "content": []map[string]any{{"type": "input_text", "text": prompt}}})
	if t.ordinals {
		t.emit("event_msg", map[string]any{"type": "item_completed", "turn_id": turnID,
			"item": map[string]any{"type": "UserMessage", "id": g.newID(), "content": []map[string]any{{"type": "text", "text": prompt}}}})
	} else {
		t.emit("event_msg", map[string]any{"type": "user_message", "message": prompt, "images": []any{}})
	}
	if thinking != "" {
		t.tick(4 * time.Second)
		t.emit("response_item", map[string]any{"type": "reasoning", "summary": []map[string]any{{"type": "summary_text", "text": thinking}}})
	}
	for _, c := range calls {
		t.tick(time.Duration(5+g.rng.IntN(20)) * time.Second)
		id := "call_" + g.newID()[:18]
		if c.name == "exec" || c.name == "apply_patch" {
			t.emit("response_item", map[string]any{"type": "custom_tool_call", "name": c.name, "input": c.input, "call_id": id, "status": "completed"})
		} else {
			t.emit("response_item", map[string]any{"type": "function_call", "name": c.name, "arguments": c.input, "call_id": id})
		}
		t.tokens(int64(30_000+g.rng.IntN(40_000)*steps), int64(300+g.rng.IntN(900)))
		t.tick(time.Duration(2+g.rng.IntN(6)) * time.Second)
		if c.name == "exec" {
			t.emit("event_msg", map[string]any{"type": "item_completed", "turn_id": turnID, "item": map[string]any{
				"type": "CommandExecution", "id": "exec-" + g.newID()[:8], "command": []string{"npm", "run", "check-links"},
				"exit_code": 0, "stdout": c.output + "\n", "stderr": "", "status": "completed"}})
		}
		kind := "function_call_output"
		if c.name == "exec" || c.name == "apply_patch" {
			kind = "custom_tool_call_output"
		}
		t.emit("response_item", map[string]any{"type": kind, "call_id": id, "output": c.output})
	}
	t.tick(time.Duration(6+g.rng.IntN(20)) * time.Second)
	t.emit("response_item", map[string]any{"type": "message", "role": "assistant", "content": []map[string]any{{"type": "output_text", "text": reply}}})
	t.tokens(int64(40_000+g.rng.IntN(30_000)), int64(400+g.rng.IntN(1200)))
	t.emit("event_msg", map[string]any{"type": "task_complete", "turn_id": turnID, "duration_ms": t.t.Sub(start).Milliseconds(), "last_agent_message": reply})
	t.tick(time.Duration(2+g.rng.IntN(8)) * time.Minute)
}

// spawn writes a turn that starts another agent: the spawn_agent call and
// the item that names the thread it started.
func (t *cxThread) spawn(agent, task, reply string) {
	g := t.g
	turnID := g.newID()
	start := t.t
	t.emit("event_msg", map[string]any{"type": "task_started", "turn_id": turnID, "started_at": t.t.Unix()})
	t.emit("turn_context", map[string]any{"turn_id": turnID, "cwd": ".", "model": t.model})
	prompt := "Before you write it, check how the other services name their limits."
	t.emit("response_item", map[string]any{"type": "message", "role": "user", "content": []map[string]any{{"type": "input_text", "text": prompt}}})
	t.emit("event_msg", map[string]any{"type": "user_message", "message": prompt, "images": []any{}})
	t.turns++
	id := "call_" + g.newID()[:18]
	args, _ := json.Marshal(map[string]any{"message": task, "agent_type": "explorer"})
	t.tick(5 * time.Second)
	t.emit("response_item", map[string]any{"type": "function_call", "namespace": "collaboration", "name": "spawn_agent", "arguments": string(args), "call_id": id})
	t.tokens(52_000, 600)
	t.emit("event_msg", map[string]any{"type": "item_completed", "turn_id": turnID, "item": map[string]any{
		"type": "CollabAgentToolCall", "id": id, "tool": "spawn_agent", "receiver_thread_ids": []string{agent}, "status": "completed"}})
	t.emit("response_item", map[string]any{"type": "function_call_output", "call_id": id, "output": `{"agent_id":"` + agent + `"}`})
	t.tick(40 * time.Second)
	t.emit("response_item", map[string]any{"type": "message", "role": "assistant", "content": []map[string]any{{"type": "output_text", "text": reply}}})
	t.tokens(55_000, 300)
	t.emit("event_msg", map[string]any{"type": "task_complete", "turn_id": turnID, "duration_ms": t.t.Sub(start).Milliseconds()})
	t.tick(3 * time.Minute)
}

func (t *cxThread) write() {
	g := t.g
	day := filepath.Join(g.home, "sessions", t.firstTime().Format("2006/01/02"))
	name := "rollout-" + t.firstTime().Format("2006-01-02T15-04-05") + "-" + t.id + ".jsonl"
	if err := os.MkdirAll(day, 0o755); err != nil {
		panic(err)
	}
	var data []byte
	for _, l := range t.lines {
		data = append(data, l...)
	}
	if err := os.WriteFile(filepath.Join(day, name), data, 0o644); err != nil {
		panic(err)
	}
}

func (t *cxThread) firstTime() time.Time {
	var l struct {
		Timestamp string `json:"timestamp"`
	}
	json.Unmarshal([]byte(t.lines[0]), &l)
	tm, _ := time.Parse("2006-01-02T15:04:05.000Z", l.Timestamp)
	return tm.Local()
}
