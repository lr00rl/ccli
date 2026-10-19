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
	// Limits and Extra come from a live check: per-model weekly windows,
	// windows of other lengths, and pay-as-you-go use past the plan.
	Limits     []quota.Limit `json:"limits,omitempty"`
	Extra      *quota.Extra  `json:"extra_usage,omitempty"`
	LimitsAt   time.Time     `json:"limits_at,omitzero"`    // when the live check holding Limits and Extra answered
	LimitsFrom string        `json:"limits_from,omitempty"` // and which tool
	Note       string        `json:"note,omitempty"`        // a short note from the live check
	// Live is true when the figures were checked just now; LiveError says
	// why a check failed, and the figures are then the last saved ones.
	Live      bool   `json:"live"`
	LiveError string `json:"live_error,omitempty"`
	Offline   bool   `json:"-"` // --offline: no check was made
}

// WindowUse is what this machine's transcripts used in a window.
type WindowUse struct {
	Counted bool         `json:"-"` // ccli counted use in this window (the 5-hour and weekly ones)
	Since   time.Time    `json:"since"`
	Exact   bool         `json:"exact"` // Since is the window's real start (from its reset time), not just now minus its length
	Tokens  query.Tokens `json:"tokens"`
	Cost    query.Cost   `json:"cost"`
}

// Quota prints subscription limits per account.
func (t Term) Quota(rows []QuotaRow) {
	// What the footnotes describe: only the sources these rows used.
	missing, offline, failed, claudeLive, codexLive, cursorLive := false, false, false, false, false, false
	for i, r := range rows {
		offline = offline || r.Offline
		failed = failed || r.LiveError != ""
		claudeLive = claudeLive || r.Live && r.Tool == account.Claude
		codexLive = codexLive || r.Live && r.Tool == account.Codex
		cursorLive = cursorLive || r.Live && (r.Tool == account.Cursor || r.Tool == account.CursorIDE)
		lw := 9 // the label column: "5-hour", "weekly", or a longer limit's name
		for _, l := range r.Limits {
			lw = max(lw, width(l.Name)+1)
		}
		if i > 0 {
			t.println()
		}
		head := t.bold(r.Account)
		switch {
		case r.Email != "":
			head += t.dim("  " + r.Email)
		case r.Tool != account.Claude:
			head += t.dim("  " + r.Tool.Label())
		}
		if r.Plan != "" {
			head += t.dim(" · " + r.Plan + " plan")
		}
		t.println(head)
		if r.LiveError != "" {
			t.printf("  %s\n", t.warn("live check failed: "+r.LiveError))
		}
		if r.FiveHour == nil && r.SevenDay == nil && len(r.Limits) == 0 {
			missing = missing || r.Tool == account.Claude
			t.printf("  %s\n", t.dim("no limits recorded yet"))
		}
		t.window("5-hour", r.FiveHour, r.Use5h, lw)
		t.window("weekly", r.SevenDay, r.Use7d, lw)
		var more *quota.Reading // the time and source of the extra windows
		for _, l := range r.Limits {
			t.window(l.Name, &quota.Reading{UsedPercent: l.UsedPercent, ResetsAt: l.ResetsAt}, WindowUse{}, lw)
		}
		if len(r.Limits) > 0 || r.Extra != nil {
			more = &quota.Reading{At: r.LimitsAt, Source: r.LimitsFrom}
		}
		if e := r.Extra; e != nil {
			label := "extra use"
			if r.Tool == account.Cursor || r.Tool == account.CursorIDE {
				label = "included"
			}
			text := pct(e.UsedPercent) + " of the monthly cap"
			if e.Limit > 0 {
				text = fmt.Sprintf("%s of %s this month (%s)", money(e.Used, e.Currency), money(e.Limit, e.Currency), pct(e.UsedPercent))
			}
			if e.Bonus > 0 {
				text += " · " + money(e.Bonus, e.Currency) + " bonus"
			}
			t.printf("  %s %s\n", t.dim(pad(label, lw, false)), text)
		}
		if r.Note != "" {
			t.printf("  %s\n", t.dim(r.Note))
		}
		if src := sources(t, r.FiveHour, r.SevenDay, more); src != "" {
			t.printf("  %s %s\n", pad("", lw, false), t.dim(src))
		}
	}
	t.println()
	if offline {
		t.println(t.dim("These are the last saved figures. Without --offline, ccli asks Claude Code, Codex and Cursor for the current ones."))
	}
	if claudeLive {
		t.println(t.dim("Live Claude figures come from Claude Code itself, with the same request /usage makes, so use on claude.ai"))
		t.println(t.dim("and other machines counts. ccli starts it once per account with no prompt, hooks or MCP servers."))
	}
	if codexLive {
		t.println(t.dim("Live Codex figures come from Codex's app server, started once per account."))
	}
	if cursorLive {
		t.println(t.dim("Live Cursor figures are the signed-in account's billing cycle from Cursor's usage page. The same cycle is shown for the CLI and the IDE."))
	}
	if failed {
		t.println(t.dim("An account whose check failed shows its last saved figures, each with its source and age."))
	}
	if missing && offline {
		t.println(t.dim("To record limits as you work, put \"ccli statusline --\" in front of your statusLine command (see ccli statusline -h)."))
	}
}

// money is an amount with its currency: "$12.34", or "12.34 EUR".
func money(v float64, currency string) string {
	if currency == "" || strings.EqualFold(currency, "USD") {
		return fmt.Sprintf("$%.2f", v)
	}
	return fmt.Sprintf("%.2f %s", v, currency)
}

func (t Term) window(name string, rd *quota.Reading, use WindowUse, lw int) {
	label := t.dim(pad(name, lw, false))
	spent := usageText(t, use)
	switch {
	case rd == nil:
		if use.Tokens.InOut() > 0 {
			t.printf("  %s %s\n", label, t.dim("not reported · "+spent))
		}
		return
	case rd.Expired(t.Now):
		text := fmt.Sprintf("reset at %s (was %s)", clock(t, rd.ResetsAt), pct(rd.UsedPercent))
		switch {
		case !use.Counted: // a window ccli does not count use in
		case use.Tokens.InOut() > 0:
			text += "; " + spent + "; the next report will give the new figure"
		default:
			text += "; no use recorded here since, so likely near 0%"
		}
		t.printf("  %s %s\n", label, t.dim(text))
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
		t.printf("  %s %s\n", pad("", lw, false), t.dim(spent))
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
