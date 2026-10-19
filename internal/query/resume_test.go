package query

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/lr00rl/ccli/internal/account"
)

func TestTitleOneLine(t *testing.T) {
	got, src := title("", "", "", "/Users/a/proj\n\nsecond line of the prompt")
	if src != "prompt" || strings.Contains(got, "\n") || !strings.Contains(got, "second line") {
		t.Fatalf("title %q from %s", got, src)
	}
}

// A Codex session resumes with codex resume, with CODEX_HOME naming any home
// but ~/.codex. A thread Codex archived must be unarchived first; ccli names
// the command and does not run it.
func TestCodexResume(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	main, work := filepath.Join(home, ".codex"), filepath.Join(home, ".codex-work")
	d := &DB{accts: []account.Account{
		{Name: "codex", Tool: account.Codex, Dir: main},
		{Name: "codex-work", Tool: account.Codex, Dir: work},
	}}
	const id = "01a00000-0000-7000-b000-00000000a001"
	det := func(acct, dir, sub string) *SessionDetail {
		s := &SessionDetail{Files: []FileInfo{{Kind: "main", Path: filepath.Join(dir, sub, "rollout-2026-10-01T09-00-00-"+id+".jsonl")}}}
		s.ID, s.Account, s.Cwd = id, acct, "/work/proj"
		return s
	}

	r := d.resume(det("codex", main, "sessions/2026/10/01"))
	if !r.Available || r.Command != "cd /work/proj && codex resume "+id || r.EnvValue != "" {
		t.Errorf("default home: %+v", r)
	}
	r = d.resume(det("codex-work", work, "sessions/2026/10/01"))
	if !r.Available || r.Command != "cd /work/proj && CODEX_HOME="+work+" codex resume "+id {
		t.Errorf("second home: %+v", r)
	}
	r = d.resume(det("codex-work", work, "archived_sessions"))
	if r.Available || !strings.HasSuffix(r.Reason, "CODEX_HOME="+work+" codex unarchive "+id) {
		t.Errorf("archived: %+v", r)
	}
}
