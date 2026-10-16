package render

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/lr00rl/ccli/internal/account"
	"github.com/lr00rl/ccli/internal/quota"
)

func quotaText(rows []QuotaRow, now time.Time) string {
	var b bytes.Buffer
	Term{W: &b, Width: 120, Now: now, Loc: time.UTC}.Quota(rows)
	return b.String()
}

// The footnotes describe only the sources used: when every check failed,
// nothing claims live figures.
func TestQuotaFootnotesFollowTheRows(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	saved := &quota.Reading{UsedPercent: 40, ResetsAt: now.Add(time.Hour), At: now.Add(-3 * time.Hour), Source: "status line"}
	failed := quotaText([]QuotaRow{
		{Account: "default", Tool: account.Claude, LiveError: "claude is not installed (not found on PATH)", FiveHour: saved},
		{Account: "codex", Tool: account.Codex, LiveError: "Codex: not signed in"},
	}, now)
	for _, want := range []string{"live check failed: claude is not installed", "from the status line, 3h ago", "last saved figures, each with its source and age"} {
		if !strings.Contains(failed, want) {
			t.Fatalf("missing %q in:\n%s", want, failed)
		}
	}
	if strings.Contains(failed, "Live Claude figures") || strings.Contains(failed, "Live Codex figures") {
		t.Fatalf("claims live figures:\n%s", failed)
	}
	live := quotaText([]QuotaRow{{Account: "default", Tool: account.Claude, Live: true,
		FiveHour: &quota.Reading{UsedPercent: 26, ResetsAt: now.Add(time.Hour), At: now, Source: "Claude Code"}}}, now)
	if !strings.Contains(live, "Live Claude figures come from Claude Code") || strings.Contains(live, "Codex") {
		t.Fatalf("live footnote:\n%s", live)
	}
}

// A saved extra window whose reset has passed says so without claiming a
// count of use, and extra use carries its source and age.
func TestQuotaExtraWindowsFromAnOldCheck(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	out := quotaText([]QuotaRow{{Account: "default", Tool: account.Claude,
		Limits:   []quota.Limit{{Name: "Fable weekly", Window: quota.Window{UsedPercent: 70, ResetsAt: now.Add(-time.Hour)}}},
		Extra:    &quota.Extra{UsedPercent: 25, Used: 12.5, Limit: 50, Currency: "USD"},
		LimitsAt: now.Add(-26 * time.Hour), LimitsFrom: "Claude Code",
	}}, now)
	if !strings.Contains(out, "Fable weekly") || !strings.Contains(out, "reset at 11:00 (was 70%)") || strings.Contains(out, "likely near 0%") {
		t.Fatalf("expired extra window:\n%s", out)
	}
	if !strings.Contains(out, "$12.50 of $50.00 this month (25%)") || !strings.Contains(out, "from Claude Code, 1d ago") {
		t.Fatalf("extra use:\n%s", out)
	}
}
