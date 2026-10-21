package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lr00rl/ccli/internal/query"
)

const (
	cleanEmpty  = "aaaaaaaa-0000-4000-8000-000000000001"
	cleanAsked  = "aaaaaaaa-0000-4000-8000-000000000002"
	cleanUsed   = "aaaaaaaa-0000-4000-8000-000000000003"
	cleanRecent = "aaaaaaaa-0000-4000-8000-000000000004"
	cleanNamed  = "aaaaaaaa-0000-4000-8000-000000000005"
)

// cleanHome is a home with one Claude Code account holding an empty
// session, one whose prompt got no reply, one in use, one empty session
// written a minute ago, and one with nothing in it but a name.
func cleanHome(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	for _, v := range []string{"CCLI_CONFIG_DIRS", "CODEX_HOME", "CCLI_CODEX_DIRS", "CCLI_PI_DIRS", "PI_CODING_AGENT_DIR",
		"CCLI_CURSOR_DIRS", "CURSOR_CONFIG_DIR", "CCLI_CURSOR_IDE_DIRS", "APPDATA"} {
		t.Setenv(v, "")
	}
	old := time.Now().Add(-3 * time.Hour)
	ts := old.UTC().Format(time.RFC3339)
	write := func(id, body string, mtime time.Time) {
		p := filepath.Join(root, ".claude", "projects", "-work-app", id+".jsonl")
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	meta := `{"type":"permission-mode","permissionMode":"default"}` + "\n"
	ask := fmt.Sprintf(`{"type":"user","timestamp":%q,"cwd":"/work/app","message":{"content":"fix the build"}}`+"\n", ts)
	reply := fmt.Sprintf(`{"type":"assistant","timestamp":%q,"cwd":"/work/app","message":{"id":"m1","model":"claude-opus-5-5","usage":{"input_tokens":10,"output_tokens":5}}}`+"\n", ts)
	write(cleanEmpty, meta, old)
	write(cleanAsked, ask, old)
	write(cleanUsed, ask+reply, old)
	write(cleanRecent, meta, time.Now().Add(-time.Minute))
	write(cleanNamed, meta+`{"type":"custom-title","customTitle":"release notes"}`+"\n", old)
	return root
}

func runCLI(t *testing.T, args ...string) []byte {
	t.Helper()
	var out, errw bytes.Buffer
	if code := Main(args, &out, &errw); code != 0 {
		t.Fatalf("ccli %v: exit %d\n%s", args, code, errw.String())
	}
	return out.Bytes()
}

func planIDs(t *testing.T, b []byte) map[string]string {
	t.Helper()
	var rows []struct {
		Action string `json:"action"`
		ID     string `json:"session_id"`
	}
	if err := json.Unmarshal(b, &rows); err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	out := map[string]string{}
	for _, r := range rows {
		out[r.ID] = r.Action
	}
	return out
}

func TestCleanPlanApplyUndo(t *testing.T) {
	root := cleanHome(t)
	transcript := filepath.Join(root, ".claude", "projects", "-work-app", cleanEmpty+".jsonl")

	// Without --apply, and with no terminal to ask on, nothing changes.
	plan := planIDs(t, runCLI(t, "clean", "--json"))
	if len(plan) != 1 || plan[cleanEmpty] != "archive" {
		t.Fatalf("plan %v, want only the empty session archived", plan)
	}
	if _, err := os.Stat(transcript); err != nil {
		t.Fatal("a dry run moved the transcript")
	}
	if plan := planIDs(t, runCLI(t, "clean", "--json", "--unanswered")); len(plan) != 2 || plan[cleanAsked] != "archive" {
		t.Fatalf("--unanswered plan %v", plan)
	}
	// The empty transcript names no folder; its project folder's other
	// sessions do, so --dir finds it.
	if plan := planIDs(t, runCLI(t, "clean", "--json", "--dir", "/work/app")); plan[cleanEmpty] != "archive" {
		t.Fatalf("--dir plan %v", plan)
	}

	var done struct {
		Undo     string `json:"undo"`
		Sessions []struct {
			ID   string `json:"session_id"`
			Note string `json:"note"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(runCLI(t, "clean", "--apply", "--json"), &done); err != nil {
		t.Fatal(err)
	}
	if done.Undo == "" || len(done.Sessions) != 1 || done.Sessions[0].Note != "" {
		t.Fatalf("apply: %+v", done)
	}
	archived := filepath.Join(root, "data", "ccli", "archive", "clean", done.Undo, "default", "projects", "-work-app", cleanEmpty+".jsonl")
	if _, err := os.Stat(archived); err != nil {
		t.Fatalf("not in the archive: %v", err)
	}
	if _, err := os.Stat(transcript); err == nil {
		t.Fatal("the transcript is still in the account")
	}
	if b := runCLI(t, "sessions", "--days", "0", "--json"); bytes.Contains(b, []byte(cleanEmpty)) || !bytes.Contains(b, []byte(cleanUsed)) {
		t.Fatalf("sessions after clean:\n%s", b)
	}

	runCLI(t, "clean", "--undo", done.Undo)
	if _, err := os.Stat(transcript); err != nil {
		t.Fatalf("undo did not put the transcript back: %v", err)
	}
	if b := runCLI(t, "sessions", "--days", "0", "--json"); !bytes.Contains(b, []byte(cleanEmpty)) {
		t.Fatalf("sessions after undo:\n%s", b)
	}
	if _, err := os.Stat(filepath.Join(root, "data", "ccli", "archive", "clean", done.Undo)); err == nil {
		t.Fatal("the emptied archive folder was kept")
	}
}

// A transcript written to between the plan and the apply stays where it is.
func TestCleanLeavesATranscriptThatChanged(t *testing.T) {
	root := cleanHome(t)
	ctx := context.Background()
	a := &app{out: io.Discard, errw: io.Discard}
	if err := a.open(); err != nil {
		t.Fatal(err)
	}
	defer a.close()
	if _, err := a.refresh(ctx, false); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	rows, err := query.New(a.st, a.accts).Empty(ctx, query.Filter{}, false, time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	plan := a.cleanPlan(rows, time.Hour, now)
	if len(plan) != 1 || plan[0].ID != cleanEmpty {
		t.Fatalf("plan %+v", plan)
	}
	transcript := filepath.Join(root, ".claude", "projects", "-work-app", cleanEmpty+".jsonl")
	f, err := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"type":"user","message":{"content":"back again"}}` + "\n")
	f.Close()
	stamp, err := a.cleanApply(ctx, plan, now)
	if err != nil || stamp != "" || plan[0].Note == "" {
		t.Fatalf("stamp %q err %v note %q", stamp, err, plan[0].Note)
	}
	if _, err := os.Stat(transcript); err != nil {
		t.Fatal("the changed transcript was moved")
	}
}
