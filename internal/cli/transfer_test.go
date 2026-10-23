package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const roundTrip = "bbbbbbbb-0000-4000-8000-000000000001"

// twoAccounts is a home with the Claude Code accounts default and work, and
// a two-turn session in default.
func twoAccounts(t *testing.T) (root string, turn1, turn2 string) {
	t.Helper()
	root = cleanHome(t)
	if err := os.MkdirAll(filepath.Join(root, ".claude-work", "projects"), 0o700); err != nil {
		t.Fatal(err)
	}
	ts := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	line := func(kind, id, text string) string {
		if kind == "user" {
			return fmt.Sprintf(`{"type":"user","timestamp":%q,"sessionId":%q,"cwd":"/work/app","message":{"content":%q}}`+"\n", ts, roundTrip, text)
		}
		return fmt.Sprintf(`{"type":"assistant","timestamp":%q,"sessionId":%q,"cwd":"/work/app","message":{"id":%q,"model":"claude-opus-5-5","usage":{"input_tokens":10,"output_tokens":5}}}`+"\n", ts, roundTrip, id)
	}
	turn1 = line("user", "", "first") + line("assistant", "r1", "")
	turn2 = line("user", "", "second") + line("assistant", "r2", "")
	writeTranscript(t, root, ".claude", roundTrip, turn1+turn2)
	return root, turn1, turn2
}

func writeTranscript(t *testing.T, root, acct, id, body string) {
	t.Helper()
	p := filepath.Join(root, acct, "projects", "-work-app", id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Going back to an account brings its older copy up to date without being
// asked; --no-sync refuses instead.
func TestResumeSyncsByDefault(t *testing.T) {
	root, turn1, turn2 := twoAccounts(t)
	runCLI(t, "copy", roundTrip, "--to", "work", "--apply")
	// The session goes on in work.
	more := strings.ReplaceAll(turn2, "second", "third")
	more = strings.ReplaceAll(more, `"r2"`, `"r3"`)
	f, err := os.OpenFile(filepath.Join(root, ".claude-work", "projects", "-work-app", roundTrip+".jsonl"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(more)
	f.Close()

	var out, errw bytes.Buffer
	if code := Main([]string{"resume", roundTrip, "-a", "default", "--no-sync", "--print"}, &out, &errw); code == 0 || !strings.Contains(errw.String(), "older copy") {
		t.Fatalf("--no-sync: exit %d\n%s", code, errw.String())
	}
	cmd := runCLI(t, "resume", roundTrip, "-a", "default", "--print")
	if !strings.Contains(string(cmd), "claude --resume "+roundTrip) {
		t.Fatalf("resume printed %q", cmd)
	}
	got, _ := os.ReadFile(filepath.Join(root, ".claude", "projects", "-work-app", roundTrip+".jsonl"))
	if string(got) != turn1+turn2+more {
		t.Fatalf("default's copy was not brought up to date:\n%s", got)
	}
}

// A fork with --turn keeps the conversation through that turn, under a new
// id in the account -a names.
func TestForkAtTurn(t *testing.T) {
	root, turn1, _ := twoAccounts(t)
	var out, errw bytes.Buffer
	if code := Main([]string{"fork", roundTrip, "--turn", "1"}, &out, &errw); code == 0 || !strings.Contains(errw.String(), "-a <account>") {
		t.Fatalf("fork without -a: exit %d\n%s", code, errw.String())
	}
	if code := Main([]string{"fork", roundTrip, "--turn", "7", "-a", "work"}, &out, &errw); code == 0 || !strings.Contains(errw.String(), "has no turn 7") {
		t.Fatalf("fork at a missing turn: exit %d\n%s", code, errw.String())
	}
	if code := Main([]string{"fork", roundTrip, "--turn", "0", "-a", "work"}, &out, &errw); code == 0 || !strings.Contains(errw.String(), "counts from 1") {
		t.Fatalf("fork at turn 0: exit %d\n%s", code, errw.String())
	}
	var res struct {
		Plan struct {
			NewID string `json:"new_session_id"`
			Turn  int    `json:"turn"`
			Turns int    `json:"turns"`
		} `json:"plan"`
	}
	if err := json.Unmarshal(runCLI(t, "fork", roundTrip, "--turn", "1", "-a", "work", "--apply", "--json"), &res); err != nil {
		t.Fatal(err)
	}
	if res.Plan.Turn != 1 || res.Plan.Turns != 2 {
		t.Fatalf("plan = %+v", res.Plan)
	}
	got, err := os.ReadFile(filepath.Join(root, ".claude-work", "projects", "-work-app", res.Plan.NewID+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.ReplaceAll(turn1, roundTrip, res.Plan.NewID); string(got) != want {
		t.Fatalf("forked transcript:\n%s\nwant:\n%s", got, want)
	}
}

// A fork through the last turn keeps the whole conversation the index has
// read, and says so.
func TestForkThroughLastTurn(t *testing.T) {
	root, turn1, turn2 := twoAccounts(t)
	plan := string(runCLI(t, "fork", roundTrip, "--turn", "2", "-a", "work"))
	if !strings.Contains(plan, "keeps the conversation through turn 2, the last one") || strings.Contains(plan, "only the conversation goes back") {
		t.Fatalf("plan:\n%s", plan)
	}
	var res struct {
		Plan struct {
			NewID string `json:"new_session_id"`
		} `json:"plan"`
	}
	if err := json.Unmarshal(runCLI(t, "fork", roundTrip, "--turn", "2", "-a", "work", "--apply", "--json"), &res); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(root, ".claude-work", "projects", "-work-app", res.Plan.NewID+".jsonl"))
	if want := strings.ReplaceAll(turn1+turn2, roundTrip, res.Plan.NewID); string(got) != want {
		t.Fatalf("forked transcript:\n%s", got)
	}
}

func TestResumeRejectsBothSyncFlags(t *testing.T) {
	twoAccounts(t)
	var out, errw bytes.Buffer
	if code := Main([]string{"resume", roundTrip, "-a", "work", "--sync", "--no-sync", "--print"}, &out, &errw); code == 0 || !strings.Contains(errw.String(), "not both") {
		t.Fatalf("exit %d\n%s", code, errw.String())
	}
}
