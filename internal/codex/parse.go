package codex

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"

	"github.com/lr00rl/ccli/internal/transcript"
)

// maxStreams bounds the token streams tracked per file. One process writes
// one stream; a resume starts another, and two processes can write one file.
const maxStreams = 8

// carry is what an incremental parse of a rollout continues from. It is kept
// as JSON in transcript.State.Carry.
type carry struct {
	// Heads are the running totals of the streams writing the file. Codex
	// counts tokens per process, cumulatively; a count is new usage when it
	// moves one of them on by exactly its own last usage.
	Heads []usage `json:"heads,omitempty"`
	Model string  `json:"model,omitempty"`
	// Unmodelled is set when a parse stored counts before any line named a
	// model; the parse that sees the first one has the index give it to them.
	Unmodelled bool `json:"unmodelled,omitempty"`

	// The turn in progress, from its task_started line: where it starts and
	// whether a person's prompt has opened it yet.
	TurnOpen   bool  `json:"turn_open,omitempty"`
	TurnOffset int64 `json:"turn_offset,omitempty"`
	Prompted   bool  `json:"prompted,omitempty"`
	// The last prompt taken, by text hash and time: outside a turn, Codex can
	// write one prompt both as an event and as a message.
	LastPrompt   uint64 `json:"last_prompt,omitempty"`
	LastPromptTS int64  `json:"last_prompt_ts,omitempty"`

	// Fork state. A fork copies the history of the thread it came from;
	// none of the copy counts here. Forks since CLI 0.147 copy the lines
	// below SkipBelow (by ordinal). Older forks copy a burst of lines that
	// all carry the time of the fork (Burst, ForkTS): the fork's own first
	// turn is the last one started in that burst.
	SkipBelow int64  `json:"skip_below,omitempty"`
	Burst     bool   `json:"burst,omitempty"`
	ForkTS    int64  `json:"fork_ts,omitempty"`
	Pending   string `json:"pending,omitempty"` // the prompt of the last turn started in the burst
	PendingTS int64  `json:"pending_ts,omitempty"`
	PendingIm int    `json:"pending_images,omitempty"`

	// Spawns are spawn_agent calls not yet linked to the agent they started.
	Spawns []string `json:"spawns,omitempty"`
	Meta   bool     `json:"meta,omitempty"` // the file's own session_meta was read
}

type parser struct {
	kind   transcript.Kind
	thread string
	res    *transcript.Result
	c      *carry
	meta   *Meta // the file's own session_meta, when this parse read it
}

// Parse reads the complete lines of a rollout from r. thread is the file's
// thread id; kind is Main for a thread a person started and Subagent for one
// another thread started. st continues an earlier parse of the same file.
// The thread's metadata comes back when this parse read its first line.
func Parse(r io.Reader, kind transcript.Kind, thread string, st transcript.State) (transcript.Result, *Meta, error) {
	res := transcript.NewResult(st)
	c := &carry{}
	if st.Carry != "" {
		if err := json.Unmarshal([]byte(st.Carry), c); err != nil {
			return res, nil, fmt.Errorf("codex parse state: %w", err)
		}
	}
	p := &parser{kind: kind, thread: thread, res: &res, c: c}
	// A parse that ends inside a fork's copied burst leaves it open: the next
	// parse goes on from there.
	err := res.EachLine(r, st.Base, p.line)
	if c.Model == "" && len(res.Responses) > 0 {
		c.Unmodelled = true
	}
	b, merr := json.Marshal(c)
	if merr != nil {
		return res, nil, merr
	}
	res.State.Carry = string(b)
	res.State.Base, res.State.Imported, res.State.Prompts = 0, nil, nil
	return res, p.meta, err
}

func (p *parser) line(off int64, raw []byte) {
	var l envelope
	if json.Unmarshal(raw, &l) != nil {
		p.res.Skipped++
		return
	}
	c := p.c
	if l.Type == "session_meta" {
		p.sessionMeta(l)
		return
	}
	if c.SkipBelow > 0 && l.Ordinal != nil && *l.Ordinal < c.SkipBelow {
		return // copied from the thread this one was forked from
	}
	ts := transcript.ParseTS(l.Timestamp)
	if c.Burst {
		if c.ForkTS == 0 {
			c.ForkTS = ts
		}
		if ts-c.ForkTS < 1000 {
			p.copied(off, ts, l)
			return
		}
		p.endBurst()
	}
	res := p.res
	gap := res.Tick(ts, p.kind == transcript.Main)
	if p.kind == transcript.Main && res.State.Turn > 0 {
		s := res.Stat()
		s.ActiveMS += gap
		if ts > s.LastTS {
			s.LastTS = ts
		}
	}
	switch l.Type {
	case "turn_context":
		p.turnContext(l.Payload)
	case "world_state":
		var ws struct {
			State struct {
				Model string `json:"model"`
			} `json:"state"`
		}
		if json.Unmarshal(l.Payload, &ws) == nil {
			p.setModel(ws.State.Model)
		}
	case "event_msg":
		p.event(off, ts, l.Payload)
	case "response_item":
		p.responseItem(off, ts, l.Payload)
	}
}

// sessionMeta reads the file's own session_meta (the first); the ones a
// fork copied after it describe other threads.
func (p *parser) sessionMeta(l envelope) {
	c, res := p.c, p.res
	if c.Meta {
		return
	}
	var sm sessionMeta
	if json.Unmarshal(l.Payload, &sm) != nil || sm.ID != p.thread {
		return
	}
	c.Meta = true
	m := sm.meta()
	p.meta = &m
	res.Tick(transcript.ParseTS(l.Timestamp), false)
	if res.Cwd == "" {
		res.Cwd = m.Cwd
	}
	res.Version, res.Entrypoint, res.GitBranch = m.Version, m.Originator, m.Branch
	if in := sm.Instructions; in != nil && in.Provenance != nil {
		p.setModel(in.Provenance.Model)
	}
	if m.Subagent != "" {
		res.AgentID = p.thread
		res.Attribution = m.Role
		if res.Attribution == "" {
			res.Attribution = m.Subagent
		}
	}
	switch {
	case sm.SubagentHistoryStart != nil:
		c.SkipBelow = *sm.SubagentHistoryStart
	case len(sm.HistoryBase) > 0 && string(sm.HistoryBase) != "null":
		// The fork points at the history instead of copying it.
	case sm.ForkedFromID != "":
		c.Burst = true
	}
}

// copied handles a line of an older fork's copied burst. Only the token
// totals (so the fork's own counts follow on from them), the model and the
// last turn started, with its prompt, matter: that turn is the fork's own.
func (p *parser) copied(off, ts int64, l envelope) {
	c := p.c
	switch l.Type {
	case "turn_context":
		p.turnContext(l.Payload)
	case "event_msg":
		var e typed
		if json.Unmarshal(l.Payload, &e) != nil {
			return
		}
		switch e.Type {
		case "token_count":
			var tc tokenCount
			if json.Unmarshal(l.Payload, &tc) == nil && tc.Info != nil {
				c.advance(tc.Info.Total, tc.Info.Last)
			}
		case "task_started":
			c.TurnOpen, c.TurnOffset, c.Prompted = true, off, false
			c.Pending, c.PendingTS, c.PendingIm = "", 0, 0
		default:
			if text, images, ok := eventPrompt(e.Type, l.Payload); ok && c.TurnOpen && c.Pending == "" {
				c.Pending, c.PendingTS, c.PendingIm = text, ts, images
			}
		}
	case "response_item":
		var m message
		if json.Unmarshal(l.Payload, &m) == nil {
			if text, images, ok := m.typedText(); ok && c.TurnOpen && c.Pending == "" {
				c.Pending, c.PendingTS, c.PendingIm = text, ts, images
			}
		}
	}
}

// endBurst ends an older fork's copied burst: the last turn started in it,
// if a prompt opened it, is the fork's first turn.
func (p *parser) endBurst() {
	c := p.c
	c.Burst = false
	if c.TurnOpen && c.Pending != "" {
		p.prompt(c.TurnOffset, c.PendingTS, c.Pending, c.PendingIm)
	}
	c.Pending, c.PendingTS, c.PendingIm = "", 0, 0
}

// setModel records the model the next token counts are for. Counts written
// before any line named a model are given the first one named: in this parse
// directly, and through FillModel for those an earlier parse stored.
func (p *parser) setModel(m string) {
	if m == "" {
		return
	}
	c := p.c
	if c.Model == "" {
		for i := range p.res.Responses {
			p.res.Responses[i].Model = m
		}
		if c.Unmodelled {
			p.res.FillModel, c.Unmodelled = m, false
		}
	}
	c.Model = m
}

func (p *parser) turnContext(raw json.RawMessage) {
	var tc struct {
		Model string `json:"model"`
		Cwd   string `json:"cwd"`
		Mode  struct {
			Settings struct {
				Model string `json:"model"`
			} `json:"settings"`
		} `json:"collaboration_mode"`
	}
	if json.Unmarshal(raw, &tc) != nil {
		return
	}
	if tc.Model != "" {
		p.setModel(tc.Model)
	} else {
		p.setModel(tc.Mode.Settings.Model)
	}
	if p.res.Cwd == "" {
		p.res.Cwd = tc.Cwd
	}
}

// eventPrompt returns what a person typed from an event that carries a
// prompt: user_message up to CLI 0.146, an item_completed UserMessage since.
func eventPrompt(kind string, raw json.RawMessage) (string, int, bool) {
	switch kind {
	case "user_message":
		var um struct {
			Message string            `json:"message"`
			Images  []json.RawMessage `json:"images"`
			Local   []json.RawMessage `json:"local_images"`
		}
		if json.Unmarshal(raw, &um) != nil || isInjected(um.Message) {
			return "", 0, false
		}
		return um.Message, len(um.Images) + len(um.Local), um.Message != ""
	case "item_completed":
		var ic struct {
			Item item `json:"item"`
		}
		if json.Unmarshal(raw, &ic) != nil || ic.Item.Type != "UserMessage" {
			return "", 0, false
		}
		text, images := ic.Item.userText()
		if isInjected(text) {
			return "", 0, false
		}
		return text, images, text != "" || images > 0
	}
	return "", 0, false
}

func (p *parser) event(off, ts int64, raw json.RawMessage) {
	var e typed
	if json.Unmarshal(raw, &e) != nil {
		return
	}
	c, res := p.c, p.res
	switch e.Type {
	case "thread_settings_applied":
		var applied struct {
			Settings struct {
				Model string `json:"model"`
			} `json:"thread_settings"`
		}
		if json.Unmarshal(raw, &applied) == nil {
			p.setModel(applied.Settings.Model)
		}
	case "task_started":
		c.TurnOpen, c.TurnOffset, c.Prompted = true, off, false
	case "task_complete", "turn_aborted":
		var d struct {
			DurationMS int64 `json:"duration_ms"`
		}
		if json.Unmarshal(raw, &d) == nil && p.kind == transcript.Main && res.State.Turn > 0 {
			if s := res.Stat(); d.DurationMS > s.DurationMS {
				s.DurationMS = d.DurationMS
			}
		}
		c.TurnOpen = false
	case "token_count":
		p.tokenCount(ts, raw)
	case "item_completed":
		var ic struct {
			Item item `json:"item"`
		}
		if json.Unmarshal(raw, &ic) != nil {
			return
		}
		switch ic.Item.Type {
		case "UserMessage":
			if text, images, ok := eventPrompt(e.Type, raw); ok {
				p.prompt(off, ts, text, images)
			}
		case "CollabAgentToolCall":
			if len(ic.Item.ReceiverThreadIDs) == 1 && c.dropSpawn(ic.Item.ID) {
				res.Targets[ic.Item.ID] = ic.Item.ReceiverThreadIDs[0]
			}
		case "SubAgentActivity":
			p.agentStarted(ic.Item.Kind, ic.Item.AgentThreadID)
		}
	case "user_message":
		if text, images, ok := eventPrompt(e.Type, raw); ok {
			p.prompt(off, ts, text, images)
		}
	case "sub_agent_activity":
		var a struct {
			AgentThreadID string `json:"agent_thread_id"`
			Kind          string `json:"kind"`
		}
		if json.Unmarshal(raw, &a) == nil {
			p.agentStarted(a.Kind, a.AgentThreadID)
		}
	}
}

// prompt handles what a person typed, on the line at off. The first prompt
// of a turn opens it at its task_started line; a later one was sent while
// the model worked. A prompt outside any turn opens one where it stands,
// unless it repeats the prompt just taken. A subagent's first prompt is the
// task its parent gave it.
func (p *parser) prompt(off, ts int64, text string, images int) {
	c, res := p.c, p.res
	if p.kind != transcript.Main {
		if res.FirstPrompt == "" {
			res.FirstPrompt = transcript.Clean(text)
		}
		return
	}
	h := fnv.New64a()
	h.Write([]byte(text))
	sum := h.Sum64()
	if c.TurnOpen {
		if c.Prompted {
			return
		}
		off = c.TurnOffset
	} else if sum == c.LastPrompt && ts-c.LastPromptTS < 2000 {
		return
	}
	c.Prompted, c.LastPrompt, c.LastPromptTS = true, sum, ts
	res.AddTurn(off, ts, "", transcript.PromptHuman, text, images)
	if res.FirstPrompt == "" {
		res.FirstPrompt = transcript.Clean(text)
	}
}

func (p *parser) responseItem(off, ts int64, raw json.RawMessage) {
	var ri struct {
		Type      string `json:"type"`
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
		CallID    string `json:"call_id"`
	}
	if json.Unmarshal(raw, &ri) != nil {
		return
	}
	res := p.res
	name := ""
	switch ri.Type {
	case "message":
		var m message
		if json.Unmarshal(raw, &m) == nil {
			if text, images, ok := m.typedText(); ok {
				p.prompt(off, ts, text, images)
			}
		}
		return
	case "function_call", "custom_tool_call":
		name = toolName(ri.Namespace, ri.Name)
	case "web_search_call":
		name = "web_search"
	case "tool_search_call":
		name = "tool_search"
	case "image_generation_call":
		name = "image_generation"
	default:
		return
	}
	res.ToolCalls++
	res.Tools[name]++
	if p.kind == transcript.Main {
		res.Stat().ToolCalls++
	}
	if name == "spawn_agent" && ri.CallID != "" {
		res.Spawns = append(res.Spawns, transcript.Spawn{ToolUseID: ri.CallID, Tool: name, Turn: res.State.Turn})
		if p.kind == transcript.Main {
			res.Stat().Agents++
		}
		p.c.Spawns = append(p.c.Spawns, ri.CallID)
	}
}

// agentStarted links the oldest spawn call not yet linked to the agent a
// sub_agent_activity "started" event names.
func (p *parser) agentStarted(kind, agent string) {
	c := p.c
	if kind != "started" || agent == "" || len(c.Spawns) == 0 {
		return
	}
	for _, t := range p.res.Targets {
		if t == agent {
			return
		}
	}
	p.res.Targets[c.Spawns[0]] = agent
	c.Spawns = c.Spawns[1:]
}

func (c *carry) dropSpawn(id string) bool {
	for i, s := range c.Spawns {
		if s == id {
			c.Spawns = append(c.Spawns[:i], c.Spawns[i+1:]...)
			return true
		}
	}
	return false
}

// advance returns the new usage a token count records, and moves the stream
// it belongs to on; ok is false when it records none.
//
//   - A count equal to a stream's total repeats an earlier one, or follows a
//     compaction (its last usage is then a context estimate): nothing new.
//   - A count that moves a stream on by exactly its last usage is the next
//     step of that stream.
//   - A count whose total is its own last usage starts a stream: a resumed
//     thread restarts its counter, and two processes can write one file.
//   - A count that moves a stream on by more, on every field, follows counts
//     Codex did not write: the whole difference is new.
//   - Any other count starts a stream with its last usage.
//
// When more than maxStreams streams are open, the one idle longest is
// dropped.
func (c *carry) advance(total, last usage) (usage, bool) {
	for _, h := range c.Heads {
		if total == h {
			return usage{}, false
		}
	}
	for i, h := range c.Heads {
		if total.minus(h) == last {
			c.move(i, total)
			return last, true
		}
	}
	if total != last {
		best := -1
		for i, h := range c.Heads {
			if total.covers(h) && (best < 0 || h.Total > c.Heads[best].Total) {
				best = i
			}
		}
		if best >= 0 {
			d := total.minus(c.Heads[best])
			c.move(best, total)
			return d, true
		}
	}
	c.Heads = append(c.Heads, total)
	if len(c.Heads) > maxStreams {
		c.Heads = c.Heads[1:]
	}
	return last, true
}

// move sets stream i's total and puts it last, so Heads runs from the stream
// idle longest to the one moved most recently.
func (c *carry) move(i int, total usage) {
	c.Heads = append(append(c.Heads[:i:i], c.Heads[i+1:]...), total)
}

func (p *parser) tokenCount(ts int64, raw json.RawMessage) {
	var tc tokenCount
	if json.Unmarshal(raw, &tc) != nil {
		return
	}
	res, c := p.res, p.c
	if tc.RateLimits != nil {
		p.limits(ts, tc.RateLimits)
	}
	if tc.Info == nil {
		return
	}
	last, ok := c.advance(tc.Info.Total, tc.Info.Last)
	if !ok {
		return
	}
	total := tc.Info.Total
	r := transcript.Response{
		// The thread and its running total name one count, in this file and
		// on every later parse of it.
		Key:   fmt.Sprintf("codex:%s:%d:%d:%d:%d", p.thread, total.Input, total.Cached, total.Output, total.Reasoning),
		TS:    ts,
		Model: c.Model,
		Usage: transcript.Usage{Input: last.Input - last.Cached, CacheRead: last.Cached, Output: last.Output},
		Turn:  res.State.Turn,
	}
	res.Responses = append(res.Responses, r)
}

// limits keeps the latest reading of the main Codex limit. Its windows are
// told apart by length: what Codex calls primary was the 5-hour window until
// mid 2026 and is the weekly one since.
func (p *parser) limits(ts int64, rl *rateLimits) {
	if rl.LimitID != "" && rl.LimitID != "codex" {
		return // a separate limit for one model, or prepaid credits
	}
	l := &transcript.Limits{At: ts, Plan: rl.PlanType}
	for _, w := range []*rateWindow{rl.Primary, rl.Secondary} {
		if w == nil {
			continue
		}
		lw := &transcript.LimitWindow{UsedPercent: w.UsedPercent, ResetsAt: w.ResetsAt}
		switch w.WindowMinutes {
		case 300:
			l.FiveHour = lw
		case 10080:
			l.Weekly = lw
		}
	}
	if l.FiveHour == nil && l.Weekly == nil {
		return
	}
	if cur := p.res.Limits; cur == nil || ts >= cur.At {
		p.res.Limits = l
	}
}
