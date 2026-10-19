// Package render formats query results for a terminal: aligned tables that
// handle wide (CJK) characters, small charts, and restrained color.
package render

import (
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mattn/go-runewidth"
	"golang.org/x/term"
)

// Term describes the output.
type Term struct {
	W     io.Writer
	Width int
	Color bool
	Now   time.Time
	Loc   *time.Location
}

// NewTerm inspects w. mode is "auto", "always" or "never".
func NewTerm(w io.Writer, mode string) Term {
	t := Term{W: w, Width: 140, Now: time.Now(), Loc: time.Local}
	if n, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil && n > 20 {
		t.Width = n
	}
	f, isFile := w.(*os.File)
	tty := isFile && term.IsTerminal(int(f.Fd()))
	if tty {
		if cols, _, err := term.GetSize(int(f.Fd())); err == nil && cols > 20 {
			t.Width = cols
		}
	}
	switch mode {
	case "always":
		t.Color = true
	case "never":
		t.Color = false
	default:
		t.Color = tty && os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb"
	}
	return t
}

// Styles. One accent (blue), green for money, amber for warnings, grey for
// secondary text.
const (
	cAccent = "38;5;75"
	cMoney  = "38;5;114"
	cWarn   = "38;5;214"
	cDim    = "38;5;245"
	cBold   = "1"
	cLive   = "38;5;203"
	cTag    = "38;5;180"
)

func (t Term) paint(code, s string) string {
	if !t.Color || s == "" {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func (t Term) dim(s string) string    { return t.paint(cDim, s) }
func (t Term) bold(s string) string   { return t.paint(cBold, s) }
func (t Term) accent(s string) string { return t.paint(cAccent, s) }
func (t Term) money(s string) string  { return t.paint(cMoney, s) }
func (t Term) warn(s string) string   { return t.paint(cWarn, s) }
func (t Term) tag(s string) string    { return t.paint(cTag, s) }

func (t Term) printf(format string, a ...any) { fmt.Fprintf(t.W, format, a...) }
func (t Term) println(a ...any)               { fmt.Fprintln(t.W, a...) }

// width is the display width of s, ignoring color codes.
func width(s string) int { return runewidth.StringWidth(stripANSI(s)) }

func stripANSI(s string) string {
	if !strings.Contains(s, "\x1b[") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && s[j] != 'm' {
				j++
			}
			i = j
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// fit truncates s (plain text) to w display columns with an ellipsis.
func fit(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if runewidth.StringWidth(s) <= w {
		return s
	}
	return runewidth.Truncate(s, w, "…")
}

// pad pads s (may contain color) to w display columns.
func pad(s string, w int, right bool) string {
	n := w - width(s)
	if n <= 0 {
		return s
	}
	if right {
		return strings.Repeat(" ", n) + s
	}
	return s + strings.Repeat(" ", n)
}

// Num shortens a count: 950, 12.3K, 4.56M, 1.2B.
func Num(n int64) string {
	f := float64(n)
	switch {
	case n < 0:
		return "-" + Num(-n)
	case n < 1000:
		return fmt.Sprintf("%d", n)
	case n < 1_000_000:
		return trim(fmt.Sprintf("%.1f", f/1e3)) + "K"
	case n < 1_000_000_000:
		return trim(fmt.Sprintf("%.1f", f/1e6)) + "M"
	}
	return trim(fmt.Sprintf("%.2f", f/1e9)) + "B"
}

func trim(s string) string {
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s
}

// USD formats a list-price cost.
func USD(v float64) string {
	switch {
	case v == 0:
		return "$0"
	case v < 0.01:
		return "<$0.01"
	case v < 100:
		return fmt.Sprintf("$%.2f", v)
	}
	return "$" + commas(int64(math.Round(v)))
}

func commas(n int64) string {
	s := fmt.Sprintf("%d", n)
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}

// Dur formats a duration as "3h 05m", "12m", "40s".
func Dur(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	switch {
	case d >= time.Hour:
		return fmt.Sprintf("%dh %02dm", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d > 0:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return "-"
}

// Ago formats a time relative to now.
func (t Term) Ago(at time.Time) string {
	if at.IsZero() {
		return "-"
	}
	d := t.Now.Sub(at)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 14*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
	return at.In(t.Loc).Format("2006-01-02")
}

// bar draws a horizontal bar of frac (0..1) over w cells with eighth blocks.
func bar(frac float64, w int) string {
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	eighths := int(math.Round(frac * float64(w*8)))
	full, rest := eighths/8, eighths%8
	s := strings.Repeat("█", full)
	if rest > 0 {
		s += string([]rune("▏▎▍▌▋▊▉")[rest-1])
	}
	return s
}

// spark maps values to a one-line chart.
func spark(vs []float64) string {
	levels := []rune("▁▂▃▄▅▆▇█")
	max := 0.0
	for _, v := range vs {
		max = math.Max(max, v)
	}
	var b strings.Builder
	for _, v := range vs {
		if max == 0 || v == 0 {
			b.WriteRune(' ')
			continue
		}
		i := int(math.Round(v / max * float64(len(levels)-1)))
		if i < 0 {
			i = 0
		}
		b.WriteRune(levels[i])
	}
	return b.String()
}

// column describes one table column.
type column struct {
	title string
	right bool
	min   int // narrowest acceptable width; 0 means the content's width
	flex  bool
}

// table prints rows with columns sized to fit the terminal; flex columns
// shrink first.
func (t Term) table(cols []column, rows [][]string) {
	ws := make([]int, len(cols))
	for i, c := range cols {
		ws[i] = width(c.title)
	}
	for _, r := range rows {
		for i := range cols {
			if w := width(r[i]); w > ws[i] {
				ws[i] = w
			}
		}
	}
	total := 0
	for _, w := range ws {
		total += w
	}
	total += 2 * (len(cols) - 1)
	// Shrink the widest flexible column one cell at a time until the row fits.
	for over := total - t.Width; over > 0; over-- {
		best := -1
		for i, c := range cols {
			min := c.min
			if min == 0 {
				min = 12
			}
			if c.flex && ws[i] > min && (best < 0 || ws[i] > ws[best]) {
				best = i
			}
		}
		if best < 0 {
			break
		}
		ws[best]--
	}
	line := func(cells []string, header bool) {
		var b strings.Builder
		for i, c := range cols {
			cell := cells[i]
			if !header {
				cell = strings.Join(strings.Fields(cell), " ")
			}
			if width(cell) > ws[i] {
				cell = fit(stripANSI(cell), ws[i])
			}
			if header {
				cell = t.dim(cell)
			}
			if i == len(cols)-1 && !c.right {
				b.WriteString(cell)
			} else {
				b.WriteString(pad(cell, ws[i], c.right))
			}
			if i < len(cols)-1 {
				b.WriteString("  ")
			}
		}
		t.println(strings.TrimRight(b.String(), " "))
	}
	titles := make([]string, len(cols))
	for i, c := range cols {
		titles[i] = c.title
	}
	line(titles, true)
	for _, r := range rows {
		line(r, false)
	}
}
