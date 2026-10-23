package index

import (
	"os"
	"testing"
)

// Claude Code moves a session's transcript between project directories
// when the session enters or leaves a worktree. The row follows the file,
// and what was read from it stays.
func TestMovedTranscriptIsFollowed(t *testing.T) {
	e := newEnv(t, "default")
	e.write(0, "-work-proj/s1.jsonl", prompt(0, "first")+asst(1, "m1", 10))
	e.run()
	from, to := e.path(0, "-work-proj/s1.jsonl"), e.path(0, "-work-proj--claude-worktrees-x/s1.jsonl")
	if err := os.MkdirAll(e.path(0, "-work-proj--claude-worktrees-x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(from, to); err != nil {
		t.Fatal(err)
	}
	e.appendTo(0, "-work-proj--claude-worktrees-x/s1.jsonl", prompt(2, "second")+asst(3, "m2", 10))
	if s := e.run(); s.Missing != 0 {
		t.Fatalf("a moved transcript was counted as gone: %+v", s)
	}
	if n := e.int("SELECT COUNT(*) FROM files WHERE session_id = 's1'"); n != 1 {
		t.Fatalf("%d rows for one transcript", n)
	}
	if n := e.int("SELECT COUNT(*) FROM files WHERE path = ? AND missing = 0 AND project_key = '-work-proj--claude-worktrees-x'", to); n != 1 {
		t.Fatal("the row did not follow the file")
	}
	if n := e.int("SELECT typed_prompts FROM files WHERE session_id = 's1'"); n != 2 {
		t.Fatalf("typed prompts = %d, want 2", n)
	}
	if n := e.int("SELECT COUNT(*) FROM responses WHERE session_id = 's1'"); n != 2 {
		t.Fatalf("responses = %d, want 2", n)
	}
}

// An index that already has rows for both paths keeps the one whose file
// is there. The vanished row is an older copy of the same transcript: it is
// dropped, and the file is read again so its work is counted once.
func TestStaleCopyOfMovedTranscriptIsDropped(t *testing.T) {
	e := newEnv(t, "default")
	head := prompt(0, "first") + asst(1, "m1", 10)
	e.write(0, "-work-proj/s1.jsonl", head)
	e.write(0, "-work-proj--claude-worktrees-x/s1.jsonl", head)
	e.run()
	os.Remove(e.path(0, "-work-proj--claude-worktrees-x/s1.jsonl"))
	e.appendTo(0, "-work-proj/s1.jsonl", prompt(2, "second")+asst(3, "m2", 10))
	e.run()
	if n := e.int("SELECT COUNT(*) FROM files WHERE session_id = 's1'"); n != 1 {
		t.Fatalf("%d rows for one transcript", n)
	}
	if n := e.int("SELECT SUM(typed_prompts) FROM files WHERE session_id = 's1'"); n != 2 {
		t.Fatalf("typed prompts = %d, want 2", n)
	}
	if n := e.int("SELECT COUNT(*) FROM responses r JOIN files f ON f.id = r.file_id WHERE f.path = ?", e.path(0, "-work-proj/s1.jsonl")); n != 2 {
		t.Fatalf("the file kept owns %d responses, want 2", n)
	}
}
