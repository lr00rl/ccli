package tui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/lr00rl/ccli/internal/conv"
	"github.com/lr00rl/ccli/internal/query"
)

func turnCalls(data any) []*conv.Call {
	d, ok := data.(*query.TurnDetail)
	if !ok || d == nil || d.Turn == nil {
		return nil
	}
	var out []*conv.Call
	for i := range d.Turn.Entries {
		if d.Turn.Entries[i].Kind == conv.KindTool && d.Turn.Entries[i].Call != nil {
			out = append(out, d.Turn.Entries[i].Call)
		}
	}
	return out
}

func (v *viewer) selectedCalls() []*conv.Call {
	s := v.cur()
	idx := s.list.selected()
	if idx < 0 {
		return nil
	}
	return turnCalls(s.data[idx])
}

// stepTool moves the tool inspector. d is -1, 0 or 1.
func (v *viewer) stepTool(s *screen, d int) {
	if s.role != turnsRole {
		return
	}
	v.toolFor(itemKey(s, s.list.selected()))
	calls := v.selectedCalls()
	if len(calls) == 0 {
		if idx := s.list.selected(); idx >= 0 && s.data[idx] == nil && s.errs[idx] == nil {
			v.status = "still loading"
			return
		}
		v.tools = false
		v.status = "this turn has no tool calls"
		return
	}
	v.toolN += d
	if v.toolN < 0 {
		v.toolN = 0
	}
	if v.toolN >= len(calls) {
		v.toolN = len(calls) - 1
	}
	v.status = fmt.Sprintf("tool %d/%d  [ ] another  esc the transcript", v.toolN+1, len(calls))
}

// toolFor starts the inspector at the first call when the selection moved
// to another turn since toolN was set.
func (v *viewer) toolFor(item string) {
	if v.toolAt != item {
		v.toolAt, v.toolN = item, 0
	}
}

func itemKey(s *screen, idx int) string { return fmt.Sprintf("%p/%d/%d/", s, s.gen, idx) }

// toolPane is one tool call, with the nearby calls listed above it.
func (v *viewer) toolPane(data any, w int) string {
	calls := turnCalls(data)
	if len(calls) == 0 {
		return dim("This turn has no tool calls.")
	}
	n := v.toolN
	if n < 0 || w < 1 {
		n = 0
	}
	if n >= len(calls) {
		n = len(calls) - 1
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", bold(fmt.Sprintf("tool %d of %d", n+1, len(calls))))
	from := max(0, n-3)
	to := min(len(calls), from+8)
	from = max(0, to-8)
	for i := from; i < to; i++ {
		mark := "  "
		name := calls[i].Name
		sum := oneLine(conv.Printable(calls[i].Summary))
		if sum == "" || sum == name {
			sum = oneLine(conv.Printable(callArg(calls[i])))
		}
		line := fmt.Sprintf("%2d  %s  %s", i+1, name, sum)
		if i == n {
			mark = "▸ "
			line = bold(line)
		} else {
			line = dim(line)
		}
		if calls[i].IsError {
			line = warn(fmt.Sprintf("%2d  %s  %s", i+1, name, sum))
		}
		fmt.Fprintf(&b, "%s%s\n", mark, line)
	}
	b.WriteByte('\n')
	c := calls[n]
	head := c.Name
	if c.IsError {
		head += "  failed"
	}
	if !c.Done {
		head += "  no result"
	}
	fmt.Fprintf(&b, "%s\n", bold(head))
	// Recorded text is shown without its escape sequences: they would act on
	// the terminal, not be read.
	if sum := oneLine(conv.Printable(c.Summary)); sum != "" && sum != c.Name {
		fmt.Fprintf(&b, "%s\n", dim(sum))
	}
	if c.Prompt != "" {
		fmt.Fprintf(&b, "\n%s\n%s\n", dim("prompt"), indent(conv.Printable(c.Prompt), "  "))
	}
	if in := callBody(c.Input); in != "" {
		fmt.Fprintf(&b, "\n%s\n%s\n", dim("input"), indent(conv.Printable(in), "  "))
	}
	out := strings.TrimRight(conv.Printable(c.Output), "\n")
	if out != "" {
		shown, extra := clipLines(out, 80)
		fmt.Fprintf(&b, "\n%s\n%s\n", dim("output"), indent(shown, "  "))
		if extra > 0 {
			fmt.Fprintf(&b, "%s\n", dim(fmt.Sprintf("  … %d more lines", extra)))
		}
	} else if c.OutputLen > 0 {
		fmt.Fprintf(&b, "\n%s\n", dim(fmt.Sprintf("output kept aside, %d bytes", c.OutputLen)))
	}
	return b.String()
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func callArg(c *conv.Call) string {
	var in map[string]any
	if json.Unmarshal(c.Input, &in) != nil {
		var s string
		if json.Unmarshal(c.Input, &s) == nil {
			return s
		}
		return ""
	}
	for _, k := range []string{"command", "file_path", "path", "pattern", "query", "url", "description"} {
		if s, ok := in[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func callBody(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var buf bytes.Buffer
	if json.Indent(&buf, raw, "", "  ") == nil {
		return clipJoin(buf.String(), 60)
	}
	return clipJoin(string(raw), 60)
}

func clipJoin(s string, n int) string {
	shown, extra := clipLines(s, n)
	if extra > 0 {
		return shown + fmt.Sprintf("\n… %d more lines", extra)
	}
	return shown
}

func clipLines(s string, n int) (string, int) {
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s, 0
	}
	return strings.Join(lines[:n], "\n"), len(lines) - n
}

func indent(s, pad string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, ln := range lines {
		lines[i] = pad + ln
	}
	return strings.Join(lines, "\n")
}
