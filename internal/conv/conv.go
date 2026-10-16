// Package conv reads a stretch of a transcript as a conversation: prompts,
// the model's text and thinking, tool calls paired with their results, and
// notices. It is what the turn views show; the index only keeps where each
// turn starts.
package conv

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"hash/fnv"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/lr00rl/ccli/internal/transcript"
)

// MaxOutput is the most of a tool result kept; OutputLen has the full size.
var MaxOutput = 32 << 10

// Entry kinds.
const (
	KindPrompt   = "prompt"   // a prompt that starts a turn
	KindQueued   = "queued"   // a message delivered while the model was working
	KindText     = "text"     // the model's text
	KindThinking = "thinking" // the model's thinking
	KindTool     = "tool"     // a tool call and, once seen, its result
	KindContext  = "context"  // text Claude Code added to the conversation (skill bodies, peer messages, reminders)
	KindNotice   = "notice"   // a system event: compaction, an API error, a summary

	// KindAssigned labels the first prompt of a subagent: the task its parent
	// gave it.
	KindAssigned = "assigned"
)

// Entry is one thing in the conversation, in transcript order.
type Entry struct {
	Kind   string    `json:"kind"`
	At     time.Time `json:"at,omitempty"`
	Text   string    `json:"text,omitempty"`
	Label  string    `json:"label,omitempty"`  // prompt kind, notice subtype, context source
	Model  string    `json:"model,omitempty"`  // the model's entries
	Images int       `json:"images,omitempty"` // prompts
	Call   *Call     `json:"call,omitempty"`
}

// Call is a tool call with its result.
type Call struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Summary   string          `json:"summary"` // one line
	Input     json.RawMessage `json:"input,omitempty"`
	Output    string          `json:"output,omitempty"`
	OutputLen int             `json:"output_len"`
	IsError   bool            `json:"is_error,omitempty"`
	Done      bool            `json:"done"`             // a result was seen
	Target    string          `json:"target,omitempty"` // agent id, run id or "teammate:<name>"
	Prompt    string          `json:"prompt,omitempty"` // Agent: the prompt it was given
}

// Turn is a prompt and everything that followed it, up to the next prompt.
type Turn struct {
	Seq     int     `json:"seq"` // 0 for what came before the first prompt
	Kind    string  `json:"kind,omitempty"`
	Offset  int64   `json:"offset"`
	Entries []Entry `json:"entries"`
	// Speaker names the model's side of the conversation; empty means Claude.
	Speaker string `json:"speaker,omitempty"`
}

// Prompt returns the turn's prompt entry, if it has one.
func (t *Turn) Prompt() *Entry {
	for i := range t.Entries {
		if t.Entries[i].Kind == KindPrompt {
			return &t.Entries[i]
		}
	}
	return nil
}

// Calls returns the turn's tool calls.
func (t *Turn) Calls() []*Call {
	var out []*Call
	for i := range t.Entries {
		if c := t.Entries[i].Call; c != nil {
			out = append(out, c)
		}
	}
	return out
}

// ReadRange reads bytes [from, to) of a transcript (to <= 0 means to the
// end) as turns. The first turn has Seq firstSeq-1 when the range does not
// start at a prompt; sub is true for a subagent's transcript, whose first
// prompt is its task and whose sidechain lines belong to it.
func ReadRange(path string, from, to int64, firstSeq int, sub bool) ([]Turn, error) {
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
			return nil, nil // an empty range, not "to the end"
		}
		r = io.LimitReader(f, to-from)
	}
	return Read(r, from, firstSeq, sub)
}

type rawLine struct {
	Type             string `json:"type"`
	Subtype          string `json:"subtype"`
	Timestamp        string `json:"timestamp"`
	IsMeta           bool   `json:"isMeta"`
	IsSidechain      bool   `json:"isSidechain"`
	IsCompactSummary bool   `json:"isCompactSummary"`
	PromptSource     string `json:"promptSource"`
	Content          string `json:"content"` // system lines
	Level            string `json:"level"`
	Origin           *struct {
		Kind string `json:"kind"`
	} `json:"origin"`
	Attachment    json.RawMessage `json:"attachment"`
	ToolUseResult json.RawMessage `json:"toolUseResult"`
	Message       struct {
		ID      string          `json:"id"`
		Model   string          `json:"model"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type block struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	Input     json.RawMessage `json:"input"`
	Content   json.RawMessage `json:"content"`
}

type reader struct {
	turns   []Turn
	calls   map[string]*Call
	seen    map[string]bool // assistant content blocks already added (a response spans several lines)
	sub     bool
	prompts int
	single  bool // the range is one turn of a main transcript
	replay  bool // in a single turn, past a second prompt: a stretch Claude Code wrote again
}

// ReadTurn reads one turn of a main transcript: bytes [from, to) hold its
// prompt and what followed (to <= 0: to the end). A later prompt inside the
// range can only start a stretch Claude Code wrote again on resume, since
// the index starts no turn there; it is shown as one notice.
func ReadTurn(path string, from, to int64, seq int) (*Turn, error) {
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
			return &Turn{Seq: seq, Offset: from}, nil // an empty range, not "to the end"
		}
		r = io.LimitReader(f, to-from)
	}
	// Turn 0 (before the first prompt) is read from the start of the file;
	// any other range starts at the prompt that opens it.
	first := seq + 1
	if seq > 0 {
		first = seq
	}
	turns, err := read(r, from, first, false, true)
	if err != nil {
		return nil, err
	}
	for i := range turns {
		if turns[i].Seq == seq {
			return &turns[i], nil
		}
	}
	if len(turns) > 0 {
		return &turns[0], nil
	}
	return &Turn{Seq: seq, Offset: from}, nil
}

// Read parses transcript lines from r; base is the file offset r starts at.
func Read(r io.Reader, base int64, firstSeq int, sub bool) ([]Turn, error) {
	return read(r, base, firstSeq, sub, false)
}

func read(r io.Reader, base int64, firstSeq int, sub, single bool) ([]Turn, error) {
	rd := &reader{calls: map[string]*Call{}, seen: map[string]bool{}, sub: sub, single: single}
	rd.turns = []Turn{{Seq: firstSeq - 1, Offset: base}}
	br := bufio.NewReaderSize(r, 1<<20)
	off := base
	for {
		ln, err := br.ReadBytes('\n')
		if len(ln) > 0 {
			// A partial last line (still being written) fails to parse and is
			// left out.
			rd.line(bytes.TrimSpace(ln), off)
			off += int64(len(ln))
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
	}
	turns := rd.turns
	if len(turns) > 1 && len(turns[0].Entries) == 0 {
		turns = turns[1:]
	}
	return turns, nil
}

func (rd *reader) cur() *Turn { return &rd.turns[len(rd.turns)-1] }

func (rd *reader) add(e Entry) { rd.cur().Entries = append(rd.cur().Entries, e) }

func (rd *reader) line(raw []byte, off int64) {
	if len(raw) == 0 || rd.replay {
		return
	}
	var l rawLine
	if json.Unmarshal(raw, &l) != nil {
		return
	}
	if l.IsSidechain && !rd.sub {
		return // subagent lines that old transcripts kept inline
	}
	at := parseTS(l.Timestamp)
	switch l.Type {
	case "user":
		rd.user(l, at, off)
	case "assistant":
		rd.assistant(l, at)
	case "system":
		rd.system(l, at)
	case "attachment":
		rd.attachment(l, at)
	}
}

func (rd *reader) user(l rawLine, at time.Time, off int64) {
	c := bytes.TrimSpace(l.Message.Content)
	if len(c) == 0 {
		return
	}
	if c[0] == '[' {
		var bs []block
		if json.Unmarshal(c, &bs) == nil {
			results := false
			var rest []block
			for _, b := range bs {
				if b.Type == "tool_result" {
					results = true
					rd.result(b, l.ToolUseResult)
				} else {
					rest = append(rest, b)
				}
			}
			if results {
				// Text sent along with tool results starts no turn (the index
				// starts none there either); show it as context.
				if len(rest) > 0 {
					if raw, err := json.Marshal(rest); err == nil {
						if text, _ := contentText(raw); strings.TrimSpace(text) != "" {
							rd.add(Entry{Kind: KindContext, At: at, Label: "sent with tool results", Text: text})
						}
					}
				}
				return
			}
		}
	}
	text, images := contentText(c)
	if l.IsCompactSummary {
		rd.add(Entry{Kind: KindNotice, At: at, Label: "compact summary", Text: text})
		return
	}
	if l.IsMeta {
		if strings.TrimSpace(text) != "" {
			rd.add(Entry{Kind: KindContext, At: at, Label: contextLabel(l, text), Text: text})
		}
		return
	}
	origin := ""
	if l.Origin != nil {
		origin = l.Origin.Kind
	}
	kind := transcript.PromptKind(origin, l.PromptSource, text)
	if kind == "" {
		if s := strings.TrimSpace(text); s != "" {
			rd.add(Entry{Kind: KindContext, At: at, Label: "command output", Text: stripTags(s)})
		}
		return
	}
	if rd.sub && rd.prompts == 0 {
		kind = KindAssigned
	}
	if rd.single && rd.prompts > 0 {
		rd.add(Entry{Kind: KindNotice, At: at, Label: "repeated", Text: "Claude Code wrote earlier turns of the conversation again here, when the session was resumed. They are shown where they first happened."})
		rd.replay = true
		return
	}
	rd.prompts++
	rd.turns = append(rd.turns, Turn{Seq: rd.cur().Seq + 1, Kind: kind, Offset: off})
	rd.add(Entry{Kind: KindPrompt, At: at, Label: kind, Text: text, Images: images})
}

func (rd *reader) result(b block, extra json.RawMessage) {
	c := rd.calls[b.ToolUseID]
	if c == nil {
		// The call is in an earlier turn than this read covers.
		c = &Call{ID: b.ToolUseID, Name: "(result of an earlier call)"}
		rd.add(Entry{Kind: KindTool, Call: c})
		rd.calls[b.ToolUseID] = c
	}
	out, _ := contentText(b.Content)
	c.OutputLen = len(out)
	c.Output = capText(out, MaxOutput)
	c.IsError = b.IsError
	c.Done = true
	if len(extra) > 0 && extra[0] == '{' {
		var r struct {
			AgentID  string `json:"agentId"`
			RunID    string `json:"runId"`
			Teammate string `json:"teammate_id"`
			Name     string `json:"name"`
		}
		if json.Unmarshal(extra, &r) == nil {
			switch {
			case r.RunID != "":
				c.Target = r.RunID
			case r.AgentID != "":
				c.Target = r.AgentID
			case r.Teammate != "":
				name := r.Name
				if name == "" {
					name, _, _ = strings.Cut(r.Teammate, "@")
				}
				c.Target = "teammate:" + name
			}
		}
	}
}

func (rd *reader) assistant(l rawLine, at time.Time) {
	var bs []block
	if json.Unmarshal(l.Message.Content, &bs) != nil {
		return
	}
	for _, b := range bs {
		// Claude Code writes one response over several lines, usually one
		// content block per line; a block repeated on a later line is shown once.
		key := ""
		if l.Message.ID != "" {
			h := fnv.New64a()
			h.Write([]byte(b.Text))
			h.Write([]byte(b.Thinking))
			key = l.Message.ID + "\x00" + b.Type + "\x00" + strconv.FormatUint(h.Sum64(), 36)
		}
		switch b.Type {
		case "text":
			if strings.TrimSpace(b.Text) == "" || key != "" && rd.seen[key] {
				continue
			}
			rd.mark(key)
			rd.add(Entry{Kind: KindText, At: at, Text: b.Text, Model: l.Message.Model})
		case "thinking":
			if strings.TrimSpace(b.Thinking) == "" || key != "" && rd.seen[key] {
				continue
			}
			rd.mark(key)
			rd.add(Entry{Kind: KindThinking, At: at, Text: b.Thinking, Model: l.Message.Model})
		case "tool_use":
			if b.ID == "" || rd.calls[b.ID] != nil {
				continue
			}
			c := &Call{ID: b.ID, Name: b.Name, Input: b.Input, Summary: Summary(b.Name, b.Input)}
			if b.Name == "Agent" || b.Name == "Task" {
				var in struct {
					Prompt string `json:"prompt"`
				}
				if json.Unmarshal(b.Input, &in) == nil {
					c.Prompt = in.Prompt
				}
			}
			rd.calls[b.ID] = c
			rd.add(Entry{Kind: KindTool, At: at, Model: l.Message.Model, Call: c})
		}
	}
}

func (rd *reader) mark(key string) {
	if key != "" {
		rd.seen[key] = true
	}
}

// Notices worth showing; the rest of Claude Code's system lines are
// bookkeeping.
var notices = map[string]string{
	"compact_boundary":    "context compacted",
	"api_error":           "API error",
	"away_summary":        "recap",
	"informational":       "info",
	"scheduled_task_fire": "scheduled task",
	"agents_killed":       "agents stopped",
	"local_command":       "command",
}

func (rd *reader) system(l rawLine, at time.Time) {
	label, ok := notices[l.Subtype]
	if !ok {
		return
	}
	text := strings.TrimSpace(stripTags(l.Content))
	if text == "" && l.Subtype != "compact_boundary" {
		return
	}
	rd.add(Entry{Kind: KindNotice, At: at, Label: label, Text: text})
}

func (rd *reader) attachment(l rawLine, at time.Time) {
	if len(l.Attachment) == 0 {
		return
	}
	var a struct {
		Type     string `json:"type"`
		Prompt   any    `json:"prompt"`
		Filename string `json:"filename"`
		Mode     string `json:"commandMode"`
	}
	if json.Unmarshal(l.Attachment, &a) != nil {
		return
	}
	switch a.Type {
	case "queued_command":
		text := ""
		switch p := a.Prompt.(type) {
		case string:
			text = p
		case []any:
			b, _ := json.Marshal(p)
			text, _ = contentText(b)
		}
		if strings.TrimSpace(text) == "" {
			return
		}
		label := a.Mode
		if strings.HasPrefix(strings.TrimSpace(text), "<task-notification>") {
			label = "task"
		}
		rd.add(Entry{Kind: KindQueued, At: at, Label: label, Text: text})
	case "file":
		if a.Filename != "" {
			rd.add(Entry{Kind: KindContext, At: at, Label: "file", Text: a.Filename})
		}
	}
}

func contextLabel(l rawLine, text string) string {
	s := strings.TrimSpace(text)
	switch {
	case l.Origin != nil && l.Origin.Kind == "peer":
		return "peer message"
	case strings.HasPrefix(s, "Base directory for this skill:"), strings.Contains(s, "<command-name>"):
		return "skill"
	case strings.HasPrefix(s, "<system-reminder>"):
		return "reminder"
	}
	return "context"
}

// contentText returns the text of a message content (a string or blocks)
// and how many images or documents it holds.
func contentText(c json.RawMessage) (string, int) {
	c = bytes.TrimSpace(c)
	if len(c) == 0 {
		return "", 0
	}
	if c[0] == '"' {
		var s string
		json.Unmarshal(c, &s)
		return s, 0
	}
	var bs []block
	if json.Unmarshal(c, &bs) != nil {
		return "", 0
	}
	var parts []string
	images := 0
	for _, b := range bs {
		switch b.Type {
		case "text":
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		case "image", "document":
			images++
		case "tool_reference":
			parts = append(parts, "["+b.Name+"]")
		}
	}
	return strings.Join(parts, "\n\n"), images
}

var tagRE = regexp.MustCompile(`</?[a-z][a-z0-9-]*(?:\s[^>]*)?>`)

func stripTags(s string) string { return strings.TrimSpace(tagRE.ReplaceAllString(s, "")) }

func parseTS(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func capText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
