package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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

// With the session in two accounts, read commands require a chosen copy.
// Once chosen, log marks the turns that came in with the copy.
func TestReadCommandsPickACopy(t *testing.T) {
	twoAccounts(t)
	runCLI(t, "copy", roundTrip, "--to", "work", "--apply")
	for _, cmd := range [][]string{
		{"show", roundTrip}, {"show", roundTrip, "--json"},
		{"log", roundTrip}, {"log", roundTrip, "last"}, {"log", roundTrip, "--detail", "reply"},
		{"export", roundTrip},
	} {
		var out, errw bytes.Buffer
		if code := Main(cmd, &out, &errw); code == 0 ||
			!strings.Contains(errw.String(), "-a <account>") ||
			!strings.Contains(errw.String(), "default:"+roundTrip[:8]) ||
			!strings.Contains(errw.String(), "work:"+roundTrip[:8]) {
			t.Fatalf("%v: exit %d, stdout %q, stderr %q", cmd, code, out.String(), errw.String())
		}
		if slices.Contains(cmd, "--json") {
			var page struct {
				Sessions []struct {
					Account string   `json:"account"`
					ID      string   `json:"id"`
					Title   string   `json:"title"`
					Models  []string `json:"models"`
				} `json:"sessions"`
			}
			if err := json.Unmarshal(out.Bytes(), &page); err != nil || len(page.Sessions) != 2 {
				t.Fatalf("%v: invalid candidates: %v %s", cmd, err, out.String())
			}
			accounts := []string{page.Sessions[0].Account, page.Sessions[1].Account}
			slices.Sort(accounts)
			if !slices.Equal(accounts, []string{"default", "work"}) {
				t.Fatalf("%v: wrong accounts in candidates: %s", cmd, out.String())
			}
			for _, row := range page.Sessions {
				if row.ID != roundTrip || row.Title == "" {
					t.Fatalf("%v: incomplete candidate: %+v", cmd, row)
				}
			}
			continue
		}
		for _, want := range []string{"LAST", "ACCOUNT", "ID", "PROJECT", "TITLE", "MODEL", "ACTIVE", "TOKENS", "CACHED", "COST", "default", "work", "first", "opus-5-5"} {
			if !strings.Contains(out.String(), want) {
				t.Fatalf("%v: missing %q in candidates:\n%s", cmd, want, out.String())
			}
		}
		if strings.Count(out.String(), roundTrip[:8]) != 2 || !strings.Contains(out.String(), "2 sessions") {
			t.Fatalf("%v: candidate list lost a copy:\n%s", cmd, out.String())
		}
	}
	if show := string(runCLI(t, "show", roundTrip, "-a", "work")); !strings.Contains(show, "ccli log work:"+roundTrip[:8]) ||
		!strings.Contains(show, "ccli view work:"+roundTrip[:8]) ||
		!strings.Contains(show, "ccli resume work:"+roundTrip[:8]) {
		t.Fatalf("show -a work has ambiguous follow-up commands:\n%s", show)
	}
	work := string(runCLI(t, "log", roundTrip, "-a", "work"))
	if !strings.Contains(work, "· work ·") || !strings.Contains(work, "turns 1 to 2 came with a copy from default") || !strings.Contains(work, "copied") ||
		!strings.Contains(work, "ccli view work:"+roundTrip[:8]) {
		t.Fatalf("log -a work:\n%s", work)
	}
	if exported := string(runCLI(t, "export", "work:"+roundTrip)); !strings.Contains(exported, "first") {
		t.Fatalf("export work:id:\n%s", exported)
	}
	if def := string(runCLI(t, "log", "default:"+roundTrip)); !strings.Contains(def, "· default ·") || strings.Contains(def, "came with a copy") {
		t.Fatalf("log default:id:\n%s", def)
	}
	var out, errw bytes.Buffer
	if code := Main([]string{"log", "default:" + roundTrip, "-a", "work"}, &out, &errw); code == 0 || !strings.Contains(errw.String(), "different accounts") {
		t.Fatalf("conflicting accounts: exit %d\n%s", code, errw.String())
	}
	if code := Main([]string{"show", roundTrip, "-a", "nosuch"}, &out, &errw); code == 0 {
		t.Fatal("an unknown account was accepted")
	}
}
