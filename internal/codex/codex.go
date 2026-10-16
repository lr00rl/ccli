// Package codex reads OpenAI Codex CLI session logs ("rollouts"): one JSON
// object per line, under sessions/YYYY/MM/DD/rollout-<time>-<id>.jsonl in a
// Codex home. It turns them into the same records ccli keeps for Claude Code
// transcripts. It only ever reads.
package codex

import (
	"regexp"
	"strings"
)

var rolloutName = regexp.MustCompile(`^rollout-.+-([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\.jsonl$`)

// SessionID returns the session (thread) id a rollout file is named after.
func SessionID(name string) (string, bool) {
	m := rolloutName.FindStringSubmatch(strings.ToLower(name))
	if m == nil {
		return "", false
	}
	return m[1], true
}
