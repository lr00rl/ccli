// Package transcript parses Claude Code session transcripts (JSONL). It is
// pure: it reads bytes and returns what it found, so the indexer can feed it a
// file from any offset and persist the result.
package transcript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// Kind says which transcript a file is.
type Kind int

const (
	Main     Kind = iota // projects/<key>/<id>.jsonl
	Subagent             // <id>/subagents/agent-*.jsonl
	Workflow             // <id>/subagents/workflows/<run>/agent-*.jsonl
)

func (k Kind) String() string {
	switch k {
	case Subagent:
		return "subagent"
	case Workflow:
		return "workflow"
	}
	return "main"
}

// ActiveGap is the longest pause between two lines that still counts as
// active time.
const ActiveGap = 5 * time.Minute

// MaxLine is the longest line parsed; longer lines are skipped. A variable so
// tests can lower it.
var MaxLine int64 = 64 << 20

// MaxPromptText is the most of a prompt kept in a Turn; the transcript keeps
// the whole of it.
const MaxPromptText = 64 << 10

// Prompt kinds. A turn starts at every prompt; only human ones count as typed.
const (
	PromptHuman = "human" // typed, a slash command, an accepted suggestion, or sent from another client
	PromptTask  = "task"  // a background task reporting back
	PromptPeer  = "peer"  // another Claude session or a workflow coordinator
	PromptOther = "other" // scheduled and other system prompts
)

// Usage is the token usage of one API response.
type Usage struct {
	Input     int64 `json:"input"`
	Output    int64 `json:"output"`
	CacheRead int64 `json:"cache_read"`
	Cache5m   int64 `json:"cache_write_5m"`
	Cache1h   int64 `json:"cache_write_1h"`
	WebSearch int64 `json:"web_search"`
}

// Response is one API response, counted once however many lines carry it.
type Response struct {
	Key      string
	TS       int64 // unix ms
	Model    string
	Speed    string
	Usage    Usage
	Turn     int  // turn of the main conversation it belongs to (main transcripts only)
	Imported bool // the line was copied in from another account
	Replay   bool // the line repeats an earlier part of the file; it keeps its first turn
}

// Range is a byte range [From, To) of a file.
type Range struct{ From, To int64 }

// State carries what the next incremental parse of the same file needs.
type State struct {
	PrevTS int64 // timestamp of the last line seen, for active time
	Turn   int   // turn in progress (0 before the first prompt)
	// Replay is set while the file repeats earlier lines: on resume Claude
	// Code can write a stretch of the conversation again (new uuids, the
	// same promptId and times). Nothing in a replay counts again.
	Replay bool
	// Prompts holds the promptIds that started turns earlier in the file;
	// the caller loads it for an incremental parse. Not carried over.
	Prompts map[string]bool
	// Base is the file offset the parse starts at and Imported the ranges of
	// the file that ccli copied in from another account. The caller sets both
	// for each parse; they are not carried over.
	Base     int64
	Imported []Range
	// Carry is what a transcript format other than Claude Code's needs to
	// continue an incremental parse (Codex: its running token totals). The
	// store keeps it as given.
	Carry string
}

// Turn is a prompt that starts a turn of the main conversation.
type Turn struct {
	Seq      int
	Offset   int64 // file offset of the prompt's line
	PromptID string
	TS       int64
	Kind     string
	Text     string // capped at MaxPromptText
	Chars    int    // length of the whole prompt in runes
	Images   int
}

// TurnStat accumulates what happened in one turn within one parse.
type TurnStat struct {
	ToolCalls  int
	Agents     int   // Agent and Workflow calls
	LastTS     int64 // last line of the turn
	DurationMS int64 // Claude Code's own turn_duration, if recorded
	ActiveMS   int64 // time between its lines, pauses of ActiveGap or more left out
}

// Spawn is an Agent, Task or Workflow tool call.
type Spawn struct {
	ToolUseID string
	Tool      string
	Turn      int
}

// Result is what one parse found.
type Result struct {
	Consumed  int64 // bytes of complete lines read
	Lines     int
	Skipped   int // unparseable or oversized lines
	Responses []Response

	TypedPrompts int // prompts a person typed
	OtherPrompts int // task notifications, peers, scheduled and system prompts
	ToolCalls    int
	Tools        map[string]int
	Skills       map[string]int
	Commands     map[string]int // slash commands a person ran, by name

	Turns     []Turn
	TurnStats map[int]*TurnStat
	Spawns    []Spawn
	// Targets maps a spawning tool call to what it started: the agent id of
	// an Agent call or the run id of a Workflow call, from its tool result.
	Targets map[string]string

	ActiveMS int64
	Activity map[int64]int64 // UTC hour (unix ms) -> active ms
	FirstTS  int64
	LastTS   int64
	State    State

	Cwd             string
	CustomTitle     string
	AITitle         string
	Summary         string
	FirstPrompt     string // first human prompt of a main transcript, the task prompt of a subagent
	TeamName        string
	AgentName       string
	AgentID         string
	Attribution     string // the agent type a subagent's lines carry (attributionAgent)
	BridgeSessionID string
	ContinuedIn     []string
	Entrypoint      string
	Version         string
	GitBranch       string
	CostUSD         float64
	HasCost         bool
	// Processes holds Claude Code's own per-process totals (cost-state
	// lines), the latest snapshot per process.
	Processes []Process
	// Limits is the latest subscription rate-limit reading in the lines read
	// (Codex writes one with each token count).
	Limits *Limits
}

// Limits is a subscription's rate-limit reading found in a log.
type Limits struct {
	At       int64  // unix ms of the line
	Plan     string // the subscription plan, when the log names it
	FiveHour *LimitWindow
	Weekly   *LimitWindow
}

// LimitWindow is how much of one rate-limit window is used and when it
// resets.
type LimitWindow struct {
	UsedPercent float64
	ResetsAt    int64 // unix seconds
}

// Process is one Claude Code process's own accounting for a session, from
// its cost-state line. It covers API calls the transcript does not record.
type Process struct {
	Start    int64 // process start, unix ms
	End      int64 // timestamp of the last line before the snapshot
	Models   []ModelUsage
	Imported bool // the snapshot was copied in from another account
}

// ModelUsage is a process's total for one model.
type ModelUsage struct {
	Model      string
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64
	WebSearch  int64
	CostUSD    float64
}

type line struct {
	Type             string `json:"type"`
	Subtype          string `json:"subtype"`
	Timestamp        string `json:"timestamp"`
	IsMeta           bool   `json:"isMeta"`
	IsSidechain      bool   `json:"isSidechain"`
	IsCompactSummary bool   `json:"isCompactSummary"`
	Cwd              string `json:"cwd"`
	RequestID        string `json:"requestId"`
	AITitle          string `json:"aiTitle"`
	CustomTitle      string `json:"customTitle"`
	Summary          string `json:"summary"`
	TeamName         string `json:"teamName"`
	AgentName        string `json:"agentName"`
	AgentID          string `json:"agentId"`
	Attribution      string `json:"attributionAgent"`
	BridgeSessionID  string `json:"bridgeSessionId"`
	ContinuedIn      string `json:"continuedInSessionId"`
	Entrypoint       string `json:"entrypoint"`
	Version          string `json:"version"`
	GitBranch        string `json:"gitBranch"`
	PromptSource     string `json:"promptSource"`
	PromptID         string `json:"promptId"`
	DurationMS       int64  `json:"durationMs"`
	Origin           *struct {
		Kind string `json:"kind"`
	} `json:"origin"`
	ToolUseResult json.RawMessage `json:"toolUseResult"`
	TotalCostUSD  *float64        `json:"totalCostUSD"`
	StartTime     int64           `json:"startTime"`
	ModelUsage    map[string]struct {
		Input      int64   `json:"inputTokens"`
		Output     int64   `json:"outputTokens"`
		CacheRead  int64   `json:"cacheReadInputTokens"`
		CacheWrite int64   `json:"cacheCreationInputTokens"`
		WebSearch  int64   `json:"webSearchRequests"`
		CostUSD    float64 `json:"costUSD"`
	} `json:"modelUsage"`
	Message json.RawMessage `json:"message"`
}

type assistantMsg struct {
	ID    string `json:"id"`
	Model string `json:"model"`
	Usage *struct {
		Input         int64 `json:"input_tokens"`
		Output        int64 `json:"output_tokens"`
		CacheRead     int64 `json:"cache_read_input_tokens"`
		CacheCreation int64 `json:"cache_creation_input_tokens"`
		Split         *struct {
			M5 int64 `json:"ephemeral_5m_input_tokens"`
			H1 int64 `json:"ephemeral_1h_input_tokens"`
		} `json:"cache_creation"`
		Server *struct {
			WebSearch int64 `json:"web_search_requests"`
		} `json:"server_tool_use"`
		Speed string `json:"speed"`
	} `json:"usage"`
	Content []block `json:"content"`
}

type block struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Text      string          `json:"text"`
	ToolUseID string          `json:"tool_use_id"`
	Input     json.RawMessage `json:"input"`
}

type userMsg struct {
	Content json.RawMessage `json:"content"`
}

type parser struct {
	kind      Kind
	res       *Result
	seen      map[string]int  // response key -> index in res.Responses
	blocks    map[string]bool // tool_use ids already counted
	continued map[string]bool
	offset    int64 // file offset of the line being handled
}

// Parse reads complete lines from r. kind decides what counts toward prompts
// and active time; st continues an earlier parse of the same file.
func Parse(r io.Reader, kind Kind, st State) (Result, error) {
	res := NewResult(st)
	p := &parser{kind: kind, res: &res, seen: map[string]int{}, blocks: map[string]bool{}, continued: map[string]bool{}}
	if res.State.Prompts == nil {
		res.State.Prompts = map[string]bool{}
	}
	err := res.EachLine(r, st.Base, func(off int64, ln []byte) {
		p.offset = off
		p.line(ln)
	})
	res.State.Base, res.State.Imported, res.State.Prompts = 0, nil, nil
	return res, err
}

// NewResult is an empty result that continues from state st.
func NewResult(st State) Result {
	return Result{
		Tools:     map[string]int{},
		Skills:    map[string]int{},
		Commands:  map[string]int{},
		Activity:  map[int64]int64{},
		TurnStats: map[int]*TurnStat{},
		Targets:   map[string]string{},
		State:     st,
	}
}

// EachLine calls fn with each complete line of r, trimmed, and the file
// offset it starts at (base is the offset r starts at), and counts the bytes
// and lines read in res. A line longer than MaxLine is counted as skipped and
// not passed on. A partial last line is left for the next parse, so Consumed
// covers complete lines only.
func (res *Result) EachLine(r io.Reader, base int64, fn func(offset int64, line []byte)) error {
	br := bufio.NewReaderSize(r, 1<<20)
	var buf []byte     // a line longer than the reader's buffer, being assembled
	var pending int64  // bytes of the current line read so far
	oversized := false // the current line is longer than MaxLine
	for {
		chunk, err := br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			pending += int64(len(chunk))
			if !oversized {
				if pending > MaxLine {
					oversized, buf = true, buf[:0]
				} else {
					buf = append(buf, chunk...)
				}
			}
			continue
		}
		if err == io.EOF {
			return nil // a partial last line waits for the next parse
		}
		if err != nil {
			return err
		}
		pending += int64(len(chunk))
		off := base + res.Consumed
		res.Consumed += pending
		res.Lines++
		if oversized || pending > MaxLine {
			res.Skipped++
		} else {
			ln := chunk
			if len(buf) > 0 {
				buf = append(buf, chunk...)
				ln = buf
			}
			if ln = bytes.TrimSpace(ln); len(ln) > 0 {
				fn(off, ln)
			}
		}
		buf, pending, oversized = buf[:0], 0, false
	}
}

func (p *parser) imported() bool {
	for _, r := range p.res.State.Imported {
		if p.offset >= r.From && p.offset < r.To {
			return true
		}
	}
	return false
}

func (p *parser) stat() *TurnStat { return p.res.Stat() }

// Stat returns the counters of the turn in progress.
func (res *Result) Stat() *TurnStat {
	t := res.State.Turn
	s := res.TurnStats[t]
	if s == nil {
		s = &TurnStat{}
		res.TurnStats[t] = s
	}
	return s
}

// AddTurn starts the next turn of the main conversation with its prompt,
// which begins at file offset off, and counts the prompt as typed or other
// by its kind.
func (res *Result) AddTurn(off, ts int64, promptID, kind, text string, images int) {
	res.State.Turn++
	t := Turn{Seq: res.State.Turn, Offset: off, PromptID: promptID, TS: ts, Kind: kind, Chars: utf8.RuneCountInString(text), Images: images}
	t.Text = capText(text, MaxPromptText)
	res.Turns = append(res.Turns, t)
	if kind == PromptHuman {
		res.TypedPrompts++
	} else {
		res.OtherPrompts++
	}
}

func (p *parser) line(raw []byte) {
	if len(raw) == 0 {
		return
	}
	res := p.res
	var l line
	if json.Unmarshal(raw, &l) != nil {
		res.Skipped++
		return
	}
	ts := parseTS(l.Timestamp)
	gap := res.Tick(ts, p.kind == Main) // active time this line adds
	if res.Cwd == "" && l.Cwd != "" {
		res.Cwd = l.Cwd
	}
	if l.TeamName != "" {
		res.TeamName = l.TeamName
	}
	if l.AgentName != "" {
		res.AgentName = l.AgentName
	}
	if l.AgentID != "" && res.AgentID == "" {
		res.AgentID = l.AgentID
	}
	if l.Attribution != "" && res.Attribution == "" {
		res.Attribution = l.Attribution
	}
	if l.BridgeSessionID != "" {
		res.BridgeSessionID = l.BridgeSessionID
	}
	if l.Entrypoint != "" {
		res.Entrypoint = l.Entrypoint
	}
	if l.Version != "" {
		res.Version = l.Version
	}
	if l.GitBranch != "" {
		res.GitBranch = l.GitBranch
	}

	turn := res.State.Turn
	switch l.Type {
	case "assistant":
		p.assistant(l, ts)
	case "user":
		p.user(l, ts)
	case "system":
		if p.kind == Main && !res.State.Replay && l.Subtype == "turn_duration" && l.DurationMS > 0 {
			if s := p.stat(); l.DurationMS > s.DurationMS {
				s.DurationMS = l.DurationMS
			}
		}
	case "custom-title":
		if l.CustomTitle != "" {
			res.CustomTitle = clean(l.CustomTitle)
		}
	case "ai-title":
		if l.AITitle != "" {
			res.AITitle = clean(l.AITitle)
		}
	case "summary":
		if l.Summary != "" {
			res.Summary = clean(l.Summary)
		}
	case "continued-in":
		if l.ContinuedIn != "" && !p.continued[l.ContinuedIn] {
			p.continued[l.ContinuedIn] = true
			res.ContinuedIn = append(res.ContinuedIn, l.ContinuedIn)
		}
	case "cost-state":
		p.costState(l)
	}
	// Recorded after a new prompt has moved the turn on, so the prompt's own
	// line counts toward its turn. Only conversation lines count: Claude Code
	// also writes bookkeeping lines (titles, modes, links) at any later time.
	if p.kind == Main && ts > 0 && !res.State.Replay && (l.Type == "assistant" || l.Type == "user") {
		if s := p.stat(); ts > s.LastTS {
			s.LastTS = ts
		}
	}
	// The pause before a prompt is the person reading and typing; it belongs
	// to neither turn.
	if p.kind == Main && gap > 0 && !res.State.Replay && res.State.Turn == turn {
		p.stat().ActiveMS += gap
	}
}

func (p *parser) costState(l line) {
	res := p.res
	if l.TotalCostUSD != nil {
		res.CostUSD, res.HasCost = *l.TotalCostUSD, true
	}
	if l.StartTime <= 0 || len(l.ModelUsage) == 0 {
		return
	}
	pr := Process{Start: l.StartTime, End: res.State.PrevTS, Imported: p.imported()}
	if pr.End == 0 {
		pr.End = res.LastTS
	}
	for m, u := range l.ModelUsage {
		pr.Models = append(pr.Models, ModelUsage{Model: m, Input: u.Input, Output: u.Output,
			CacheRead: u.CacheRead, CacheWrite: u.CacheWrite, WebSearch: u.WebSearch, CostUSD: u.CostUSD})
	}
	sort.Slice(pr.Models, func(i, j int) bool { return pr.Models[i].Model < pr.Models[j].Model })
	for i := range res.Processes {
		if res.Processes[i].Start == pr.Start {
			res.Processes[i] = pr
			return
		}
	}
	res.Processes = append(res.Processes, pr)
}

func (p *parser) assistant(l line, ts int64) {
	res := p.res
	var m assistantMsg
	if json.Unmarshal(l.Message, &m) != nil {
		return
	}
	for _, b := range m.Content {
		if b.Type != "tool_use" {
			continue
		}
		if b.ID != "" {
			if p.blocks[b.ID] {
				continue
			}
			p.blocks[b.ID] = true
		}
		res.ToolCalls++
		res.Tools[b.Name]++
		if p.kind == Main && !res.State.Replay {
			p.stat().ToolCalls++
		}
		if (b.Name == "Agent" || b.Name == "Task" || b.Name == "Workflow") && b.ID != "" {
			res.Spawns = append(res.Spawns, Spawn{ToolUseID: b.ID, Tool: b.Name, Turn: res.State.Turn})
			if p.kind == Main && !res.State.Replay {
				p.stat().Agents++
			}
		}
		if b.Name == "Skill" && len(b.Input) > 0 {
			var in struct {
				Skill string `json:"skill"`
			}
			if json.Unmarshal(b.Input, &in) == nil && in.Skill != "" {
				res.Skills[in.Skill]++
			}
		}
	}
	if m.Usage == nil || m.Model == "<synthetic>" {
		return
	}
	key := m.ID
	if key == "" {
		key = l.RequestID
	}
	if key == "" {
		return
	}
	u := Usage{Input: m.Usage.Input, Output: m.Usage.Output, CacheRead: m.Usage.CacheRead}
	if s := m.Usage.Split; s != nil && s.M5+s.H1 > 0 {
		u.Cache5m, u.Cache1h = s.M5, s.H1
	} else {
		u.Cache5m = m.Usage.CacheCreation
	}
	if m.Usage.Server != nil {
		u.WebSearch = m.Usage.Server.WebSearch
	}
	speed := m.Usage.Speed
	if speed == "" {
		speed = "standard"
	}
	r := Response{Key: key, TS: ts, Model: m.Model, Speed: speed, Usage: u, Imported: p.imported(), Replay: res.State.Replay}
	if p.kind == Main {
		r.Turn = res.State.Turn
	}
	if i, ok := p.seen[key]; ok {
		if res.Responses[i].Usage != u {
			r.TS, r.Turn, r.Imported = res.Responses[i].TS, res.Responses[i].Turn, res.Responses[i].Imported
			res.Responses[i] = r
		}
		return
	}
	p.seen[key] = len(res.Responses)
	res.Responses = append(res.Responses, r)
}

func (p *parser) user(l line, ts int64) {
	var m userMsg
	if json.Unmarshal(l.Message, &m) != nil || len(m.Content) == 0 {
		return
	}
	c := bytes.TrimSpace(m.Content)
	if len(c) > 0 && c[0] == '[' && len(l.ToolUseResult) > 0 {
		p.toolResult(c, l.ToolUseResult)
	}
	if l.IsMeta || l.IsCompactSummary {
		return
	}
	if p.kind != Main {
		// A subagent's first prompt is the task it was given; it labels
		// subagents whose meta file has no description.
		if p.res.FirstPrompt == "" {
			if text, _, ok := promptText(c); ok {
				p.res.FirstPrompt = clean(text)
			}
		}
		return
	}
	if l.IsSidechain {
		return
	}
	text, images, ok := promptText(c)
	if !ok {
		return
	}
	kind := promptKind(l, text)
	if kind == "" {
		return
	}
	res := p.res
	if l.PromptID != "" {
		if res.State.Prompts[l.PromptID] {
			res.State.Replay = true
			return
		}
		res.State.Prompts[l.PromptID] = true
	}
	res.State.Replay = false
	res.AddTurn(p.offset, ts, l.PromptID, kind, text, images)
	if kind != PromptHuman {
		return
	}
	if m := commandName.FindStringSubmatch(text); m != nil {
		name := m[1]
		if !strings.HasPrefix(name, "/") {
			name = "/" + name
		}
		res.Commands[name]++
	}
	if res.FirstPrompt == "" {
		if s := strings.TrimSpace(text); s != "" && !strings.HasPrefix(s, "<") {
			res.FirstPrompt = clean(s)
		}
	}
}

// toolResult links an Agent or Workflow call to what it started: a subagent,
// a workflow run, or a teammate (another session, recorded as
// "teammate:<name>").
func (p *parser) toolResult(content, result json.RawMessage) {
	if !bytes.Contains(result, []byte(`"agentId"`)) && !bytes.Contains(result, []byte(`"runId"`)) && !bytes.Contains(result, []byte(`"teammate_id"`)) {
		return
	}
	var r struct {
		AgentID  string `json:"agentId"`
		RunID    string `json:"runId"`
		Teammate string `json:"teammate_id"`
		Name     string `json:"name"`
	}
	if json.Unmarshal(result, &r) != nil {
		return
	}
	target := r.RunID
	switch {
	case target != "":
	case r.AgentID != "":
		target = r.AgentID
	case r.Teammate != "":
		name := r.Name
		if name == "" {
			name, _, _ = strings.Cut(r.Teammate, "@")
		}
		target = "teammate:" + name
	default:
		return
	}
	var bs []block
	if json.Unmarshal(content, &bs) != nil {
		return
	}
	for _, b := range bs {
		if b.Type == "tool_result" && b.ToolUseID != "" {
			p.res.Targets[b.ToolUseID] = target
			return
		}
	}
}

// Output that Claude Code records as a user line but nobody wrote.
var outputTags = []string{"<local-command-stdout>", "<local-command-stderr>", "<local-command-caveat>", "<bash-stdout>", "<bash-stderr>"}

func promptKind(l line, text string) string {
	origin := ""
	if l.Origin != nil {
		origin = l.Origin.Kind
	}
	return PromptKind(origin, l.PromptSource, text)
}

// PromptKind classifies a prompt from its line's origin.kind and
// promptSource and its text, or returns "" for a line that is output, not a
// prompt.
func PromptKind(origin, source, text string) string {
	s := strings.TrimSpace(text)
	for _, tag := range outputTags {
		if strings.HasPrefix(s, tag) {
			return ""
		}
	}
	if strings.HasPrefix(s, "[Request interrupted by user") {
		return "" // Claude Code's marker for an interrupted turn
	}
	if origin != "" {
		switch origin {
		case "human":
			return PromptHuman
		case "task-notification":
			return PromptTask
		case "peer", "coordinator":
			return PromptPeer
		}
		return PromptOther
	}
	switch source {
	case "":
	case "typed", "suggestion_accepted":
		return PromptHuman
	default:
		if strings.HasPrefix(s, "<task-notification>") {
			return PromptTask
		}
		return PromptOther
	}
	// Lines from before Claude Code recorded an origin.
	switch {
	case strings.HasPrefix(s, "<task-notification>"):
		return PromptTask
	case strings.HasPrefix(s, "Another Claude session sent a message:"), strings.HasPrefix(s, "<teammate-message"):
		return PromptPeer
	}
	return PromptHuman
}

// promptText returns the text of a user message, how many images it carries,
// and whether it is a prompt (text or an image, no tool result).
func promptText(c json.RawMessage) (string, int, bool) {
	if len(c) > 0 && c[0] == '"' {
		var s string
		if json.Unmarshal(c, &s) != nil {
			return "", 0, false
		}
		return s, 0, strings.TrimSpace(s) != ""
	}
	var bs []block
	if json.Unmarshal(c, &bs) != nil {
		return "", 0, false
	}
	var parts []string
	images := 0
	for _, b := range bs {
		switch b.Type {
		case "tool_result":
			return "", 0, false
		case "text":
			if strings.TrimSpace(b.Text) != "" {
				parts = append(parts, b.Text)
			}
		case "image", "document":
			images++
		}
	}
	return strings.Join(parts, "\n\n"), images, len(parts) > 0 || images > 0
}

func parseTS(s string) int64 {
	if s == "" {
		return 0
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return 0
	}
	return t.UnixMilli()
}

// clean collapses whitespace and caps a title at 200 runes.
func clean(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > 200 {
		r := []rune(s)
		s = string(r[:200])
	}
	return s
}

// capText cuts s to at most n bytes on a rune boundary.
func capText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

var commandName = regexp.MustCompile(`<command-name>\s*([^<\s]+)\s*</command-name>`)

// Tick records a line's time (unix ms; 0 when it has none): the first and
// last times, and, when active is set, the active time since the previous
// line, leaving out pauses of ActiveGap or longer. It returns the active
// time the line added.
func (res *Result) Tick(ts int64, active bool) int64 {
	if ts <= 0 {
		return 0
	}
	if res.FirstTS == 0 || ts < res.FirstTS {
		res.FirstTS = ts
	}
	if ts > res.LastTS {
		res.LastTS = ts
	}
	if !active {
		return 0
	}
	var gap int64
	if prev := res.State.PrevTS; prev > 0 && ts > prev {
		if d := ts - prev; d < ActiveGap.Milliseconds() {
			gap = d
			res.ActiveMS += gap
			res.Activity[ts-ts%3_600_000] += gap
		}
	}
	if ts > res.State.PrevTS {
		res.State.PrevTS = ts
	}
	return gap
}

// ParseTS reads an RFC 3339 time as unix milliseconds, 0 when it is empty or
// malformed.
func ParseTS(s string) int64 { return parseTS(s) }

// Clean collapses whitespace and caps a title at 200 runes.
func Clean(s string) string { return clean(s) }
