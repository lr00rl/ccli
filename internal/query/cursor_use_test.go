package query

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/lr00rl/ccli/internal/account"
	"github.com/lr00rl/ccli/internal/cursorusage"
	"github.com/lr00rl/ccli/internal/store"
)

func TestMergeBilledLeavesIDETokens(t *testing.T) {
	local := Tokens{Input: 12, Output: 6}
	u := cursorusage.Use{Input: 100, Output: 40, USD: 1.25, Events: 2}
	got, cost := mergeBilled(account.CursorIDE, local, Cost{}, u)
	if got != local || cost.USD != 1.25 || cost.Unpriced != 0 {
		t.Fatalf("tokens %+v cost %+v", got, cost)
	}
	priced, cost := mergeBilled(account.CursorIDE, local, Cost{USD: 0.5}, u)
	if priced != local || cost.USD != 0.5 {
		t.Fatalf("priced tokens %+v cost %+v", priced, cost)
	}
	cli, cost := mergeBilled(account.Cursor, Tokens{}, Cost{}, u)
	if cli.Input != 100 || cli.Output != 40 || cost.USD != 1.25 {
		t.Fatalf("cli %+v %+v", cli, cost)
	}
}

// A load that fails, here because its context is cancelled, must say so and
// must not leave empty joins behind: they would drop a subagent's billed
// usage from every later total.
func TestCursorLinksFailureIsNotKept(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "ccli.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, f := range [][]string{
		{"/c/parent.jsonl", "cursor", "parent", "main", ""},
		{"/c/kid.jsonl", "cursor", "parent", "subagent", "kid"},
		{"/ide#composer", "cursor-ide", "composer", "main", ""},
	} {
		if _, err := st.DB().Exec(`INSERT INTO files(path, account, session_id, project_key, kind, agent_id) VALUES (?, ?, ?, '', ?, ?)`,
			f[0], f[1], f[2], f[3], f[4]); err != nil {
			t.Fatal(err)
		}
	}
	d := New(st, []account.Account{{Name: "cursor", Tool: account.Cursor}, {Name: "cursor-ide", Tool: account.CursorIDE}})
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := d.cursorLinks(cancelled); err == nil {
		t.Fatal("a cancelled load reported no error")
	}
	l, err := d.cursorLinks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(l.kids["parent"]) != 1 || l.ideCopy(account.Cursor, "composer") != "cursor-ide" || !l.billedHere(account.Cursor, "parent") {
		t.Fatalf("joins after a failed load: kids %v, ide %v", l.kids, l.ideIDs)
	}
}
