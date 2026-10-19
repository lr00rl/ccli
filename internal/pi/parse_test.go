package pi

import (
	"strings"
	"testing"

	"github.com/lr00rl/ccli/internal/transcript"
)

func TestActiveBranchKeepsEveryReply(t *testing.T) {
	// u1 -> a1 -> u2 is abandoned. The current leaf is a3, hanging off u1.
	const body = `{"type":"session","version":3,"id":"s1","timestamp":"2026-08-01T00:00:00Z","cwd":"/work/app"}
{"type":"message","id":"u1","parentId":null,"timestamp":"2026-08-01T00:00:01Z","message":{"role":"user","content":"first","timestamp":1000}}
{"type":"message","id":"a1","parentId":"u1","timestamp":"2026-08-01T00:00:02Z","message":{"role":"assistant","content":[{"type":"text","text":"A"}],"provider":"openai-codex","model":"gpt-5.4","usage":{"input":10,"output":5,"cacheRead":1,"cacheWrite":2,"totalTokens":18,"cost":{"total":0.10}},"timestamp":2000}}
{"type":"message","id":"u2","parentId":"a1","timestamp":"2026-08-01T00:00:03Z","message":{"role":"user","content":"abandoned","timestamp":3000}}
{"type":"message","id":"a2","parentId":"u2","timestamp":"2026-08-01T00:00:04Z","message":{"role":"assistant","content":[{"type":"text","text":"B"}],"model":"gpt-5.4","usage":{"input":3,"output":1,"cacheRead":0,"cacheWrite":0,"totalTokens":4,"cost":{"total":0.05}},"timestamp":4000}}
{"type":"message","id":"u3","parentId":"u1","timestamp":"2026-08-01T00:00:05Z","message":{"role":"user","content":"branch","timestamp":5000}}
{"type":"message","id":"a3","parentId":"u3","timestamp":"2026-08-01T00:00:06Z","message":{"role":"assistant","content":[{"type":"toolCall","id":"c1","name":"bash","arguments":{"command":"ls"}}],"model":"gpt-5.4","usage":{"input":7,"output":4,"cacheRead":0,"cacheWrite":0,"totalTokens":11,"cost":{"total":0.15}},"timestamp":6000}}
{"type":"message","id":"t1","parentId":"a3","timestamp":"2026-08-01T00:00:07Z","message":{"role":"toolResult","toolCallId":"c1","toolName":"bash","content":[{"type":"text","text":"ok"}],"isError":false,"timestamp":7000}}
{"type":"session_info","id":"n1","parentId":"t1","timestamp":"2026-08-01T00:00:08Z","name":"named"}
`
	res, err := Parse(strings.NewReader(body), "", transcript.State{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Cwd != "/work/app" || res.CustomTitle != "named" {
		t.Fatalf("header: cwd %q title %q", res.Cwd, res.CustomTitle)
	}
	if res.TypedPrompts != 2 || res.FirstPrompt != "first" {
		t.Fatalf("prompts %d %q, want the active branch only", res.TypedPrompts, res.FirstPrompt)
	}
	if len(res.Turns) != 2 || res.Turns[1].Text != "branch" {
		t.Fatalf("turns %+v", res.Turns)
	}
	var cost float64
	var in, out int64
	for _, r := range res.Responses {
		if !r.HasCost {
			t.Fatalf("reply %s has no embedded cost", r.Key)
		}
		cost += r.Cost
		in += r.Usage.Input
		out += r.Usage.Output
	}
	if len(res.Responses) != 3 || cost < 0.29 || cost > 0.31 || in != 20 || out != 10 {
		t.Fatalf("responses %d cost %v in %d out %d", len(res.Responses), cost, in, out)
	}
	if res.ToolCalls != 1 || res.Tools["bash"] != 1 {
		t.Fatalf("tools %+v calls %d", res.Tools, res.ToolCalls)
	}
}
