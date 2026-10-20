// Package cursoride reads Cursor IDE composer chats from state.vscdb.
// A read creates no -shm or -wal next to the database (see sqlitero) and
// still sees what Cursor has committed to its -wal. IDE chats and Cursor CLI
// transcripts are different stores and stay different accounts.
package cursoride

import (
	"database/sql"
	"encoding/json"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/lr00rl/ccli/internal/conv"
	"github.com/lr00rl/ccli/internal/transcript"
)

// Speaker names the model's side of an IDE conversation.
const Speaker = "Cursor IDE"

var userQuery = regexp.MustCompile(`(?s)<user_query>\s*(.*?)\s*</user_query>`)

// Head is one composer, enough to decide whether it is a session of its
// own or a subagent of another.
type Head struct {
	ID, Title, Cwd, Model string
	Parent, Root          string
	AgentType, ToolCall   string
	Sub                   bool
	Created, Updated      int64
}

// List reads composer headers and the workspace folder each one names.
func List(user string) ([]Head, error) {
	dbPath := DBPath(user)
	gate.Lock()
	defer gate.Unlock()
	db, err := open(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT composerId, createdAt, lastUpdatedAt, isSubagent, value FROM composerHeaders`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Head
	for rows.Next() {
		var id string
		var created, updated sql.NullInt64
		var sub int
		var value []byte
		if err := rows.Scan(&id, &created, &updated, &sub, &value); err != nil {
			return nil, err
		}
		if id == "" {
			continue
		}
		h := Head{ID: id, Sub: sub != 0, Created: created.Int64, Updated: updated.Int64}
		fillHead(&h, value)
		if h.Cwd == "" {
			h.Cwd = workspacePath(user, workspaceID(value))
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func fillHead(h *Head, value []byte) {
	if len(value) == 0 {
		return
	}
	var v struct {
		Name  string `json:"name"`
		Model string `json:"model"`
		Info  *struct {
			Parent   string `json:"parentComposerId"`
			Root     string `json:"rootParentConversationId"`
			TypeName string `json:"subagentTypeName"`
			ToolCall string `json:"toolCallId"`
		} `json:"subagentInfo"`
	}
	if json.Unmarshal(value, &v) != nil {
		return
	}
	h.Title = v.Name
	if v.Info != nil {
		h.Parent, h.Root = v.Info.Parent, v.Info.Root
		h.AgentType, h.ToolCall = v.Info.TypeName, v.Info.ToolCall
		if h.Root == "" {
			h.Root = h.Parent
		}
	}
}

func workspaceID(value []byte) string {
	var v struct {
		WS struct {
			ID string `json:"id"`
		} `json:"workspaceIdentifier"`
	}
	if json.Unmarshal(value, &v) != nil {
		return ""
	}
	return v.WS.ID
}

// Agent is the subagent link a composer carries, when it is one.
type Agent struct {
	Type, Description, ToolUseID string
}

// Parse reads one composer's bubbles. path is "<db>#<composerId>".
func Parse(path string) (transcript.Result, *Agent, error) {
	res, agent, _, err := load(path, false)
	return res, agent, err
}

// FindTurn returns the turn of ReadRange's result whose seq matches, or nil.
func FindTurn(turns []conv.Turn, _ int64, seq int) *conv.Turn {
	for i := range turns {
		if turns[i].Seq == seq {
			return &turns[i]
		}
	}
	return nil
}

// ReadRange reads a composer's bubbles as turns.
func ReadRange(path string) ([]conv.Turn, error) {
	_, _, turns, err := load(path, true)
	return turns, err
}

func load(path string, view bool) (transcript.Result, *Agent, []conv.Turn, error) {
	res := transcript.NewResult(transcript.State{})
	dbPath, id, ok := Split(path)
	if !ok {
		return res, nil, nil, os.ErrNotExist
	}
	gate.Lock()
	defer gate.Unlock()
	db, err := open(dbPath)
	if err != nil {
		return res, nil, nil, err
	}
	defer db.Close()

	var value []byte
	var created, updated sql.NullInt64
	var sub int
	err = db.QueryRow(`SELECT createdAt, lastUpdatedAt, isSubagent, value FROM composerHeaders WHERE composerId = ?`, id).
		Scan(&created, &updated, &sub, &value)
	if err != nil {
		return res, nil, nil, err
	}
	h := Head{ID: id, Sub: sub != 0, Created: created.Int64, Updated: updated.Int64}
	fillHead(&h, value)
	h.Cwd = workspacePath(userDir(dbPath), workspaceID(value))
	if name, model := composerMeta(db, id); name != "" || model != "" {
		if h.Title == "" {
			h.Title = name
		}
		h.Model = model
	}
	res.Cwd = h.Cwd
	res.CustomTitle = h.Title
	res.Entrypoint = "cursor-ide"
	res.FirstTS, res.LastTS = h.Created, h.Updated
	if res.LastTS == 0 {
		res.LastTS = h.Created
	}
	var agent *Agent
	if h.Sub {
		agent = &Agent{Type: h.AgentType, ToolUseID: h.ToolCall}
		if agent.Type == "" {
			agent.Type = "subagent"
		}
	}

	bubbles, err := bubblesOf(db, id)
	if err != nil {
		return res, agent, nil, err
	}
	if order := bubbleOrder(db, id); len(order) > 0 {
		bubbles = orderBubbles(bubbles, order)
	} else {
		sort.SliceStable(bubbles, func(i, j int) bool {
			if bubbles[i].at != bubbles[j].at {
				return bubbles[i].at < bubbles[j].at
			}
			return bubbles[i].key < bubbles[j].key
		})
	}
	var turns []conv.Turn
	var models []string
	noteModel := func(m string) {
		if m == "" {
			return
		}
		for _, have := range models {
			if have == m {
				return
			}
		}
		models = append(models, m)
	}
	cur := 0
	for _, b := range bubbles {
		if b.at > 0 && (res.LastTS == 0 || b.at > res.LastTS) {
			res.LastTS = b.at
		}
		model := b.model
		if model == "" {
			model = h.Model
		}
		noteModel(model)
		if b.tokens.in+b.tokens.out+b.tokens.cr+b.tokens.cw > 0 {
			res.Responses = append(res.Responses, transcript.Response{
				Key: "ide:" + id + ":" + b.id, TS: b.at, Model: model, Turn: cur,
				Usage: transcript.Usage{Input: b.tokens.in, Output: b.tokens.out, CacheRead: b.tokens.cr, Cache5m: b.tokens.cw},
			})
		}
		if b.sim {
			if strings.TrimSpace(b.text) != "" {
				res.OtherPrompts++
				if view {
					tail(&turns, cur).Entries = append(tail(&turns, cur).Entries, conv.Entry{Kind: conv.KindContext, Label: "cursor", Text: clip(b.text)})
				}
			}
			continue
		}
		switch b.kind {
		case 1:
			text := promptText(b.text)
			if text == "" {
				continue
			}
			cur++
			keep := text
			if utf8.RuneCountInString(keep) > transcript.MaxPromptText {
				keep = string([]rune(keep)[:transcript.MaxPromptText])
			}
			res.Turns = append(res.Turns, transcript.Turn{
				Seq: cur, Offset: int64(cur), TS: b.at, Kind: transcript.PromptHuman,
				Text: keep, Chars: utf8.RuneCountInString(text),
			})
			res.TypedPrompts++
			if res.FirstPrompt == "" {
				res.FirstPrompt = text
				if agent != nil && agent.Description == "" {
					agent.Description = clipLine(text, 200)
				}
			}
			turns = append(turns, conv.Turn{Seq: cur, Offset: int64(cur), Speaker: Speaker, Entries: []conv.Entry{{
				Kind: conv.KindPrompt, At: ms(b.at), Text: text, Label: transcript.PromptHuman,
			}}})
		case 2:
			t := tail(&turns, cur)
			if b.think != "" && view {
				t.Entries = append(t.Entries, conv.Entry{Kind: conv.KindThinking, At: ms(b.at), Text: b.think, Model: model})
			}
			if b.text != "" && view {
				t.Entries = append(t.Entries, conv.Entry{Kind: conv.KindText, At: ms(b.at), Text: b.text, Model: model})
			}
			if b.tool.Name != "" {
				res.ToolCalls++
				res.Tools[b.tool.Name]++
				if cur > 0 {
					st := res.TurnStats[cur]
					if st == nil {
						st = &transcript.TurnStat{}
						res.TurnStats[cur] = st
					}
					st.ToolCalls++
				}
				if view {
					c := b.tool
					t.Entries = append(t.Entries, conv.Entry{Kind: conv.KindTool, At: ms(b.at), Model: model, Call: &c})
				}
			}
		}
	}
	res.State.Turn = cur
	have := map[string]bool{}
	for _, r := range res.Responses {
		if r.Model != "" {
			have[r.Model] = true
		}
	}
	for _, m := range models {
		if have[m] {
			continue
		}
		res.Responses = append(res.Responses, transcript.Response{
			Key: "ide:" + id + ":model:" + m, Model: m, TS: res.FirstTS, Turn: 1,
		})
	}
	if !view {
		turns = nil
	}
	return res, agent, turns, nil
}

// bubbleOrder is the conversation's own bubble list. Rows that are not in
// it are drafts and retries Cursor kept beside the thread, and createdAt
// is empty often enough that sorting on it reorders the thread.
func bubbleOrder(db *sql.DB, id string) []string {
	var raw []byte
	if db.QueryRow(`SELECT value FROM cursorDiskKV WHERE key = ?`, "composerData:"+id).Scan(&raw) != nil || len(raw) == 0 {
		return nil
	}
	var v struct {
		Headers []struct {
			BubbleID string `json:"bubbleId"`
		} `json:"fullConversationHeadersOnly"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return nil
	}
	var ids []string
	seen := map[string]bool{}
	for _, h := range v.Headers {
		if h.BubbleID == "" || seen[h.BubbleID] {
			continue
		}
		seen[h.BubbleID] = true
		ids = append(ids, h.BubbleID)
	}
	return ids
}

func orderBubbles(in []bubble, order []string) []bubble {
	by := map[string]bubble{}
	for _, b := range in {
		if _, ok := by[b.id]; ok {
			continue
		}
		by[b.id] = b
	}
	out := make([]bubble, 0, len(order))
	for _, id := range order {
		b, ok := by[id]
		if !ok {
			continue
		}
		out = append(out, b)
	}
	return out
}

func composerMeta(db *sql.DB, id string) (name, model string) {
	var raw []byte
	if db.QueryRow(`SELECT value FROM cursorDiskKV WHERE key = ?`, "composerData:"+id).Scan(&raw) != nil || len(raw) == 0 {
		return "", ""
	}
	var v struct {
		Name   string `json:"name"`
		Config struct {
			ModelName string `json:"modelName"`
		} `json:"modelConfig"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return "", ""
	}
	return v.Name, v.Config.ModelName
}

type bubble struct {
	key, id, text, think, model string
	kind                        int
	at                          int64
	sim                         bool
	tokens                      tok
	tool                        conv.Call
}

type tok struct{ in, out, cr, cw int64 }

func bubblesOf(db *sql.DB, id string) ([]bubble, error) {
	prefix := "bubbleId:" + id + ":"
	rows, err := db.Query(`SELECT key, value FROM cursorDiskKV WHERE key >= ? AND key < ?`, prefix, prefixEnd(prefix))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []bubble
	for rows.Next() {
		var key string
		var value []byte
		if err := rows.Scan(&key, &value); err != nil {
			return nil, err
		}
		b, ok := decodeBubble(key, value)
		if ok {
			out = append(out, b)
		}
	}
	return out, rows.Err()
}

func prefixEnd(prefix string) string {
	return prefix[:len(prefix)-1] + string(prefix[len(prefix)-1]+1)
}

func decodeBubble(key string, value []byte) (bubble, bool) {
	var raw struct {
		Type       int             `json:"type"`
		BubbleID   string          `json:"bubbleId"`
		Text       string          `json:"text"`
		Thinking   json.RawMessage `json:"thinking"`
		CreatedAt  json.RawMessage `json:"createdAt"`
		Simulated  bool            `json:"isSimulatedMsg"`
		TokenCount struct {
			Input      int64 `json:"inputTokens"`
			Output     int64 `json:"outputTokens"`
			CacheRead  int64 `json:"cacheReadTokens"`
			CacheWrite int64 `json:"cacheWriteTokens"`
		} `json:"tokenCount"`
		ModelInfo struct {
			ModelName string `json:"modelName"`
		} `json:"modelInfo"`
		Tool *struct {
			Name   string          `json:"name"`
			CallID string          `json:"toolCallId"`
			Params json.RawMessage `json:"params"`
			Args   json.RawMessage `json:"rawArgs"`
			Result json.RawMessage `json:"result"`
			Status string          `json:"status"`
		} `json:"toolFormerData"`
	}
	if json.Unmarshal(value, &raw) != nil || (raw.Type != 1 && raw.Type != 2) {
		return bubble{}, false
	}
	id := raw.BubbleID
	if id == "" {
		if i := strings.LastIndex(key, ":"); i >= 0 {
			id = key[i+1:]
		}
	}
	b := bubble{
		key: key, id: id, text: raw.Text, kind: raw.Type, at: unixMS(raw.CreatedAt), sim: raw.Simulated,
		model:  raw.ModelInfo.ModelName,
		tokens: tok{in: raw.TokenCount.Input, out: raw.TokenCount.Output, cr: raw.TokenCount.CacheRead, cw: raw.TokenCount.CacheWrite},
		think:  thinkingText(raw.Thinking),
	}
	if raw.Tool != nil && raw.Tool.Name != "" {
		in := raw.Tool.Params
		if len(bytesTrim(in)) == 0 {
			in = raw.Tool.Args
		}
		b.tool = conv.Call{
			ID: raw.Tool.CallID, Name: raw.Tool.Name, Input: in, Done: len(raw.Tool.Result) > 0,
			Summary: raw.Tool.Name, Output: clip(jsonText(raw.Tool.Result)), OutputLen: len(raw.Tool.Result),
		}
		if raw.Tool.Status == "error" {
			b.tool.IsError = true
		}
	}
	return b, true
}

func thinkingText(raw json.RawMessage) string {
	raw = bytesTrim(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
	}
	return ""
}

func jsonText(raw json.RawMessage) string {
	raw = bytesTrim(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
	}
	return string(raw)
}

func bytesTrim(b []byte) []byte { return []byte(strings.TrimSpace(string(b))) }

func unixMS(raw json.RawMessage) int64 {
	raw = bytesTrim(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return 0
	}
	var n int64
	if json.Unmarshal(raw, &n) == nil && n > 0 {
		return n
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t.UnixMilli()
		}
	}
	return 0
}

func promptText(s string) string {
	if qs := userQuery.FindAllStringSubmatch(s, -1); len(qs) > 0 {
		var b strings.Builder
		for _, q := range qs {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(strings.TrimSpace(q[1]))
		}
		return b.String()
	}
	return strings.TrimSpace(s)
}

func tail(turns *[]conv.Turn, seq int) *conv.Turn {
	if len(*turns) == 0 || (*turns)[len(*turns)-1].Seq != seq {
		*turns = append(*turns, conv.Turn{Seq: seq, Speaker: Speaker})
	}
	return &(*turns)[len(*turns)-1]
}

func ms(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.UnixMilli(v)
}

func clip(s string) string {
	if len(s) > conv.MaxOutput {
		return s[:conv.MaxOutput]
	}
	return s
}

func clipLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > n {
		return string([]rune(s)[:n])
	}
	return s
}
