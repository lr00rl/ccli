package transcript

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func ts(min float64) string {
	base := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	return base.Add(time.Duration(min * float64(time.Minute))).Format(time.RFC3339Nano)
}

func asst(at float64, id, model string, in, out, read, cw int64, extra string) string {
	return fmt.Sprintf(`{"type":"assistant","timestamp":%q,"requestId":"req_%s","message":{"id":%q,"model":%q,"usage":{"input_tokens":%d,"output_tokens":%d,"cache_read_input_tokens":%d,"cache_creation_input_tokens":%d%s,"speed":"standard"},"content":[{"type":"text","text":"x"}]}}`,
		ts(at), id, id, model, in, out, read, cw, extra)
}

func userText(at float64, text, extra string) string {
	return fmt.Sprintf(`{"type":"user","timestamp":%q,"cwd":"/work/proj","message":{"role":"user","content":%q}%s}`, ts(at), text, extra)
}

func parse(t *testing.T, kind Kind, lines ...string) Result {
	t.Helper()
	res, err := Parse(strings.NewReader(strings.Join(lines, "\n")+"\n"), kind, State{})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestDuplicateLinesCountOnce(t *testing.T) {
	res := parse(t, Main,
		asst(0, "msg_1", "claude-opus-5-5", 10, 20, 300, 40, ""),
		asst(0.01, "msg_1", "claude-opus-5-5", 10, 20, 300, 40, ""),
		asst(0.02, "msg_1", "claude-opus-5-5", 10, 20, 300, 40, ""),
		asst(1, "msg_2", "claude-opus-5-5", 1, 2, 3, 4, ""),
	)
	if len(res.Responses) != 2 {
		t.Fatalf("responses = %d, want 2", len(res.Responses))
	}
	if u := res.Responses[0].Usage; u.Input != 10 || u.Output != 20 || u.CacheRead != 300 || u.Cache5m != 40 {
		t.Fatalf("usage = %+v", u)
	}
}

func TestRevisedUsageReplacesEarlier(t *testing.T) {
	res := parse(t, Main,
		asst(0, "msg_1", "claude-opus-5-5", 10, 1, 0, 0, ""),
		asst(0.5, "msg_1", "claude-opus-5-5", 10, 99, 0, 0, ""),
	)
	if len(res.Responses) != 1 || res.Responses[0].Usage.Output != 99 {
		t.Fatalf("got %+v", res.Responses)
	}
	if res.Responses[0].TS != parseTS(ts(0)) {
		t.Fatalf("revision moved the timestamp")
	}
}

func TestCacheWriteSplit(t *testing.T) {
	res := parse(t, Main,
		asst(0, "a", "claude-opus-5-5", 1, 1, 0, 30, `,"cache_creation":{"ephemeral_5m_input_tokens":10,"ephemeral_1h_input_tokens":20}`),
		asst(1, "b", "claude-opus-5-5", 1, 1, 0, 30, ""),
	)
	if u := res.Responses[0].Usage; u.Cache5m != 10 || u.Cache1h != 20 {
		t.Fatalf("split usage = %+v", u)
	}
	if u := res.Responses[1].Usage; u.Cache5m != 30 || u.Cache1h != 0 {
		t.Fatalf("unsplit usage = %+v", u)
	}
}

func TestSyntheticSkippedAndRequestIDFallback(t *testing.T) {
	res := parse(t, Main,
		asst(0, "", "claude-opus-5-5", 5, 5, 0, 0, ""),
		asst(1, "syn", "<synthetic>", 5, 5, 0, 0, ""),
	)
	if len(res.Responses) != 1 || res.Responses[0].Key != "req_" {
		t.Fatalf("got %+v", res.Responses)
	}
}

func TestToolsAndSkills(t *testing.T) {
	tool := func(at float64, id, blockID, name, input string) string {
		return fmt.Sprintf(`{"type":"assistant","timestamp":%q,"message":{"id":%q,"model":"claude-opus-5-5","usage":{"input_tokens":1,"output_tokens":1},"content":[{"type":"tool_use","id":%q,"name":%q,"input":%s}]}}`, ts(at), id, blockID, name, input)
	}
	res := parse(t, Main,
		tool(0, "m1", "tu_1", "Bash", `{"command":"ls"}`),
		tool(0.1, "m1", "tu_1", "Bash", `{"command":"ls"}`), // same block again
		tool(1, "m2", "tu_2", "Skill", `{"skill":"humanizer"}`),
		tool(2, "m3", "tu_3", "Skill", `{"skill":"humanizer","args":"x"}`),
	)
	if res.ToolCalls != 3 || res.Tools["Bash"] != 1 || res.Tools["Skill"] != 2 || res.Skills["humanizer"] != 2 {
		t.Fatalf("tools=%v skills=%v calls=%d", res.Tools, res.Skills, res.ToolCalls)
	}
}

func TestPromptKinds(t *testing.T) {
	toolResult := fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1","content":"ok"}]}}`, ts(1))
	blocks := fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"role":"user","content":[{"type":"text","text":"second prompt"}]}}`, ts(2))
	res := parse(t, Main,
		userText(0, "<command-name>/clear</command-name>", ""),
		toolResult,
		blocks,
		userText(3, "fix   the\nbug", ""),
		userText(4, "a task finished", `,"promptSource":"system","origin":{"kind":"task-notification"}`),
		userText(5, "typed with origin", `,"origin":{"kind":"human"}`),
		userText(6, "meta", `,"isMeta":true`),
		userText(7, "side", `,"isSidechain":true`),
	)
	if res.TypedPrompts != 4 || res.OtherPrompts != 1 {
		t.Fatalf("typed=%d other=%d", res.TypedPrompts, res.OtherPrompts)
	}
	if res.FirstPrompt != "second prompt" {
		t.Fatalf("first prompt = %q", res.FirstPrompt)
	}
	if res.Commands["/clear"] != 1 || len(res.Commands) != 1 {
		t.Fatalf("commands = %v", res.Commands)
	}
	if res.Cwd != "/work/proj" {
		t.Fatalf("cwd = %q", res.Cwd)
	}
}

func TestTitlesLatestWins(t *testing.T) {
	res := parse(t, Main,
		`{"type":"ai-title","aiTitle":"first ai","sessionId":"s"}`,
		`{"type":"ai-title","aiTitle":"second   ai","sessionId":"s"}`,
		`{"type":"custom-title","customTitle":"mine","sessionId":"s"}`,
		`{"type":"summary","summary":"sum"}`,
	)
	if res.AITitle != "second ai" || res.CustomTitle != "mine" || res.Summary != "sum" {
		t.Fatalf("titles: %q %q %q", res.AITitle, res.CustomTitle, res.Summary)
	}
}

func TestActiveTimeAndContinuation(t *testing.T) {
	lines := []string{
		userText(0, "a", ""),
		asst(2, "m1", "claude-opus-5-5", 1, 1, 0, 0, ""),    // +2 min
		asst(4, "m2", "claude-opus-5-5", 1, 1, 0, 0, ""),    // +2 min
		userText(30, "b", ""),                               // +26 min: idle
		asst(31, "m3", "claude-opus-5-5", 1, 1, 0, 0, ""),   // +1 min
		asst(62, "m4", "claude-opus-5-5", 1, 1, 0, 0, ""),   // +31 min: idle
		asst(63.5, "m5", "claude-opus-5-5", 1, 1, 0, 0, ""), // +1.5 min, next hour
	}
	whole := parse(t, Main, lines...)
	if want := int64(6.5 * 60_000); whole.ActiveMS != want {
		t.Fatalf("active = %d, want %d", whole.ActiveMS, want)
	}
	h9 := parseTS(ts(0))
	if whole.Activity[h9] != 5*60_000 || whole.Activity[h9+3_600_000] != 90_000 {
		t.Fatalf("activity = %v", whole.Activity)
	}
	// The same input split in two parses gives the same total.
	first, _ := Parse(strings.NewReader(strings.Join(lines[:3], "\n")+"\n"), Main, State{})
	second, _ := Parse(strings.NewReader(strings.Join(lines[3:], "\n")+"\n"), Main, first.State)
	if first.ActiveMS+second.ActiveMS != whole.ActiveMS {
		t.Fatalf("split active = %d + %d, want %d", first.ActiveMS, second.ActiveMS, whole.ActiveMS)
	}
	if whole.FirstTS != parseTS(ts(0)) || whole.LastTS != parseTS(ts(63.5)) {
		t.Fatalf("first/last wrong")
	}
}

func TestSubagentCountsTokensOnly(t *testing.T) {
	res := parse(t, Subagent,
		userText(0, "do the task", `,"isSidechain":true,"agentId":"a1"`),
		asst(1, "s1", "claude-sonnet-5-5", 7, 8, 0, 0, ""),
	)
	if len(res.Responses) != 1 || res.TypedPrompts != 0 || res.ActiveMS != 0 || res.AgentID != "a1" {
		t.Fatalf("subagent: %+v", res)
	}
}

func TestPartialLastLineIsNotConsumed(t *testing.T) {
	full := asst(0, "m1", "claude-opus-5-5", 1, 1, 0, 0, "") + "\n"
	half := asst(1, "m2", "claude-opus-5-5", 1, 1, 0, 0, "")
	res, err := Parse(strings.NewReader(full+half[:40]), Main, State{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Consumed != int64(len(full)) || len(res.Responses) != 1 {
		t.Fatalf("consumed %d of %d, responses %d", res.Consumed, len(full), len(res.Responses))
	}
}

func TestLongAndOversizedLines(t *testing.T) {
	long := fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"content":[{"type":"tool_result","content":"%s"}]}}`, ts(0), strings.Repeat("x", 3<<20))
	ok := asst(1, "m1", "claude-opus-5-5", 1, 1, 0, 0, "")
	res := parse(t, Main, long, ok)
	if res.Skipped != 0 || len(res.Responses) != 1 || res.Lines != 2 {
		t.Fatalf("long line: skipped=%d responses=%d lines=%d", res.Skipped, len(res.Responses), res.Lines)
	}
	saved := MaxLine
	MaxLine = 2 << 20
	defer func() { MaxLine = saved }()
	input := long + "\n" + ok + "\n"
	res, _ = Parse(strings.NewReader(input), Main, State{})
	if res.Skipped != 1 || len(res.Responses) != 1 || res.Consumed != int64(len(input)) {
		t.Fatalf("oversized: skipped=%d responses=%d consumed=%d/%d", res.Skipped, len(res.Responses), res.Consumed, len(input))
	}
}

func TestBadJSONAndLinks(t *testing.T) {
	res := parse(t, Main,
		`{not json`,
		`{"type":"continued-in","continuedInSessionId":"next-1","sessionId":"s"}`,
		`{"type":"continued-in","continuedInSessionId":"next-1","sessionId":"s"}`,
		`{"type":"cost-state","totalCostUSD":1.5}`,
		`{"type":"cost-state","totalCostUSD":2.25}`,
		`{"type":"assistant","teamName":"session-abcd1234","agentName":"reviewer","bridgeSessionId":"br_1","message":{"id":"x","model":"m","usage":{"input_tokens":1}}}`,
	)
	if res.Skipped != 1 {
		t.Fatalf("skipped = %d", res.Skipped)
	}
	if len(res.ContinuedIn) != 1 || res.ContinuedIn[0] != "next-1" {
		t.Fatalf("continued = %v", res.ContinuedIn)
	}
	if !res.HasCost || res.CostUSD != 2.25 {
		t.Fatalf("cost = %v %v", res.HasCost, res.CostUSD)
	}
	if res.TeamName != "session-abcd1234" || res.AgentName != "reviewer" || res.BridgeSessionID != "br_1" {
		t.Fatalf("links: %q %q %q", res.TeamName, res.AgentName, res.BridgeSessionID)
	}
}

// Claude Code's /rename writes an agent-name entry with the new name. It
// names the session, so it does not make the session a teammate.
func TestRenameIsNotATeammate(t *testing.T) {
	res := parse(t, Main,
		`{"type":"user","message":{"role":"user","content":"hello"}}`,
		`{"type":"agent-name","agentName":"payments audit","sessionId":"s"}`,
		`{"type":"custom-title","customTitle":"payments audit","sessionId":"s"}`,
	)
	if res.AgentName != "" || res.TeamName != "" {
		t.Fatalf("a renamed session reads as teammate %q of team %q", res.AgentName, res.TeamName)
	}
}

func TestCostStateProcesses(t *testing.T) {
	cs := func(start int64, cost float64, out int64) string {
		return fmt.Sprintf(`{"type":"cost-state","startTime":%d,"totalCostUSD":%v,"modelUsage":{"claude-opus-5-5":{"inputTokens":1,"outputTokens":%d,"cacheReadInputTokens":3,"cacheCreationInputTokens":4,"webSearchRequests":0,"costUSD":%v},"claude-haiku-5-5":{"inputTokens":9,"outputTokens":9,"costUSD":0.01}}}`, start, cost, out, cost)
	}
	res := parse(t, Main,
		asst(1, "m1", "claude-opus-5-5", 1, 1, 0, 0, ""),
		cs(100, 1.5, 10),
		asst(2, "m2", "claude-opus-5-5", 1, 1, 0, 0, ""),
		cs(100, 2.5, 20), // a later snapshot of the same process replaces it
		cs(200, 0.5, 5),
	)
	if len(res.Processes) != 2 {
		t.Fatalf("processes = %+v", res.Processes)
	}
	p := res.Processes[0]
	if p.Start != 100 || p.End != parseTS(ts(2)) || len(p.Models) != 2 {
		t.Fatalf("process = %+v", p)
	}
	if m := p.Models[1]; m.Model != "claude-opus-5-5" || m.Output != 20 || m.CostUSD != 2.5 || m.CacheWrite != 4 {
		t.Fatalf("model usage = %+v", m)
	}
}

func TestTurnsKindsAndOffsets(t *testing.T) {
	agentCall := fmt.Sprintf(`{"type":"assistant","timestamp":%q,"message":{"id":"m2","model":"claude-opus-5-5","usage":{"input_tokens":1,"output_tokens":1},"content":[{"type":"tool_use","id":"tu_a","name":"Agent","input":{"description":"look"}},{"type":"tool_use","id":"tu_w","name":"Workflow","input":{}},{"type":"tool_use","id":"tu_b","name":"Bash","input":{}}]}}`, ts(2))
	agentResult := fmt.Sprintf(`{"type":"user","timestamp":%q,"toolUseResult":{"status":"async_launched","agentId":"a77"},"message":{"content":[{"type":"tool_result","tool_use_id":"tu_a","content":"launched"}]}}`, ts(2.1))
	runResult := fmt.Sprintf(`{"type":"user","timestamp":%q,"toolUseResult":{"status":"async_launched","runId":"wf_1234abcd-001"},"message":{"content":[{"type":"tool_result","tool_use_id":"tu_w","content":"launched"}]}}`, ts(2.2))
	lines := []string{
		asst(0, "m0", "claude-opus-5-5", 1, 1, 0, 0, ""), // before the first prompt: turn 0
		userText(1, "first **real** prompt", `,"origin":{"kind":"human"},"promptSource":"typed"`),
		agentCall,
		agentResult,
		runResult,
		`{"type":"system","subtype":"turn_duration","durationMs":4200,"timestamp":"` + ts(3) + `"}`,
		userText(4, "Another Claude session sent a message:\n<teammate-message>hi</teammate-message>", ""),
		userText(5, "<local-command-stdout>ok</local-command-stdout>", ""),
		userText(6, "This session is being continued from a previous conversation", `,"isCompactSummary":true`),
		userText(7, "<task-notification>done</task-notification>", `,"origin":{"kind":"task-notification"},"promptSource":"system"`),
		userText(8, "pick this one", `,"origin":{"kind":"human"},"promptSource":"suggestion_accepted"`),
		asst(9, "m3", "claude-opus-5-5", 1, 1, 0, 0, ""),
	}
	input := strings.Join(lines, "\n") + "\n"
	res, err := Parse(strings.NewReader(input), Main, State{Base: 1000})
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, tr := range res.Turns {
		kinds = append(kinds, fmt.Sprintf("%d:%s", tr.Seq, tr.Kind))
	}
	if got := strings.Join(kinds, " "); got != "1:human 2:peer 3:task 4:human" {
		t.Fatalf("turns = %s", got)
	}
	if res.TypedPrompts != 2 || res.OtherPrompts != 2 {
		t.Fatalf("typed=%d other=%d", res.TypedPrompts, res.OtherPrompts)
	}
	// Offsets are file offsets: Base plus the bytes before the line.
	if want := int64(1000 + len(lines[0]) + 1); res.Turns[0].Offset != want {
		t.Fatalf("offset = %d, want %d", res.Turns[0].Offset, want)
	}
	if !strings.HasPrefix(input[res.Turns[3].Offset-1000:], `{"type":"user"`) {
		t.Fatalf("offset of turn 4 does not start its line")
	}
	if res.Turns[0].Text != "first **real** prompt" || res.Turns[0].Chars != 21 {
		t.Fatalf("turn 1 = %+v", res.Turns[0])
	}
	if r := res.Responses; r[0].Turn != 0 || r[1].Turn != 1 || r[2].Turn != 4 {
		t.Fatalf("response turns = %d %d %d", r[0].Turn, r[1].Turn, r[2].Turn)
	}
	s := res.TurnStats[1]
	if s == nil || s.ToolCalls != 3 || s.Agents != 2 || s.DurationMS != 4200 || s.LastTS != parseTS(ts(2.2)) {
		t.Fatalf("turn 1 stats = %+v", s)
	}
	if len(res.Spawns) != 2 || res.Spawns[0] != (Spawn{ToolUseID: "tu_a", Tool: "Agent", Turn: 1}) || res.Spawns[1].Tool != "Workflow" {
		t.Fatalf("spawns = %+v", res.Spawns)
	}
	if res.Targets["tu_a"] != "a77" || res.Targets["tu_w"] != "wf_1234abcd-001" {
		t.Fatalf("targets = %v", res.Targets)
	}
	if res.State.Turn != 4 || res.State.Base != 0 {
		t.Fatalf("state = %+v", res.State)
	}
}

func TestTurnNumberingContinuesAcrossParses(t *testing.T) {
	a := userText(0, "one", "") + "\n" + asst(1, "m1", "claude-opus-5-5", 1, 1, 0, 0, "") + "\n"
	b := userText(2, "two", "") + "\n" + asst(3, "m2", "claude-opus-5-5", 1, 1, 0, 0, "") + "\n"
	first, _ := Parse(strings.NewReader(a), Main, State{})
	st := first.State
	st.Base = int64(len(a))
	second, _ := Parse(strings.NewReader(b), Main, st)
	if len(second.Turns) != 1 || second.Turns[0].Seq != 2 || second.Turns[0].Offset != int64(len(a)) {
		t.Fatalf("second parse turns = %+v", second.Turns)
	}
	if second.Responses[0].Turn != 2 {
		t.Fatalf("response turn = %d", second.Responses[0].Turn)
	}
}

func TestImportedRanges(t *testing.T) {
	l1 := asst(0, "m1", "claude-opus-5-5", 1, 1, 0, 0, "") + "\n"
	l2 := `{"type":"cost-state","startTime":5,"totalCostUSD":1,"modelUsage":{"claude-opus-5-5":{"inputTokens":1,"outputTokens":1,"costUSD":1}}}` + "\n"
	l3 := asst(1, "m2", "claude-opus-5-5", 1, 1, 0, 0, "") + "\n"
	base := int64(500)
	st := State{Base: base, Imported: []Range{{From: 0, To: base + int64(len(l1)+len(l2))}}}
	res, _ := Parse(strings.NewReader(l1+l2+l3), Main, st)
	if !res.Responses[0].Imported || res.Responses[1].Imported {
		t.Fatalf("imported = %v %v", res.Responses[0].Imported, res.Responses[1].Imported)
	}
	if len(res.Processes) != 1 || !res.Processes[0].Imported {
		t.Fatalf("processes = %+v", res.Processes)
	}
	if res.State.Imported != nil {
		t.Fatalf("imported ranges carried over")
	}
}

func TestSubagentFirstPromptAndAttribution(t *testing.T) {
	res := parse(t, Subagent,
		userText(0, "Review   the diff\nin full", `,"isSidechain":true,"agentId":"a1"`),
		`{"type":"assistant","isSidechain":true,"attributionAgent":"code-reviewer","timestamp":"`+ts(1)+`","message":{"id":"s1","model":"m","usage":{"input_tokens":1}}}`,
	)
	if res.FirstPrompt != "Review the diff in full" || res.Attribution != "code-reviewer" || len(res.Turns) != 0 {
		t.Fatalf("subagent: prompt=%q attribution=%q turns=%d", res.FirstPrompt, res.Attribution, len(res.Turns))
	}
}

func TestTeammateSpawnTarget(t *testing.T) {
	call := fmt.Sprintf(`{"type":"assistant","timestamp":%q,"message":{"id":"m1","model":"m","content":[{"type":"tool_use","id":"tu_t","name":"Agent","input":{}}]}}`, ts(1))
	result := fmt.Sprintf(`{"type":"user","timestamp":%q,"toolUseResult":{"status":"teammate_spawned","teammate_id":"researcher@session-1234abcd","agent_id":"researcher@session-1234abcd","name":"researcher"},"message":{"content":[{"type":"tool_result","tool_use_id":"tu_t","content":"ok"}]}}`, ts(2))
	res := parse(t, Main, userText(0, "go", ""), call, result)
	if res.Targets["tu_t"] != "teammate:researcher" {
		t.Fatalf("targets = %v", res.Targets)
	}
}

func TestReplayedStretchCountsOnce(t *testing.T) {
	call := func(at float64, msg, tool string) string {
		return fmt.Sprintf(`{"type":"assistant","timestamp":%q,"message":{"id":%q,"model":"claude-opus-5-5","usage":{"input_tokens":1,"output_tokens":1},"content":[{"type":"tool_use","id":%q,"name":"Bash","input":{}}]}}`, ts(at), msg, tool)
	}
	lines := []string{
		userText(0, "first", `,"promptId":"p1"`),
		call(1, "m1", "tu1"),
		userText(2, "second", `,"promptId":"p2"`),
		call(3, "m2", "tu2"),
		`{"type":"custom-title","customTitle":"t","timestamp":"` + ts(900) + `"}`,
		// Resumed later: Claude Code writes the first turn again.
		userText(0, "first", `,"promptId":"p1"`),
		call(1, "m1", "tu1"),
		userText(5, "third", `,"promptId":"p3"`),
		userText(6, "[Request interrupted by user]", `,"promptId":"p4"`),
	}
	res := parse(t, Main, lines...)
	if len(res.Turns) != 3 || res.Turns[2].Text != "third" || res.TypedPrompts != 3 {
		t.Fatalf("turns = %+v typed=%d", res.Turns, res.TypedPrompts)
	}
	if s := res.TurnStats[2]; s.ToolCalls != 1 || s.LastTS != parseTS(ts(3)) {
		t.Fatalf("turn 2 stats = %+v (the replay or the title line counted)", s)
	}
	if len(res.Responses) != 2 || res.Responses[0].Turn != 1 {
		t.Fatalf("responses = %+v", res.Responses)
	}
	// The replay resumes across parses with the prompt ids the caller loads.
	st := State{Turn: 2, Prompts: map[string]bool{"p1": true, "p2": true}}
	again, _ := Parse(strings.NewReader(strings.Join(lines[5:7], "\n")+"\n"), Main, st)
	if len(again.Turns) != 0 || !again.State.Replay || !again.Responses[0].Replay {
		t.Fatalf("incremental replay: turns=%d state=%+v", len(again.Turns), again.State)
	}
}

func TestCommandName(t *testing.T) {
	if name, ok := CommandName("/copy the rest"); !ok || name != "copy" {
		t.Fatalf("copy: %q %v", name, ok)
	}
	if _, ok := CommandName("/Users/cdcd/proj\n\nmore"); ok {
		t.Fatal("a path was taken as a command")
	}
	if _, ok := CommandName("just text"); ok {
		t.Fatal("plain text was a command")
	}
}
