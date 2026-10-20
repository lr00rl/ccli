// Package export writes one session as a readable transcript. The text is
// for a person or another agent to continue from. It is not a file any tool
// can resume.
package export

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/lr00rl/ccli/internal/conv"
	"github.com/lr00rl/ccli/internal/query"
)

// Options selects how much of the conversation is written.
type Options struct {
	Format   string // md or jsonl. Empty means md.
	Detail   string // full (default) or reply. reply keeps each turn's prompt and its last reply.
	Thinking bool   // include the model's thinking
	Context  bool   // include text the tool added (skills, reminders, peer messages)
	Agents   bool   // inline a subagent's conversation after the turn that started it
	Loc      *time.Location
}

// Doc is one session export. Agents is keyed by agent id and is used only
// when Options.Agents is set.
type Doc struct {
	Session query.SessionRow
	Turns   []*query.TurnDetail
	Agents  map[string]*query.AgentLog
}

// Write writes doc to w.
func Write(w io.Writer, doc Doc, opt Options) error {
	if opt.Loc == nil {
		opt.Loc = time.Local
	}
	switch strings.ToLower(strings.TrimSpace(opt.Detail)) {
	case "", "full", "reply":
	default:
		return fmt.Errorf("export detail %q: use full or reply", opt.Detail)
	}
	switch strings.ToLower(opt.Format) {
	case "", "md", "markdown":
		return writeMD(w, doc, opt)
	case "jsonl":
		return writeJSONL(w, doc, opt)
	default:
		return fmt.Errorf("export format %q: use md or jsonl", opt.Format)
	}
}

func writeMD(w io.Writer, doc Doc, opt Options) error {
	var b strings.Builder
	title := oneLine(doc.Session.Title)
	if title == "" {
		title = "(untitled)"
	}
	fmt.Fprintf(&b, "# %s\n\n", title)
	fmt.Fprintf(&b, "%s · %s · %s\n\n", doc.Session.Tool, doc.Session.Account, doc.Session.ID)
	if cwd := oneLine(doc.Session.Cwd); cwd != "" {
		fmt.Fprintf(&b, "%s\n\n", cwd)
	}
	if !doc.Session.Started.IsZero() {
		end := doc.Session.Last.In(opt.Loc).Format("2006-01-02 15:04")
		fmt.Fprintf(&b, "%s → %s\n\n", doc.Session.Started.In(opt.Loc).Format("2006-01-02 15:04"), end)
	}
	if len(doc.Session.Models) > 0 {
		fmt.Fprintf(&b, "%s\n\n", strings.Join(doc.Session.Models, ", "))
	}
	b.WriteString("This is a readable export. It is not a file any tool can resume.\n")
	for _, d := range doc.Turns {
		if d == nil {
			continue
		}
		fmt.Fprintf(&b, "\n## Turn %d\n", d.Row.Seq)
		writeTurnMD(&b, d.Turn, opt, "")
		if opt.reply() {
			continue
		}
		writeAgentsMD(&b, d.Agents, doc.Agents, opt)
		for _, r := range d.Runs {
			name := r.Name
			if name == "" {
				name = r.ID
			}
			fmt.Fprintf(&b, "\nStarted workflow %s.\n", name)
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func writeTurnMD(b *strings.Builder, tr *conv.Turn, opt Options, agent string) {
	if tr == nil {
		b.WriteString("\nThis part of the transcript could not be read.\n")
		return
	}
	speaker := tr.Speaker
	if speaker == "" {
		speaker = "Assistant"
	}
	entries := tr.Entries
	if opt.reply() {
		entries = conv.ReplyEntries(entries)
	}
	for _, e := range entries {
		e.Text = conv.Printable(e.Text)
		switch e.Kind {
		case conv.KindThinking:
			if !opt.Thinking {
				continue
			}
			fmt.Fprintf(b, "\n### %s, thinking\n\n%s\n", speaker, body(strings.TrimSpace(e.Text)))
		case conv.KindContext:
			if !opt.Context {
				continue
			}
			label := e.Label
			if label == "" {
				label = "context"
			}
			fmt.Fprintf(b, "\n### Context: %s\n\n%s\n", oneLine(label), body(strings.TrimSpace(e.Text)))
		case conv.KindPrompt, conv.KindQueued:
			who := "You"
			if e.Kind == conv.KindQueued {
				who = "You, while it worked"
			}
			_, text := conv.SplitReminders(e.Text)
			text = strings.TrimSpace(conv.PromptDisplay(text))
			if text == "" && e.Images > 0 {
				text = fmt.Sprintf("(%d images)", e.Images)
			}
			if text == "" {
				continue
			}
			fmt.Fprintf(b, "\n### %s\n\n%s\n", who, body(text))
			if agent != "" {
				fmt.Fprintf(b, "\n")
			}
		case conv.KindText:
			text := strings.TrimSpace(e.Text)
			if text == "" {
				continue
			}
			who := e.Model
			if who == "" {
				who = speaker
			}
			fmt.Fprintf(b, "\n### %s\n\n%s\n", oneLine(who), body(text))
		case conv.KindTool:
			if e.Call == nil {
				continue
			}
			writeToolMD(b, e.Call)
		case conv.KindNotice:
			text := strings.TrimSpace(e.Text)
			if text == "" {
				text = e.Label
			}
			if text == "" {
				continue
			}
			fmt.Fprintf(b, "\n### Note\n\n%s\n", body(text))
		}
	}
}

// body makes text from the conversation safe to place under one of the
// document's own headings. A heading in it moves down three levels, so it
// stays a heading but cannot pass for a turn or a speaker, and a code fence
// it leaves open is closed, so it cannot swallow the rest of the document.
func body(text string) string {
	lines := strings.Split(text, "\n")
	open := "" // the fence that is open, as written
	for i, l := range lines {
		trim := strings.TrimLeft(l, " ")
		indent := len(l) - len(trim)
		if indent > 3 {
			continue
		}
		if f := fenceOf(trim); f != "" {
			if open == "" {
				open = f
			} else if f[0] == open[0] && len(f) >= len(open) && strings.TrimSpace(trim[len(f):]) == "" {
				open = ""
			}
			continue
		}
		if open != "" || !strings.HasPrefix(trim, "#") {
			continue
		}
		n := len(trim) - len(strings.TrimLeft(trim, "#"))
		if n <= 6 && (n == len(trim) || trim[n] == ' ' || trim[n] == '\t') {
			lines[i] = l[:indent] + strings.Repeat("#", min(n+3, 6)) + trim[n:]
		}
	}
	if open != "" {
		lines = append(lines, open)
	}
	return strings.Join(lines, "\n")
}

// fenceOf returns the run of three or more backticks or tildes that opens or
// closes a code block on this line, or "".
func fenceOf(s string) string {
	if len(s) < 3 || (s[0] != '`' && s[0] != '~') {
		return ""
	}
	n := 0
	for n < len(s) && s[n] == s[0] {
		n++
	}
	if n < 3 || (s[0] == '`' && strings.Contains(s[n:], "`")) {
		return ""
	}
	return s[:n]
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func writeToolMD(b *strings.Builder, c *conv.Call) {
	head := c.Name
	if c.IsError {
		head += " (failed)"
	}
	if !c.Done {
		head += " (no result)"
	}
	fmt.Fprintf(b, "\n### Tool: %s\n", head)
	if s := strings.Join(strings.Fields(c.Summary), " "); s != "" && s != c.Name {
		fmt.Fprintf(b, "\n%s\n", s)
	}
	if c.Prompt != "" {
		fmt.Fprintf(b, "\n%s\n", fence(strings.TrimSpace(conv.Printable(c.Prompt))))
	} else if in := inputText(c.Input); in != "" {
		fmt.Fprintf(b, "\n%s\n", fence(conv.Printable(in)))
	}
	out := strings.TrimRight(conv.Printable(c.Output), "\n")
	if out != "" {
		fmt.Fprintf(b, "\n%s\n", fence(out))
		if c.OutputLen > len(c.Output) {
			fmt.Fprintf(b, "\n%d more bytes were not included.\n", c.OutputLen-len(c.Output))
		}
	}
}

func writeAgentsMD(b *strings.Builder, nodes []query.AgentNode, logs map[string]*query.AgentLog, opt Options) {
	for _, n := range nodes {
		if !opt.Agents {
			fmt.Fprintf(b, "\nStarted agent %s.\n", oneLine(n.Label))
			writeAgentsMD(b, n.Children, logs, opt)
			continue
		}
		fmt.Fprintf(b, "\n### Agent: %s\n\n%s\n", oneLine(n.Label), n.AgentID)
		log := logs[n.AgentID]
		if log == nil || len(log.Turns) == 0 {
			b.WriteString("\nThe transcript for this agent is gone.\n")
		} else {
			for i := range log.Turns {
				writeTurnMD(b, &log.Turns[i], opt, n.AgentID)
			}
		}
		writeAgentsMD(b, n.Children, logs, opt)
	}
}

func writeJSONL(w io.Writer, doc Doc, opt Options) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	head := map[string]any{
		"type":    "session",
		"id":      doc.Session.ID,
		"account": doc.Session.Account,
		"tool":    string(doc.Session.Tool),
		"title":   doc.Session.Title,
		"cwd":     doc.Session.Cwd,
	}
	if !doc.Session.Started.IsZero() {
		head["started"] = doc.Session.Started.UTC().Format(time.RFC3339)
	}
	if !doc.Session.Last.IsZero() {
		head["ended"] = doc.Session.Last.UTC().Format(time.RFC3339)
	}
	if len(doc.Session.Models) > 0 {
		head["models"] = doc.Session.Models
	}
	if err := enc.Encode(head); err != nil {
		return err
	}
	for _, d := range doc.Turns {
		if d == nil {
			continue
		}
		if err := writeTurnJSONL(enc, d.Row.Seq, d.Turn, "", opt); err != nil {
			return err
		}
		if opt.reply() {
			continue
		}
		if err := writeAgentsJSONL(enc, d.Row.Seq, d.Agents, doc.Agents, opt); err != nil {
			return err
		}
	}
	return nil
}

func writeTurnJSONL(enc *json.Encoder, seq int, tr *conv.Turn, agent string, opt Options) error {
	if tr == nil {
		return enc.Encode(entryLine(seq, "note", agent, "The transcript for this part is gone.", nil))
	}
	speaker := tr.Speaker
	if speaker == "" {
		speaker = "Assistant"
	}
	entries := tr.Entries
	if opt.reply() {
		entries = conv.ReplyEntries(entries)
	}
	for _, e := range entries {
		var line map[string]any
		switch e.Kind {
		case conv.KindThinking:
			if !opt.Thinking {
				continue
			}
			line = entryLine(seq, "thinking", agent, strings.TrimSpace(e.Text), nil)
			line["speaker"] = speaker
		case conv.KindContext:
			if !opt.Context {
				continue
			}
			line = entryLine(seq, "context", agent, strings.TrimSpace(e.Text), nil)
			if e.Label != "" {
				line["label"] = e.Label
			}
		case conv.KindPrompt, conv.KindQueued:
			_, text := conv.SplitReminders(e.Text)
			text = strings.TrimSpace(conv.PromptDisplay(text))
			if text == "" && e.Images > 0 {
				text = fmt.Sprintf("(%d images)", e.Images)
			}
			if text == "" {
				continue
			}
			kind := "prompt"
			if e.Kind == conv.KindQueued {
				kind = "queued"
			}
			line = entryLine(seq, kind, agent, text, nil)
			line["speaker"] = "You"
		case conv.KindText:
			text := strings.TrimSpace(e.Text)
			if text == "" {
				continue
			}
			line = entryLine(seq, "text", agent, text, nil)
			who := e.Model
			if who == "" {
				who = speaker
			}
			line["speaker"] = who
		case conv.KindTool:
			if e.Call == nil {
				continue
			}
			line = entryLine(seq, "tool", agent, "", toolJSON(e.Call))
		case conv.KindNotice:
			text := strings.TrimSpace(e.Text)
			if text == "" {
				text = e.Label
			}
			if text == "" {
				continue
			}
			line = entryLine(seq, "note", agent, text, nil)
		default:
			continue
		}
		if !e.At.IsZero() {
			line["at"] = e.At.UTC().Format(time.RFC3339)
		}
		if err := enc.Encode(line); err != nil {
			return err
		}
	}
	return nil
}

func writeAgentsJSONL(enc *json.Encoder, seq int, nodes []query.AgentNode, logs map[string]*query.AgentLog, opt Options) error {
	for _, n := range nodes {
		if !opt.Agents {
			line := entryLine(seq, "agent", n.AgentID, "Started agent "+n.Label, nil)
			if err := enc.Encode(line); err != nil {
				return err
			}
		} else {
			line := entryLine(seq, "agent", n.AgentID, n.Label, nil)
			if err := enc.Encode(line); err != nil {
				return err
			}
			log := logs[n.AgentID]
			if log == nil || len(log.Turns) == 0 {
				if err := enc.Encode(entryLine(seq, "note", n.AgentID, "The transcript for this agent is gone.", nil)); err != nil {
					return err
				}
			} else {
				for i := range log.Turns {
					if err := writeTurnJSONL(enc, log.Turns[i].Seq, &log.Turns[i], n.AgentID, opt); err != nil {
						return err
					}
				}
			}
		}
		if err := writeAgentsJSONL(enc, seq, n.Children, logs, opt); err != nil {
			return err
		}
	}
	return nil
}

func (opt Options) reply() bool {
	return strings.EqualFold(strings.TrimSpace(opt.Detail), "reply")
}

func entryLine(seq int, kind, agent, text string, tool map[string]any) map[string]any {
	line := map[string]any{"type": "entry", "turn": seq, "kind": kind}
	if agent != "" {
		line["agent"] = agent
	}
	if text != "" {
		line["text"] = text
	}
	if tool != nil {
		line["tool"] = tool
	}
	return line
}

func toolJSON(c *conv.Call) map[string]any {
	m := map[string]any{"name": c.Name, "error": c.IsError, "done": c.Done}
	if c.Summary != "" {
		m["summary"] = c.Summary
	}
	if in := bytes.TrimSpace(c.Input); len(in) > 0 && string(in) != "null" {
		m["input"] = json.RawMessage(in)
	}
	if c.Output != "" {
		m["output"] = c.Output
	}
	if c.OutputLen > len(c.Output) {
		m["output_len"] = c.OutputLen
		m["truncated"] = true
	}
	return m
}

func inputText(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var buf bytes.Buffer
	if json.Indent(&buf, raw, "", "  ") == nil {
		return buf.String()
	}
	return string(raw)
}

func fence(s string) string {
	tick := "```"
	for strings.Contains(s, tick) {
		tick += "`"
	}
	return tick + "\n" + s + "\n" + tick
}
