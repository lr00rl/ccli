// Package pi reads Pi coding-agent session files. A session is one JSONL
// file under <agent-dir>/sessions/, and the entries form a tree: the
// conversation Pi shows is the path from the latest leaf back to the root,
// while every assistant reply still counts, because an abandoned branch
// spent the tokens. Pi prices each reply itself; that USD figure is kept.
// The package only ever reads.
package pi

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/lr00rl/ccli/internal/conv"
	"github.com/lr00rl/ccli/internal/transcript"
)

// Speaker names the model's side of a Pi conversation.
const Speaker = "Pi"

// Header is the first line of a session file: its id and the directory it
// started in. A file that does not begin with a session line is skipped.
func Header(path string) (id, cwd string, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", false
	}
	defer f.Close()
	br := bufio.NewReader(f)
	line, err := br.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return "", "", false
	}
	var v struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Cwd  string `json:"cwd"`
	}
	if json.Unmarshal(bytes.TrimSpace(line), &v) != nil || v.Type != "session" || v.ID == "" {
		return "", "", false
	}
	return v.ID, v.Cwd, true
}

// Parse reads one session file from r. st.Base is the file offset r starts
// at; a Pi file is re-read from the start whenever it changes, because a new
// branch changes which leaf is current.
func Parse(r io.Reader, session string, st transcript.State) (transcript.Result, error) {
	res, _, err := read(r, session, st.Base, false)
	return res, err
}

// FindTurn returns the turn of ReadRange's result whose seq matches, or nil.
// The index's byte offset is not used: the active branch, not the bytes
// between two prompts, decides what belongs to a turn.
func FindTurn(turns []conv.Turn, _ int64, seq int) *conv.Turn {
	for i := range turns {
		if turns[i].Seq == seq {
			return &turns[i]
		}
	}
	return nil
}

// ReadRange reads the active branch of a session as turns.
func ReadRange(path string) ([]conv.Turn, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	_, turns, err := read(f, "", 0, true)
	return turns, err
}

type rec struct {
	off        int64
	id         string
	respID     string
	parent     string
	kind       string
	ts         int64
	role       string
	text       string
	think      string
	images     int
	model      string
	usage      *use
	title      string
	cwd        string
	sid        string
	parentFile string
	tools      []conv.Call
	toolID     string
	toolName   string
	isErr      bool
	summary    string
}

type use struct {
	in, out, cr, cw, c1 int64
	cost                float64
	hasCost             bool
}

func read(r io.Reader, session string, base int64, view bool) (transcript.Result, []conv.Turn, error) {
	res := transcript.NewResult(transcript.State{})
	lines, err := linesOf(r, base, &res)
	if err != nil {
		return res, nil, err
	}
	var recs []rec
	for _, ln := range lines {
		rc, ok := decode(ln.off, ln.body)
		if !ok {
			res.Skipped++
			continue
		}
		recs = append(recs, rc)
	}
	header, tree := split(recs)
	if header.cwd != "" {
		res.Cwd = header.cwd
	}
	if header.id != "" {
		session = header.id
	}
	if id := fileSessionID(header.parentFile); id != "" {
		res.ContinuedIn = append(res.ContinuedIn, id)
	}
	res.Entrypoint = "pi"
	if header.ts > 0 {
		res.FirstTS = header.ts
	}
	active := activeSet(tree)
	var turns []conv.Turn
	cur := 0
	var prev int64
	calls := map[string]*conv.Call{}
	var model string
	for i, rc := range tree {
		on := active[i]
		if rc.kind == "session_info" && on && rc.title != "" {
			res.CustomTitle = rc.title
		}
		if rc.usage != nil && rc.kind != "message" {
			addUsage(&res, session, rc, cur, model)
		}
		if !on {
			if rc.kind == "message" && rc.role == "assistant" && rc.usage != nil {
				addUsage(&res, session, rc, 0, model)
			}
			continue
		}
		if rc.ts > 0 {
			if res.FirstTS == 0 || rc.ts < res.FirstTS {
				res.FirstTS = rc.ts
			}
			if rc.ts > res.LastTS {
				res.LastTS = rc.ts
			}
			noteActive(&res, cur, prev, rc.ts)
			prev = rc.ts
		}
		if rc.kind == "model_change" && rc.model != "" {
			model = rc.model
		}
		switch rc.kind {
		case "message":
			switch rc.role {
			case "user":
				cur++
				text := strings.TrimSpace(rc.text)
				turns = append(turns, conv.Turn{Seq: cur, Offset: rc.off, Speaker: Speaker, Entries: []conv.Entry{{
					Kind: conv.KindPrompt, At: ms(rc.ts), Text: text, Label: transcript.PromptHuman, Images: rc.images,
				}}})
				keep := text
				if utf8.RuneCountInString(keep) > transcript.MaxPromptText {
					keep = string([]rune(keep)[:transcript.MaxPromptText])
				}
				res.Turns = append(res.Turns, transcript.Turn{
					Seq: cur, Offset: rc.off, PromptID: rc.id, TS: rc.ts, Kind: transcript.PromptHuman,
					Text: keep, Chars: utf8.RuneCountInString(text), Images: rc.images,
				})
				res.TypedPrompts++
				if res.FirstPrompt == "" {
					res.FirstPrompt = text
				}
				if name, ok := transcript.CommandName(text); ok {
					res.Commands[name]++
				}
				stat(res.TurnStats, cur).LastTS = rc.ts
			case "assistant":
				if rc.model != "" {
					model = rc.model
				}
				addUsage(&res, session, rc, cur, model)
				t := tail(&turns, cur, rc.off)
				if rc.think != "" {
					t.Entries = append(t.Entries, conv.Entry{Kind: conv.KindThinking, At: ms(rc.ts), Text: rc.think, Model: model})
				}
				if rc.text != "" {
					t.Entries = append(t.Entries, conv.Entry{Kind: conv.KindText, At: ms(rc.ts), Text: rc.text, Model: model})
				}
				for i := range rc.tools {
					c := rc.tools[i]
					cp := c
					calls[c.ID] = &cp
					// store the pointer we appended
					t.Entries = append(t.Entries, conv.Entry{Kind: conv.KindTool, At: ms(rc.ts), Model: model, Call: &cp})
					calls[c.ID] = t.Entries[len(t.Entries)-1].Call
					res.ToolCalls++
					res.Tools[c.Name]++
					stat(res.TurnStats, cur).ToolCalls++
				}
			case "toolResult":
				if c := calls[rc.toolID]; c != nil {
					c.Output, c.OutputLen, c.IsError, c.Done = clip(rc.text), len(rc.text), rc.isErr, true
				}
			case "system":
				// System messages are Pi's prompt and tool list, not a turn.
			}
		case "compaction":
			tail(&turns, cur, rc.off).Entries = append(tail(&turns, cur, rc.off).Entries, conv.Entry{
				Kind: conv.KindNotice, At: ms(rc.ts), Label: "compaction", Text: rc.summary,
			})
		case "branch_summary":
			tail(&turns, cur, rc.off).Entries = append(tail(&turns, cur, rc.off).Entries, conv.Entry{
				Kind: conv.KindNotice, At: ms(rc.ts), Label: "branch", Text: rc.summary,
			})
		}
	}
	res.State.Turn = cur
	res.State.PrevTS = prev
	if view {
		return res, turns, nil
	}
	return res, nil, nil
}

func addUsage(res *transcript.Result, session string, rc rec, turn int, model string) {
	if rc.usage == nil {
		return
	}
	u := rc.usage
	m := rc.model
	if m == "" {
		m = model
	}
	key := rc.respID
	if key == "" {
		key = rc.id
	}
	if key == "" {
		key = strconv.FormatInt(rc.off, 10)
	}
	c1 := u.c1
	if c1 > u.cw {
		c1 = u.cw
	}
	res.Responses = append(res.Responses, transcript.Response{
		Key: "pi:" + session + ":" + key, TS: rc.ts, Model: m, Turn: turn,
		Usage: transcript.Usage{
			Input: u.in, Output: u.out, CacheRead: u.cr, Cache5m: u.cw - c1, Cache1h: c1,
		},
		Cost: u.cost, HasCost: u.hasCost,
	})
}

func tail(turns *[]conv.Turn, seq int, off int64) *conv.Turn {
	if len(*turns) == 0 || (*turns)[len(*turns)-1].Seq != seq {
		*turns = append(*turns, conv.Turn{Seq: seq, Offset: off, Speaker: Speaker})
	}
	return &(*turns)[len(*turns)-1]
}

func stat(m map[int]*transcript.TurnStat, seq int) *transcript.TurnStat {
	s := m[seq]
	if s == nil {
		s = &transcript.TurnStat{}
		m[seq] = s
	}
	return s
}

func noteActive(res *transcript.Result, turn int, prev, ts int64) {
	if prev <= 0 || ts <= prev {
		return
	}
	d := ts - prev
	if d > transcript.ActiveGap.Milliseconds() {
		return
	}
	res.ActiveMS += d
	hour := ts - ts%(3600*1000)
	res.Activity[hour] += d
	if turn > 0 {
		stat(res.TurnStats, turn).ActiveMS += d
		stat(res.TurnStats, turn).LastTS = ts
	}
}

func split(recs []rec) (header rec, tree []rec) {
	for _, rc := range recs {
		if rc.kind == "session" && header.kind == "" {
			header = rc
			continue
		}
		if rc.kind == "session" {
			continue
		}
		tree = append(tree, rc)
	}
	return header, tree
}

// activeSet marks the path from the last leaf to its root. A leaf is an
// entry nothing else points at; Pi appends, so the last leaf is the one the
// conversation is on.
func activeSet(tree []rec) map[int]bool {
	idx := map[string]int{}
	kids := make([]int, len(tree))
	for i, rc := range tree {
		if rc.id != "" {
			idx[rc.id] = i
		}
	}
	for _, rc := range tree {
		if j, ok := idx[rc.parent]; ok {
			kids[j]++
		}
	}
	leaf := -1
	for i, rc := range tree {
		if rc.id != "" && kids[i] == 0 {
			leaf = i
		}
	}
	active := map[int]bool{}
	if leaf < 0 {
		for i := range tree {
			active[i] = true
		}
		return active
	}
	for i := leaf; i >= 0; {
		if active[i] {
			break
		}
		active[i] = true
		j, ok := idx[tree[i].parent]
		if !ok {
			break
		}
		i = j
	}
	return active
}

func decode(off int64, body []byte) (rec, bool) {
	var raw struct {
		Type          string          `json:"type"`
		ID            string          `json:"id"`
		ParentID      *string         `json:"parentId"`
		Timestamp     string          `json:"timestamp"`
		Cwd           string          `json:"cwd"`
		ParentSession string          `json:"parentSession"`
		Name          string          `json:"name"`
		Provider      string          `json:"provider"`
		ModelID       string          `json:"modelId"`
		Model         string          `json:"model"`
		Summary       string          `json:"summary"`
		Kind          string          `json:"kind"`
		Usage         *rawUse         `json:"usage"`
		Message       json.RawMessage `json:"message"`
	}
	if json.Unmarshal(body, &raw) != nil || raw.Type == "" {
		return rec{}, false
	}
	rc := rec{off: off, id: raw.ID, kind: raw.Type, ts: iso(raw.Timestamp), cwd: raw.Cwd, title: raw.Name, summary: raw.Summary, parentFile: raw.ParentSession}
	if raw.ParentID != nil {
		rc.parent = *raw.ParentID
	}
	if raw.Type == "model_change" {
		rc.model = joinModel(raw.Provider, raw.ModelID)
	}
	if raw.Usage != nil {
		rc.usage = raw.Usage.conv()
		rc.model = joinModel(raw.Provider, raw.Model)
	}
	if len(raw.Message) == 0 || string(raw.Message) == "null" {
		return rc, true
	}
	var msg struct {
		Role       string          `json:"role"`
		Content    json.RawMessage `json:"content"`
		Timestamp  int64           `json:"timestamp"`
		Provider   string          `json:"provider"`
		Model      string          `json:"model"`
		Usage      *rawUse         `json:"usage"`
		ResponseID string          `json:"responseId"`
		ToolCallID string          `json:"toolCallId"`
		ToolName   string          `json:"toolName"`
		IsError    bool            `json:"isError"`
	}
	if json.Unmarshal(raw.Message, &msg) != nil {
		return rc, true
	}
	rc.role = msg.Role
	if msg.Timestamp > 0 {
		rc.ts = msg.Timestamp
	}
	rc.model = joinModel(msg.Provider, msg.Model)
	rc.respID = msg.ResponseID
	if msg.Usage != nil {
		rc.usage = msg.Usage.conv()
	}
	rc.text, rc.think, rc.images, rc.tools = content(msg.Content)
	rc.toolID, rc.toolName, rc.isErr = msg.ToolCallID, msg.ToolName, msg.IsError
	return rc, true
}

type rawUse struct {
	Input        int64 `json:"input"`
	Output       int64 `json:"output"`
	CacheRead    int64 `json:"cacheRead"`
	CacheWrite   int64 `json:"cacheWrite"`
	CacheWrite1h int64 `json:"cacheWrite1h"`
	Cost         *struct {
		Total float64 `json:"total"`
	} `json:"cost"`
}

func (u *rawUse) conv() *use {
	if u == nil {
		return nil
	}
	out := &use{in: u.Input, out: u.Output, cr: u.CacheRead, cw: u.CacheWrite, c1: u.CacheWrite1h}
	if u.Cost != nil {
		out.cost, out.hasCost = u.Cost.Total, true
	}
	return out
}

func content(raw json.RawMessage) (text, think string, images int, tools []conv.Call) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return "", "", 0, nil
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s, "", 0, nil
		}
	}
	var blocks []struct {
		Type      string          `json:"type"`
		Text      string          `json:"text"`
		Thinking  string          `json:"thinking"`
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return "", "", 0, nil
	}
	var b strings.Builder
	var th strings.Builder
	for _, bl := range blocks {
		switch bl.Type {
		case "text":
			if b.Len() > 0 && bl.Text != "" {
				b.WriteByte('\n')
			}
			b.WriteString(bl.Text)
		case "image":
			images++
		case "thinking":
			if th.Len() > 0 && bl.Thinking != "" {
				th.WriteByte('\n')
			}
			th.WriteString(bl.Thinking)
		case "toolCall":
			tools = append(tools, conv.Call{ID: bl.ID, Name: bl.Name, Input: bl.Arguments, Summary: summarize(bl.Name, bl.Arguments)})
		}
	}
	return b.String(), th.String(), images, tools
}

func summarize(name string, args json.RawMessage) string {
	var m map[string]any
	if json.Unmarshal(args, &m) == nil {
		for _, k := range []string{"command", "cmd", "path", "file_path", "pattern", "query", "url"} {
			if s, ok := m[k].(string); ok && s != "" {
				return name + " " + clipLine(s, 80)
			}
		}
	}
	return name
}

func joinModel(provider, model string) string {
	switch {
	case provider != "" && model != "":
		return provider + "/" + model
	case model != "":
		return model
	default:
		return provider
	}
}

func iso(s string) int64 {
	if s == "" {
		return 0
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UnixMilli()
	}
	return 0
}

func ms(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.UnixMilli(v)
}

func clip(s string) string {
	if len(s) <= conv.MaxOutput {
		return s
	}
	n := conv.MaxOutput
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func clipLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > n {
		return string([]rune(s)[:n])
	}
	return s
}

func fileSessionID(path string) string {
	base := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	if i := strings.LastIndex(base, "_"); i >= 0 {
		base = base[i+1:]
	}
	if base == "" || base == "." || base == string(filepath.Separator) {
		return ""
	}
	return base
}

type lineRec struct {
	off  int64
	body []byte
}

func linesOf(r io.Reader, base int64, res *transcript.Result) ([]lineRec, error) {
	br := bufio.NewReaderSize(r, 1<<20)
	var out []lineRec
	var buf []byte
	var pending int64
	for {
		chunk, err := br.ReadSlice('\n')
		// A line past MaxLine is counted and skipped, never held in memory.
		if err == bufio.ErrBufferFull {
			pending += int64(len(chunk))
			if pending <= transcript.MaxLine {
				buf = append(buf, chunk...)
			}
			continue
		}
		if err == io.EOF {
			if len(chunk) > 0 || len(buf) > 0 {
				pending += int64(len(chunk))
				if pending <= transcript.MaxLine {
					buf = append(buf, chunk...)
				}
				off := base + res.Consumed
				res.Consumed += pending
				res.Lines++
				if body := bytes.TrimSpace(buf); len(body) > 0 && pending <= transcript.MaxLine {
					out = append(out, lineRec{off: off, body: append([]byte(nil), body...)})
				} else if pending > transcript.MaxLine {
					res.Skipped++
				}
			}
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		pending += int64(len(chunk))
		off := base + res.Consumed
		res.Consumed += pending
		res.Lines++
		if pending > transcript.MaxLine {
			res.Skipped++
			buf, pending = buf[:0], 0
			continue
		}
		body := append(append([]byte{}, buf...), chunk...)
		buf, pending = buf[:0], 0
		if body = bytes.TrimSpace(body); len(body) > 0 {
			out = append(out, lineRec{off: off, body: body})
		}
	}
}
