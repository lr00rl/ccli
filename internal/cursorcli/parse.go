// Package cursorcli reads Cursor CLI agent transcripts. They are JSONL
// files under ~/.cursor/projects/<slug>/agent-transcripts/. A line is a user
// or assistant message, or a turn_ended marker. The files carry no model, no
// token counts and no timestamps; the chat sidecar (meta.json) supplies the
// working directory and the clock. The package only ever reads.
package cursorcli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/lr00rl/ccli/internal/conv"
	"github.com/lr00rl/ccli/internal/transcript"
)

// Speaker names the model's side of a Cursor CLI conversation.
const Speaker = "Cursor"

var userQuery = regexp.MustCompile(`(?s)<user_query>\s*(.*?)\s*</user_query>`)

// Side is what the chat sidecar knows and the transcript does not.
type Side struct {
	Cwd     string
	Title   string
	Model   string
	Created int64
	Updated int64
}

// Parse reads one transcript. id is the conversation id. side carries the
// chat sidecar's cwd, title, model and clock; the JSONL itself has none of
// the first three, and its clock is the <timestamp> tag inside a prompt.
func Parse(r io.Reader, id string, side Side) (transcript.Result, error) {
	res, _, err := read(r, id, side, false)
	return res, err
}

// FindTurn returns the turn of ReadRange's result with this seq, or the one
// that starts at the index's byte offset, or nil.
func FindTurn(turns []conv.Turn, from int64, seq int) *conv.Turn {
	for i := range turns {
		if turns[i].Seq == seq || (from > 0 && turns[i].Offset == from) {
			t := turns[i]
			if t.Seq == 0 {
				t.Seq = seq
			}
			return &t
		}
	}
	return nil
}

// ReadRange reads a whole transcript, a subagent's included, as turns.
func ReadRange(path string) ([]conv.Turn, error) {
	return readFile(path, Side{}, true)
}

func readFile(path string, side Side, view bool) ([]conv.Turn, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	_, turns, err := read(f, "", side, view)
	return turns, err
}

type msg struct {
	off    int64
	role   string
	text   string
	query  string
	images int
	tools  []conv.Call
	ended  string
}

func read(r io.Reader, id string, side Side, view bool) (transcript.Result, []conv.Turn, error) {
	res := transcript.NewResult(transcript.State{})
	res.Cwd = side.Cwd
	res.CustomTitle = side.Title
	res.Entrypoint = "cursor"
	res.FirstTS, res.LastTS = side.Created, side.Updated
	b, err := io.ReadAll(r)
	if err != nil {
		return res, nil, err
	}
	var msgs []msg
	off := int64(0)
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		var line []byte
		if i < 0 {
			line, b = b, nil
		} else {
			line, b = b[:i], b[i+1:]
		}
		n := int64(len(line))
		if i >= 0 {
			n++
		}
		start := off
		off += n
		res.Consumed += n
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		res.Lines++
		m, ok := decode(start, line)
		if !ok {
			res.Skipped++
			continue
		}
		msgs = append(msgs, m)
	}
	var turns []conv.Turn
	cur := 0
	for _, m := range msgs {
		switch {
		case m.ended != "":
			if view {
				tail(&turns, cur, m.off).Entries = append(tail(&turns, cur, m.off).Entries, conv.Entry{
					Kind: conv.KindNotice, Label: "turn", Text: m.ended,
				})
			}
		case m.role == "user":
			text, kind := m.query, transcript.PromptHuman
			if text == "" {
				plain := strings.TrimSpace(m.text)
				if strings.Contains(plain, "<") {
					text, kind = plain, transcript.PromptOther
				} else {
					text = plain
				}
			}
			if kind == transcript.PromptOther && text == "" {
				continue
			}
			if kind == transcript.PromptHuman {
				cur++
				when := cursorWhen(m.text)
				if when == 0 {
					when = side.Created
				}
				if when > 0 {
					if res.FirstTS == 0 || when < res.FirstTS {
						res.FirstTS = when
					}
					if when > res.LastTS {
						res.LastTS = when
					}
				}
				keep := text
				if utf8.RuneCountInString(keep) > transcript.MaxPromptText {
					keep = string([]rune(keep)[:transcript.MaxPromptText])
				}
				res.Turns = append(res.Turns, transcript.Turn{
					Seq: cur, Offset: m.off, TS: when, Kind: kind,
					Text: keep, Chars: utf8.RuneCountInString(text), Images: m.images,
				})
				res.TypedPrompts++
				if res.FirstPrompt == "" {
					res.FirstPrompt = text
				}
				if name, ok := transcript.CommandName(text); ok {
					res.Commands[name]++
				}
				turns = append(turns, conv.Turn{Seq: cur, Offset: m.off, Speaker: Speaker, Entries: []conv.Entry{{
					Kind: conv.KindPrompt, Text: text, Label: kind, Images: m.images,
				}}})
			} else {
				res.OtherPrompts++
				tail(&turns, cur, m.off).Entries = append(tail(&turns, cur, m.off).Entries, conv.Entry{
					Kind: conv.KindContext, Label: "cursor", Text: clip(text),
				})
			}
		case m.role == "assistant":
			if id != "" {
				// A reply with no token counts: Cursor bills them on its usage page.
				res.Responses = append(res.Responses, transcript.Response{
					Key: "cursor:" + id + ":" + strconv.FormatInt(m.off, 10), Model: side.Model, TS: res.LastTS, Turn: cur,
				})
			}
			t := tail(&turns, cur, m.off)
			if m.text != "" {
				t.Entries = append(t.Entries, conv.Entry{Kind: conv.KindText, Text: m.text})
			}
			for i := range m.tools {
				c := m.tools[i]
				t.Entries = append(t.Entries, conv.Entry{Kind: conv.KindTool, Call: &c})
				res.ToolCalls++
				res.Tools[c.Name]++
				if cur > 0 {
					st := res.TurnStats[cur]
					if st == nil {
						st = &transcript.TurnStat{}
						res.TurnStats[cur] = st
					}
					st.ToolCalls++
				}
			}
		}
	}
	res.State.Turn = cur
	if side.Model != "" && id != "" {
		res.Responses = append(res.Responses, transcript.Response{
			Key: "cursor:" + id + ":model", Model: side.Model, TS: res.FirstTS, Turn: 1, Marker: true,
		})
	}
	if !view {
		turns = nil
	}
	return res, turns, nil
}

func tail(turns *[]conv.Turn, seq int, off int64) *conv.Turn {
	if len(*turns) == 0 || (*turns)[len(*turns)-1].Seq != seq {
		*turns = append(*turns, conv.Turn{Seq: seq, Offset: off, Speaker: Speaker})
	}
	return &(*turns)[len(*turns)-1]
}

func decode(off int64, line []byte) (msg, bool) {
	var raw struct {
		Role    string `json:"role"`
		Type    string `json:"type"`
		Status  string `json:"status"`
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &raw) != nil {
		return msg{}, false
	}
	if raw.Type == "turn_ended" {
		s := raw.Status
		if s == "" {
			s = "ended"
		}
		return msg{off: off, ended: s}, true
	}
	if raw.Role != "user" && raw.Role != "assistant" {
		return msg{}, false
	}
	text, images, tools := blocks(raw.Message.Content)
	m := msg{off: off, role: raw.Role, text: text, images: images, tools: tools}
	if raw.Role == "user" {
		if qs := userQuery.FindAllStringSubmatch(text, -1); len(qs) > 0 {
			var b strings.Builder
			for _, q := range qs {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(strings.TrimSpace(q[1]))
			}
			m.query = b.String()
		}
	}
	return m, true
}

func blocks(raw json.RawMessage) (text string, images int, tools []conv.Call) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return "", 0, nil
	}
	if raw[0] == '"' {
		var s string
		_ = json.Unmarshal(raw, &s)
		return s, 0, nil
	}
	var arr []struct {
		Type  string          `json:"type"`
		Text  string          `json:"text"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	}
	if json.Unmarshal(raw, &arr) != nil {
		return "", 0, nil
	}
	var b strings.Builder
	for i, bl := range arr {
		switch bl.Type {
		case "text":
			if b.Len() > 0 && bl.Text != "" {
				b.WriteByte('\n')
			}
			b.WriteString(bl.Text)
		case "image":
			images++
		case "tool_use":
			tools = append(tools, conv.Call{ID: fmt.Sprintf("%s:%d", bl.Name, i), Name: bl.Name, Input: bl.Input, Summary: summarize(bl.Name, bl.Input)})
		}
	}
	return b.String(), images, tools
}

func summarize(name string, args json.RawMessage) string {
	var m map[string]any
	if json.Unmarshal(args, &m) == nil {
		for _, k := range []string{"command", "cmd", "path", "file_path", "pattern", "query", "url"} {
			if s, ok := m[k].(string); ok && s != "" {
				s = strings.Join(strings.Fields(s), " ")
				if utf8.RuneCountInString(s) > 80 {
					s = string([]rune(s)[:80])
				}
				return name + " " + s
			}
		}
	}
	return name
}

func clip(s string) string {
	if len(s) > conv.MaxOutput {
		return s[:conv.MaxOutput]
	}
	return s
}

var stamp = regexp.MustCompile(`(?s)<timestamp>\s*(.*?)\s*</timestamp>`)

// cursorWhen reads the clock Cursor writes into a prompt. The shape is
// "Wednesday, Sep 23, 2026, 3:19 AM (UTC-7)".
func cursorWhen(text string) int64 {
	m := stamp.FindStringSubmatch(text)
	if m == nil {
		return 0
	}
	s := strings.TrimSpace(m[1])
	// Without a zone ccli can read, the hour is unknown: the caller falls
	// back to the chat's own clock rather than guess UTC.
	i := strings.LastIndex(s, "(")
	if i < 0 {
		return 0
	}
	z := strings.Trim(s[i:], "() \t")
	s = strings.TrimSpace(s[:i])
	off, ok := utcOffset(z)
	if !ok || !strings.HasPrefix(z, "UTC") {
		return 0
	}
	zone := time.FixedZone(z, off)
	if i := strings.Index(s, ","); i >= 0 {
		s = strings.TrimSpace(s[i+1:])
	}
	t, err := time.ParseInLocation("Jan 2, 2006, 3:04 PM", s, zone)
	if err != nil {
		return 0
	}
	return t.UnixMilli()
}

func utcOffset(z string) (int, bool) {
	z = strings.TrimPrefix(z, "UTC")
	if z == "" || z == "Z" {
		return 0, true
	}
	sign := 1
	switch z[0] {
	case '-':
		sign = -1
		z = z[1:]
	case '+':
		z = z[1:]
	default:
		return 0, false
	}
	hours, err := strconv.Atoi(z)
	mins := 0
	if i := strings.IndexByte(z, ':'); i >= 0 {
		hours, err = strconv.Atoi(z[:i])
		if err != nil {
			return 0, false
		}
		mins, err = strconv.Atoi(z[i+1:])
	}
	if err != nil {
		return 0, false
	}
	return sign * (hours*3600 + mins*60), true
}
