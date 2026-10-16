package codex

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/lr00rl/ccli/internal/conv"
	"github.com/lr00rl/ccli/internal/transcript"
)

// Speaker names the model's side of a Codex conversation.
const Speaker = "Codex"

// ReadTurn reads one turn of a thread a person started: bytes [from, to)
// begin at the turn's task_started line (to <= 0: to the end). Turn 0 is
// what came before the first prompt; for a fork that is the history it
// copied, which is shown as one notice instead.
func ReadTurn(path string, from, to int64, seq int) (*conv.Turn, error) {
	turns, err := read(path, from, to, seq, false, true)
	if err != nil {
		return nil, err
	}
	if len(turns) == 0 {
		return &conv.Turn{Seq: seq, Offset: from, Speaker: Speaker}, nil
	}
	return &turns[0], nil
}

// ReadRange reads a whole subagent thread as turns, its first prompt being
// the task it was given.
func ReadRange(path string) ([]conv.Turn, error) {
	return read(path, 0, 0, 1, true, false)
}

func read(path string, from, to int64, seq int, sub, single bool) ([]conv.Turn, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := f.Seek(from, io.SeekStart); err != nil {
		return nil, err
	}
	var r io.Reader = f
	if to > 0 {
		if to <= from {
			return nil, nil
		}
		r = io.LimitReader(f, to-from)
	}
	rd := &reader{sub: sub, single: single, calls: map[string]*conv.Call{}, seq: seq}
	if single {
		rd.turns = []conv.Turn{{Seq: seq, Offset: from, Speaker: Speaker}}
	} else {
		rd.turns = []conv.Turn{{Seq: seq - 1, Offset: from, Speaker: Speaker}}
	}
	res := transcript.NewResult(transcript.State{})
	err = res.EachLine(r, from, rd.line)
	if single && seq == 0 && rd.forkedFrom != "" {
		rd.turns[0].Entries = []conv.Entry{{Kind: conv.KindNotice, Label: "forked",
			Text: fmt.Sprintf("This thread was forked from %s; the history it copied belongs to that thread.", rd.forkedFrom)}}
	}
	if !single && len(rd.turns) > 1 && len(rd.turns[0].Entries) == 0 {
		rd.turns = rd.turns[1:]
	}
	return rd.turns, err
}

type reader struct {
	turns      []conv.Turn
	calls      map[string]*conv.Call
	model      string
	sub        bool // a subagent: its first prompt is its task
	single     bool // one turn: everything goes in it
	seq        int
	prompts    int
	skipBelow  int64  // a fork's copied lines, by ordinal
	forkedFrom string // the thread a fork copied
	metaSeen   bool
}

func (rd *reader) cur() *conv.Turn { return &rd.turns[len(rd.turns)-1] }

func (rd *reader) add(e conv.Entry) { rd.cur().Entries = append(rd.cur().Entries, e) }

func (rd *reader) line(off int64, raw []byte) {
	var l envelope
	if json.Unmarshal(raw, &l) != nil {
		return
	}
	if l.Type == "session_meta" {
		if !rd.metaSeen {
			rd.metaSeen = true
			var sm sessionMeta
			if json.Unmarshal(l.Payload, &sm) == nil {
				rd.forkedFrom = sm.ForkedFromID
				if sm.SubagentHistoryStart != nil {
					rd.skipBelow = *sm.SubagentHistoryStart
				}
			}
		}
		return
	}
	if rd.skipBelow > 0 && l.Ordinal != nil && *l.Ordinal < rd.skipBelow {
		return
	}
	if rd.single && rd.seq == 0 && rd.forkedFrom != "" {
		return // turn 0 of a fork is the copied history
	}
	at := time.UnixMilli(transcript.ParseTS(l.Timestamp))
	switch l.Type {
	case "turn_context":
		var tc struct {
			Model string `json:"model"`
		}
		if json.Unmarshal(l.Payload, &tc) == nil && tc.Model != "" {
			rd.model = tc.Model
		}
	case "compacted":
		var c struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(l.Payload, &c) == nil && c.Message != "" {
			rd.add(conv.Entry{Kind: conv.KindNotice, At: at, Label: "compact summary", Text: c.Message})
		}
	case "event_msg":
		rd.event(off, at, l.Payload)
	case "response_item":
		rd.responseItem(off, at, l.Payload)
	}
}

// prompt adds what a person typed: the first prompt of a turn opens it, a
// later one was sent while the model worked. The same text arrives both as
// an event and as a message; it is added once.
func (rd *reader) prompt(off int64, at time.Time, text string, images int) {
	if p := rd.cur().Prompt(); p != nil && p.Text == text {
		return
	}
	for _, e := range rd.cur().Entries {
		if e.Kind == conv.KindQueued && e.Text == text {
			return
		}
	}
	kind, label := conv.KindPrompt, transcript.PromptHuman
	if rd.sub && rd.prompts == 0 {
		label = conv.KindAssigned
	}
	switch {
	case rd.single && rd.cur().Prompt() != nil:
		kind = conv.KindQueued
	case !rd.single:
		rd.turns = append(rd.turns, conv.Turn{Seq: rd.cur().Seq + 1, Kind: label, Offset: off, Speaker: Speaker})
	default:
		rd.cur().Kind = label
	}
	rd.prompts++
	rd.add(conv.Entry{Kind: kind, At: at, Label: label, Text: text, Images: images})
}

func (rd *reader) event(off int64, at time.Time, raw json.RawMessage) {
	var e typed
	if json.Unmarshal(raw, &e) != nil {
		return
	}
	switch e.Type {
	case "user_message", "item_completed":
		if text, images, ok := eventPrompt(e.Type, raw); ok {
			rd.prompt(off, at, text, images)
			return
		}
		if e.Type == "item_completed" {
			rd.item(at, raw)
		}
	case "task_complete":
		var tc struct {
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(raw, &tc) == nil && tc.Error != nil && tc.Error.Message != "" {
			rd.add(conv.Entry{Kind: conv.KindNotice, At: at, Label: "error", Text: tc.Error.Message})
		}
	case "turn_aborted":
		rd.add(conv.Entry{Kind: conv.KindNotice, At: at, Label: "interrupted", Text: "The turn was interrupted."})
	}
}

// item adds what code-mode `exec` ran (CLI 0.147 and later): commands, file
// changes and MCP calls are recorded only as completed items.
func (rd *reader) item(at time.Time, raw json.RawMessage) {
	var ic struct {
		Item struct {
			Type     string          `json:"type"`
			ID       string          `json:"id"`
			Command  json.RawMessage `json:"command"`
			ExitCode *int            `json:"exit_code"`
			Stdout   string          `json:"stdout"`
			Stderr   string          `json:"stderr"`
			Changes  json.RawMessage `json:"changes"`
			Server   string          `json:"server"`
			Tool     string          `json:"tool"`
			Status   string          `json:"status"`
		} `json:"item"`
	}
	if json.Unmarshal(raw, &ic) != nil {
		return
	}
	it := ic.Item
	c := &conv.Call{ID: it.ID, Done: true}
	switch it.Type {
	case "CommandExecution":
		c.Name, c.Input = "exec_command", it.Command
		c.Summary = commandLine(it.Command)
		out := strings.TrimRight(it.Stdout+it.Stderr, "\n")
		c.OutputLen, c.Output = len(out), capText(out, conv.MaxOutput)
		c.IsError = it.ExitCode != nil && *it.ExitCode != 0
	case "FileChange":
		c.Name, c.Input = "apply_patch", it.Changes
		c.Summary = changedFiles(it.Changes)
	case "McpToolCall":
		c.Name = "mcp__" + it.Server + "__" + it.Tool
		c.IsError = it.Status == "failed"
	default:
		return
	}
	rd.add(conv.Entry{Kind: conv.KindTool, At: at, Model: rd.model, Call: c})
}

func (rd *reader) responseItem(off int64, at time.Time, raw json.RawMessage) {
	var ri struct {
		Type      string          `json:"type"`
		Role      string          `json:"role"`
		Name      string          `json:"name"`
		Namespace string          `json:"namespace"`
		CallID    string          `json:"call_id"`
		Arguments string          `json:"arguments"`
		Input     string          `json:"input"`
		Output    json.RawMessage `json:"output"`
		Summary   []content       `json:"summary"`
		Author    string          `json:"author"`
		Content   []content       `json:"content"`
		Action    *struct {
			Query string `json:"query"`
			URL   string `json:"url"`
		} `json:"action"`
	}
	if json.Unmarshal(raw, &ri) != nil {
		return
	}
	switch ri.Type {
	case "message":
		var m message
		if json.Unmarshal(raw, &m) != nil {
			return
		}
		switch m.Role {
		case "user":
			if text, images, ok := m.typedText(); ok {
				rd.prompt(off, at, text, images)
				return
			}
			for _, c := range m.Content {
				if strings.TrimSpace(c.Text) != "" {
					rd.add(conv.Entry{Kind: conv.KindContext, At: at, Label: contextLabel(c.Text), Text: c.Text})
				}
			}
		case "developer":
			if text := joinText(m.Content); text != "" {
				rd.add(conv.Entry{Kind: conv.KindContext, At: at, Label: contextLabel(text), Text: text})
			}
		case "assistant":
			if text := joinText(m.Content); text != "" {
				rd.add(conv.Entry{Kind: conv.KindText, At: at, Model: rd.model, Text: text})
			}
		}
	case "reasoning":
		if text := joinText(ri.Summary); text != "" {
			rd.add(conv.Entry{Kind: conv.KindThinking, At: at, Model: rd.model, Text: text})
		}
	case "function_call", "custom_tool_call":
		name := toolName(ri.Namespace, ri.Name)
		input := json.RawMessage(ri.Arguments)
		if ri.Type == "custom_tool_call" || !json.Valid(input) {
			input, _ = json.Marshal(firstNonEmpty(ri.Input, ri.Arguments))
		}
		c := &conv.Call{ID: ri.CallID, Name: name, Input: input, Summary: callSummary(name, input)}
		rd.calls[ri.CallID] = c
		rd.add(conv.Entry{Kind: conv.KindTool, At: at, Model: rd.model, Call: c})
	case "function_call_output", "custom_tool_call_output":
		c := rd.calls[ri.CallID]
		if c == nil {
			c = &conv.Call{ID: ri.CallID, Name: "(result of an earlier call)"}
			rd.calls[ri.CallID] = c
			rd.add(conv.Entry{Kind: conv.KindTool, At: at, Call: c})
		}
		out := outputText(ri.Output)
		c.OutputLen, c.Output, c.Done = len(out), capText(out, conv.MaxOutput), true
	case "web_search_call":
		c := &conv.Call{Name: "web_search", Done: true}
		if ri.Action != nil {
			c.Summary = firstNonEmpty(ri.Action.Query, ri.Action.URL)
		}
		rd.add(conv.Entry{Kind: conv.KindTool, At: at, Model: rd.model, Call: c})
	case "agent_message":
		if text := joinText(ri.Content); text != "" {
			rd.add(conv.Entry{Kind: conv.KindContext, At: at, Label: "message from " + ri.Author, Text: text})
		}
	}
}

func joinText(cs []content) string {
	var parts []string
	for _, c := range cs {
		if t := strings.TrimSpace(c.Text); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n\n")
}

// outputText reads a tool output: a string, or a list of text parts.
func outputText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimRight(s, "\n")
	}
	var parts []content
	if json.Unmarshal(raw, &parts) == nil {
		return joinText(parts)
	}
	return ""
}

// contextLabel names injected text by the tag it starts with.
func contextLabel(text string) string {
	t := strings.TrimSpace(text)
	switch {
	case strings.HasPrefix(t, "# AGENTS.md"):
		return "AGENTS.md"
	case strings.HasPrefix(t, "<"):
		if i := strings.IndexAny(t, "> "); i > 1 {
			return strings.ReplaceAll(t[1:i], "_", " ")
		}
	}
	return "instructions"
}

// callSummary is a one-line description of a call: the command it ran,
// the files a patch changed, or its first meaningful argument.
func callSummary(name string, input json.RawMessage) string {
	if name == "apply_patch" {
		var patch string
		if json.Unmarshal(input, &patch) == nil {
			return patchFiles(patch)
		}
	}
	var args map[string]json.RawMessage
	if json.Unmarshal(input, &args) != nil {
		var s string
		if json.Unmarshal(input, &s) == nil {
			return firstLine(s)
		}
		return ""
	}
	for _, k := range []string{"cmd", "command", "path", "query", "message", "prompt", "task", "url", "agent_id"} {
		if v, ok := args[k]; ok {
			if s := commandLine(v); s != "" {
				return s
			}
		}
	}
	return ""
}

// commandLine reads a command given as a string or a list of words.
func commandLine(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return firstLine(s)
	}
	var words []string
	if json.Unmarshal(raw, &words) == nil {
		return firstLine(strings.Join(words, " "))
	}
	return ""
}

// patchFiles lists the files an apply_patch changes.
func patchFiles(patch string) string {
	var files []string
	for _, ln := range strings.Split(patch, "\n") {
		for _, p := range []string{"*** Add File: ", "*** Update File: ", "*** Delete File: "} {
			if strings.HasPrefix(ln, p) {
				files = append(files, strings.TrimSpace(strings.TrimPrefix(ln, p)))
			}
		}
	}
	return strings.Join(files, ", ")
}

// changedFiles lists the files a FileChange item names.
func changedFiles(raw json.RawMessage) string {
	var byPath map[string]json.RawMessage
	if json.Unmarshal(raw, &byPath) == nil {
		var files []string
		for p := range byPath {
			files = append(files, p)
		}
		return strings.Join(files, ", ")
	}
	return ""
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + " …"
	}
	return s
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

func capText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut]
}
