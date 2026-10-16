package index

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/lr00rl/ccli/internal/account"
	"github.com/lr00rl/ccli/internal/store"
)

const (
	userThread  = "01a00000-0000-7000-b000-00000000a001"
	agentThread = "01a00000-0000-7000-b000-00000000a002"
	oldThread   = "019c0000-0000-7000-b000-00000000a003"
)

func codexMeta(id, source string) string {
	return fmt.Sprintf(`{"timestamp":"2026-10-01T09:00:00.000Z","type":"session_meta","payload":{"id":%q,"cwd":"/work/proj","cli_version":"0.150.1","source":%s}}`+"\n", id, source)
}

func TestScanCodex(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".codex")
	write := func(rel, content string) {
		p := filepath.Join(home, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("sessions/2026/10/01/rollout-2026-10-01T09-00-00-"+userThread+".jsonl", codexMeta(userThread, `"cli"`))
	write("sessions/2026/10/01/rollout-2026-10-01T09-01-00-"+agentThread+".jsonl",
		codexMeta(agentThread, `{"subagent":{"thread_spawn":{"parent_thread_id":"`+userThread+`","depth":1,"agent_role":"explorer"}}}`))
	write("archived_sessions/rollout-2026-09-01T10-00-00-"+oldThread+".jsonl", codexMeta(oldThread, `"exec"`))
	write("sessions/2026/10/01/rollout-2026-10-01T09-02-00-01a00000-0000-7000-b000-00000000a004.jsonl", "") // still being written
	write("sessions/2026/10/01/notes.txt", "{}\n")
	write("history.jsonl", "{}\n")
	acct := account.Account{Name: "codex", Tool: account.Codex, Dir: home}
	got, err := scanCodex(acct, nil)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]store.File{}
	for _, d := range got {
		if d.tool != account.Codex || d.file.Account != "codex" {
			t.Fatalf("discovered %+v", d)
		}
		files[d.file.Path] = d.file
	}
	if len(got) != 3 {
		t.Fatalf("found %d files, want 3: %v", len(got), files)
	}
	for _, f := range files {
		switch {
		case f.AgentID == agentThread:
			if f.Kind != "subagent" || f.SessionID != userThread {
				t.Fatalf("the agent is %+v, want a subagent of the user's thread", f)
			}
		case f.Kind != "main" || (f.SessionID != userThread && f.SessionID != oldThread):
			t.Fatalf("unexpected %+v", f)
		}
	}
	// Files already indexed keep what the index knows, without being read.
	known := map[string]store.File{}
	for p, f := range files {
		f.SessionID = "kept"
		known[p] = f
		os.WriteFile(p, []byte("not a rollout\n"), 0o644)
	}
	again, err := scanCodex(acct, known)
	if err != nil || len(again) != 3 {
		t.Fatalf("rescan found %d: %v", len(again), err)
	}
	for _, d := range again {
		if d.file.SessionID != "kept" {
			t.Fatalf("rescan reread %+v", d.file)
		}
	}
}
