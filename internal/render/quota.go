package render

import (
	"fmt"
	"strings"
	"time"

	"github.com/lr00rl/ccli/internal/account"
	"github.com/lr00rl/ccli/internal/query"
	"github.com/lr00rl/ccli/internal/quota"
)

// QuotaRow is one account's limits and the usage ccli counted in them.
type QuotaRow struct {
	Account  string         `json:"account"`
	Email    string         `json:"email,omitempty"`
	Tool     account.Tool   `json:"tool"`
	Plan     string         `json:"plan,omitempty"`
	FiveHour *quota.Reading `json:"five_hour,omitempty"`
	SevenDay *quota.Reading `json:"seven_day,omitempty"`
	Use5h    WindowUse      `json:"use_five_hour"`
	Use7d    WindowUse      `json:"use_seven_day"`
}

// WindowUse is what this machine's transcripts used in a window.
type WindowUse struct {
	Since  time.Time    `json:"since"`
	Exact  bool         `json:"exact"` // Since is the window's real start (from its reset time), not just now minus its length
	Tokens query.Tokens `json:"tokens"`
	Cost   query.Cost   `json:"cost"`
}

// Quota prints subscription limits per account.
func (t Term) Quota(rows []QuotaRow) {
	missing, claude, codex := false, false, false
	for i, r := range rows {
		if i > 0 {
			t.println()
		}
		head := t.bold(r.Account)
		switch {
		case r.Email != "":
			head += t.dim("  " + r.Email)
		case r.Tool == account.Codex:
			head += t.dim("  Codex")
		}
		if r.Plan != "" {
			head += t.dim(" · " + r.Plan + " plan")
		}
		t.println(head)
		if r.Tool == account.Codex {
			codex = true
		} else {
			claude = true
		}
		if r.FiveHour == nil && r.SevenDay == nil {
			missing = missing || r.Tool != account.Codex
			t.printf("  %s\n", t.dim("no limits recorded yet"))
		}
		t.window("5-hour", r.FiveHour, r.Use5h)
		t.window("weekly", r.SevenDay, r.Use7d)
		if src := sources(t, r.FiveHour, r.SevenDay); src != "" {
			t.printf("  %s %s\n", pad("", 9, false), t.dim(src))
		}
	}
	t.println()
	if claude {
		t.println(t.dim("Claude percentages are Anthropic's own figures, as Claude Code passes them to the status line (and ccline"))
		t.println(t.dim("reads them from the usage API). Use on claude.ai or another machine counts too but shows up only in the next report."))
	}
	if codex {
		t.println(t.dim("Codex percentages are OpenAI's own figures, which Codex writes to its session logs with each reply."))
	}
	if missing {
		t.println(t.dim("To record limits, put \"ccli statusline --\" in front of your statusLine command (see ccli statusline -h)."))
	}
}

func (t Term) window(name string, rd *quota.Reading, use WindowUse) {
	label := t.dim(pad(name, 9, false))
	spent := usageText(t, use)
	switch {
	case rd == nil:
		if use.Tokens.InOut() > 0 {
			t.printf("  %s %s\n", label, t.dim("not reported · "+spent))
		}
		return
	case rd.Expired(t.Now):
		state := "no use recorded here since, so likely near 0%"
		if use.Tokens.InOut() > 0 {
			state = spent + "; the next report will give the new figure"
		}
		t.printf("  %s %s\n", label, t.dim(fmt.Sprintf("reset at %s (was %s); %s", clock(t, rd.ResetsAt), pct(rd.UsedPercent), state)))
		return
	}
	const cells = 24
	b := bar(rd.UsedPercent/100, cells)
	color := cAccent
	switch {
	case rd.UsedPercent >= 90:
		color = cLive
	case rd.UsedPercent >= 70:
		color = cWarn
	}
	reset := t.dim("reset time unknown")
	if !rd.ResetsAt.IsZero() {
		reset = t.dim("resets in " + Until(rd.ResetsAt.Sub(t.Now)) + " (" + clock(t, rd.ResetsAt) + ")")
	}
	t.printf("  %s %s%s %s  %s\n", label, t.paint(color, b), t.dim(strings.Repeat("·", cells-width(b))), t.bold(fmt.Sprintf("%6s", pct(rd.UsedPercent))), reset)
	if spent != "" {
		t.printf("  %s %s\n", pad("", 9, false), t.dim(spent))
	}
}

func usageText(t Term, u WindowUse) string {
	if u.Tokens.InOut() == 0 && u.Cost.USD == 0 {
		return ""
	}
	when := "in this window"
	if !u.Exact {
		when = "since " + clock(t, u.Since)
	}
	return fmt.Sprintf("used here %s: %s tokens, %s cache read, %s list price", when, Num(u.Tokens.InOut()), Num(u.Tokens.CacheRead), costText(u.Cost))
}

func sources(t Term, rs ...*quota.Reading) string {
	var parts []string
	seen := map[string]bool{}
	for _, r := range rs {
		if r == nil || seen[r.Source] {
			continue
		}
		seen[r.Source] = true
		src := r.Source
		if src == "status line" {
			src = "the status line"
		}
		parts = append(parts, fmt.Sprintf("from %s, %s", src, t.Ago(r.At)))
	}
	return strings.Join(parts, " · ")
}

// pct shows a percentage with one decimal when it has one.
func pct(v float64) string {
	s := fmt.Sprintf("%.1f", v)
	return strings.TrimSuffix(s, ".0") + "%"
}

// clock is a time of day, with the weekday and date when it is not today.
func clock(t Term, at time.Time) string {
	at = at.In(t.Loc)
	now := t.Now.In(t.Loc)
	if at.Year() == now.Year() && at.YearDay() == now.YearDay() {
		return at.Format("15:04")
	}
	return at.Format("Mon 01-02 15:04")
}

// Until formats a time left: "2h 10m", "3d 12h", "40m".
func Until(d time.Duration) string {
	if d >= 48*time.Hour {
		return fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
	}
	if d < time.Minute {
		return "under a minute"
	}
	return Dur(d.Milliseconds())
}
