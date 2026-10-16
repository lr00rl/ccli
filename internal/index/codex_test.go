package index

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	got, _, err := scanCodex(acct, nil)
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
	again, moves, err := scanCodex(acct, known)
	if err != nil || len(again) != 3 || len(moves) != 0 {
		t.Fatalf("rescan found %d, %d moved: %v", len(again), len(moves), err)
	}
	for _, d := range again {
		if d.file.SessionID != "kept" {
			t.Fatalf("rescan reread %+v", d.file)
		}
	}
}

func codexLine(sec int, typ, payload string) string {
	return fmt.Sprintf(`{"timestamp":"2026-10-01T09:00:%02d.000Z","type":%q,"payload":%s}`+"\n", sec, typ, payload)
}

// codexTurn is one turn of a rollout: a prompt, one count, and the end.
func codexTurn(sec int, prompt string, total int) string {
	tok := fmt.Sprintf(`{"input_tokens":%d,"cached_input_tokens":0,"output_tokens":10,"reasoning_output_tokens":0,"total_tokens":%d}`, total, total+10)
	return codexLine(sec, "event_msg", `{"type":"task_started","turn_id":"t"}`) +
		codexLine(sec, "turn_context", `{"model":"gpt-5.5","cwd":"/work/proj"}`) +
		codexLine(sec, "event_msg", fmt.Sprintf(`{"type":"user_message","message":%q}`, prompt)) +
		codexLine(sec+2, "event_msg", fmt.Sprintf(`{"type":"token_count","info":{"total_token_usage":%s,"last_token_usage":%s}}`, tok, tok)) +
		codexLine(sec+3, "event_msg", `{"type":"task_complete","duration_ms":3000}`)
}

// Codex archives a thread by moving its rollout, and unarchives it by moving
// it back. The file keeps its row and what was read of it, so nothing is
// counted twice, and what is added later is read from where reading stopped.
func TestCodexArchiveKeepsItsRow(t *testing.T) {
	e := newEnv(t)
	home := filepath.Join(t.TempDir(), ".codex")
	e.accts = append(e.accts, account.Account{Name: "codex", Tool: account.Codex, Dir: home})
	name := "rollout-2026-10-01T09-00-00-" + userThread + ".jsonl"
	live := filepath.Join(home, "sessions", "2026", "10", "01", name)
	archived := filepath.Join(home, "archived_sessions", name)
	os.MkdirAll(filepath.Dir(live), 0o755)
	os.MkdirAll(filepath.Dir(archived), 0o755)
	if err := os.WriteFile(live, []byte(codexMeta(userThread, `"cli"`)+codexTurn(1, "fix the build", 100)), 0o644); err != nil {
		t.Fatal(err)
	}
	e.run()
	counts := func() string {
		return fmt.Sprint(e.int("SELECT COUNT(*) FROM files"), e.int("SELECT COUNT(*) FROM files WHERE missing = 1"),
			e.int("SELECT SUM(typed_prompts) FROM files"), e.int("SELECT SUM(active_ms) FROM files"),
			e.int("SELECT COUNT(*) FROM turns"), e.int("SELECT COUNT(*) FROM responses"))
	}
	before := counts()
	if !strings.HasPrefix(before, "1 0 1 ") {
		t.Fatalf("first run: files, missing, prompts, active, turns, responses = %s", before)
	}
	if err := os.Rename(live, archived); err != nil {
		t.Fatal(err)
	}
	e.run()
	if got := counts(); got != before {
		t.Fatalf("after archiving: %s, want %s", got, before)
	}
	var path string
	e.st.DB().QueryRow("SELECT path FROM files").Scan(&path)
	if path != archived {
		t.Fatalf("the row names %s, want %s", path, archived)
	}

	// Unarchived, then used again.
	if err := os.Rename(archived, live); err != nil {
		t.Fatal(err)
	}
	f, _ := os.OpenFile(live, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(codexTurn(20, "and the tests", 150))
	f.Close()
	e.run()
	if got := counts(); !strings.HasPrefix(got, "1 0 2 ") || e.int("SELECT COUNT(*) FROM responses") != 2 {
		t.Fatalf("after unarchiving and a new turn: %s", got)
	}
}

// A count stored before any line named a model gets the first one named, and
// a price, when a later run reads that line.
func TestCodexModelFilledLater(t *testing.T) {
	e := newEnv(t)
	home := filepath.Join(t.TempDir(), ".codex")
	e.accts = append(e.accts, account.Account{Name: "codex", Tool: account.Codex, Dir: home})
	p := filepath.Join(home, "sessions", "2026", "10", "01", "rollout-2026-10-01T09-00-00-"+userThread+".jsonl")
	os.MkdirAll(filepath.Dir(p), 0o755)
	tok := `{"input_tokens":100,"cached_input_tokens":0,"output_tokens":10,"reasoning_output_tokens":0,"total_tokens":110}`
	head := codexMeta(userThread, `"cli"`) +
		codexLine(1, "event_msg", `{"type":"user_message","message":"go"}`) +
		codexLine(2, "event_msg", `{"type":"token_count","info":{"total_token_usage":`+tok+`,"last_token_usage":`+tok+`}}`)
	if err := os.WriteFile(p, []byte(head), 0o644); err != nil {
		t.Fatal(err)
	}
	e.run()
	if n := e.int("SELECT COUNT(*) FROM responses WHERE model = '' AND cost IS NULL"); n != 1 {
		t.Fatalf("%d responses with no model before it is named, want 1", n)
	}
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(codexLine(3, "turn_context", `{"model":"gpt-5.5","cwd":"/work/proj"}`))
	f.Close()
	e.run()
	if n := e.int("SELECT COUNT(*) FROM responses WHERE model = 'gpt-5.5' AND cost > 0"); n != 1 {
		t.Fatalf("%d responses filled and priced, want 1", n)
	}
}
