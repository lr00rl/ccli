package codex

import "testing"

func TestSessionID(t *testing.T) {
	for name, want := range map[string]string{
		"rollout-2026-10-01T09-00-00-01a00000-0000-7000-b000-00000000a001.jsonl": "01a00000-0000-7000-b000-00000000a001",
		"rollout-2025-01-02T03-04-05-ABCDEF01-2345-6789-abcd-ef0123456789.jsonl": "abcdef01-2345-6789-abcd-ef0123456789",
		"rollout-2026-10-01T09-00-00-01a00000.jsonl":                             "",
		"history.jsonl": "",
		"rollout-2026-10-01T09-00-00-01a00000-0000-7000-b000-00000000a001.json": "",
	} {
		got, ok := SessionID(name)
		if got != want || ok != (want != "") {
			t.Errorf("SessionID(%q) = %q, %v; want %q", name, got, ok, want)
		}
	}
}
