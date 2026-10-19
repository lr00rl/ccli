package query

import (
	"testing"

	"github.com/lr00rl/ccli/internal/account"
	"github.com/lr00rl/ccli/internal/cursorusage"
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
