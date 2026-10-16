package codex

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"strings"
)

// envelope is one rollout line. Files Codex writes with paginated history
// (CLI 0.147 and later) number their lines with ordinal.
type envelope struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Ordinal   *int64          `json:"ordinal"`
	Payload   json.RawMessage `json:"payload"`
}

// typed reads the discriminator of a payload.
type typed struct {
	Type string `json:"type"`
}

// sessionMeta is the payload of a session_meta line. A rollout starts with
// its own; a fork copies the session_meta of the thread it came from right
// after it.
type sessionMeta struct {
	ID            string          `json:"id"`
	Cwd           string          `json:"cwd"`
	CLIVersion    string          `json:"cli_version"`
	Originator    string          `json:"originator"`
	ModelProvider string          `json:"model_provider"`
	Source        json.RawMessage `json:"source"`
	Git           *struct {
		Branch string `json:"branch"`
	} `json:"git"`
	ParentThreadID string `json:"parent_thread_id"`
	SessionID      string `json:"session_id"` // the user's thread a subagent belongs to
	AgentNickname  string `json:"agent_nickname"`
	AgentRole      string `json:"agent_role"`
	ForkedFromID   string `json:"forked_from_id"`
	// Forks made since CLI 0.147 copy the history below this ordinal, or
	// none at all when they point at it (history_base).
	SubagentHistoryStart *int64          `json:"subagent_history_start_ordinal"`
	HistoryBase          json.RawMessage `json:"history_base"`
	Instructions         *struct {
		Provenance *struct {
			Model string `json:"model"`
		} `json:"provenance"`
	} `json:"base_instructions"`
}

// spawnSource is the object form of session_meta.source.
type spawnSource struct {
	Subagent json.RawMessage `json:"subagent"`
}

type threadSpawn struct {
	ThreadSpawn *struct {
		ParentThreadID string `json:"parent_thread_id"`
		Depth          int    `json:"depth"`
		AgentNickname  string `json:"agent_nickname"`
		AgentRole      string `json:"agent_role"`
	} `json:"thread_spawn"`
	Other string `json:"other"`
}

// Meta is what a rollout's first line says about its thread.
type Meta struct {
	ID         string // the thread id
	Cwd        string
	Version    string // Codex CLI version
	Originator string // codex-tui, codex_exec, ...
	Provider   string // the model provider: openai, or another one Codex was pointed at
	Branch     string
	// Subagent says what started the thread when another thread did:
	// "spawn" (an agent another agent spawned), "review", or another kind.
	// Empty for a thread a person started.
	Subagent string
	Parent   string // the thread that started it
	Root     string // the user's thread it belongs to, when recorded
	Depth    int
	Role     string
	Nickname string
	// ForkedFrom is the thread whose history this one copied.
	ForkedFrom string
}

func (m sessionMeta) meta() Meta {
	out := Meta{ID: m.ID, Cwd: m.Cwd, Version: m.CLIVersion, Originator: m.Originator, Provider: m.ModelProvider,
		Parent: m.ParentThreadID, Root: m.SessionID, Role: m.AgentRole, Nickname: m.AgentNickname, ForkedFrom: m.ForkedFromID}
	if m.Git != nil {
		out.Branch = m.Git.Branch
	}
	var src spawnSource
	if len(m.Source) > 0 && m.Source[0] == '{' && json.Unmarshal(m.Source, &src) == nil && len(src.Subagent) > 0 {
		var kind string
		var spawn threadSpawn
		switch {
		case json.Unmarshal(src.Subagent, &kind) == nil:
			out.Subagent = kind // "review"
		case json.Unmarshal(src.Subagent, &spawn) == nil && spawn.ThreadSpawn != nil:
			t := spawn.ThreadSpawn
			out.Subagent, out.Depth = "spawn", t.Depth
			if out.Parent == "" {
				out.Parent = t.ParentThreadID
			}
			if out.Role == "" {
				out.Role = t.AgentRole
			}
			if out.Nickname == "" {
				out.Nickname = t.AgentNickname
			}
		case spawn.Other != "":
			out.Subagent = spawn.Other // "guardian"
		default:
			out.Subagent = "agent"
		}
	}
	if out.Root == out.ID {
		out.Root = ""
	}
	return out
}

// errNoMeta is returned for a rollout that does not start with its
// session_meta line.
var errNoMeta = errors.New("rollout does not start with session_meta")

// ReadMeta reads a rollout's first line.
func ReadMeta(path string) (Meta, error) {
	f, err := os.Open(path)
	if err != nil {
		return Meta{}, err
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 64<<10)
	ln, err := br.ReadBytes('\n')
	if err != nil && len(ln) == 0 {
		return Meta{}, err
	}
	var l envelope
	if json.Unmarshal(ln, &l) != nil || l.Type != "session_meta" {
		return Meta{}, errNoMeta
	}
	var m sessionMeta
	if err := json.Unmarshal(l.Payload, &m); err != nil {
		return Meta{}, err
	}
	return m.meta(), nil
}

// usage is a token count. Cached input is part of input and reasoning part
// of output.
type usage struct {
	Input     int64 `json:"input_tokens"`
	Cached    int64 `json:"cached_input_tokens"`
	Output    int64 `json:"output_tokens"`
	Reasoning int64 `json:"reasoning_output_tokens"`
	Total     int64 `json:"total_tokens"`
}

func (u usage) minus(o usage) usage {
	return usage{u.Input - o.Input, u.Cached - o.Cached, u.Output - o.Output, u.Reasoning - o.Reasoning, u.Total - o.Total}
}

// covers reports whether u is a step forward from o on every field.
func (u usage) covers(o usage) bool {
	return u.Total > o.Total && u.Input >= o.Input && u.Cached >= o.Cached && u.Output >= o.Output && u.Reasoning >= o.Reasoning
}

// tokenCount is the payload of an event_msg/token_count line.
type tokenCount struct {
	Info *struct {
		Total usage `json:"total_token_usage"`
		Last  usage `json:"last_token_usage"`
	} `json:"info"`
	RateLimits *rateLimits `json:"rate_limits"`
}

type rateLimits struct {
	LimitID   string      `json:"limit_id"`
	PlanType  string      `json:"plan_type"`
	Primary   *rateWindow `json:"primary"`
	Secondary *rateWindow `json:"secondary"`
}

type rateWindow struct {
	UsedPercent   float64 `json:"used_percent"`
	WindowMinutes int     `json:"window_minutes"`
	ResetsAt      int64   `json:"resets_at"` // unix seconds
}

// content is one part of a message.
type content struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// message is a response_item/message payload.
type message struct {
	Role        string    `json:"role"`
	Content     []content `json:"content"`
	Passthrough *struct {
		// Since 2026-08-27 each content part is tagged with where it came
		// from; "user.text" is what a person typed.
		Kinds []string `json:"content_item_kinds"`
	} `json:"internal_chat_message_metadata_passthrough"`
}

// injected marks text Codex adds to the conversation as if the user had
// sent it: instructions, environment, notifications.
var injected = []string{
	"<environment_context>", "# AGENTS.md instructions", "<user_instructions>", "<recommended_plugins>",
	"<hook_prompt>", "<subagent_notification>", "<turn_aborted>", "<codex_internal_context>",
	"<goal_context>", "<skill>", "<user_action>", "<permissions", "<skills_instructions>",
	"<apps_instructions>", "<plugins_instructions>", "<multi_agent_mode>", "<identity>",
	"<collaboration_mode>", "<model_switch>", "<personality_spec>", "<app-context>",
}

func isInjected(text string) bool {
	t := strings.TrimSpace(text)
	for _, p := range injected {
		if strings.HasPrefix(t, p) {
			return true
		}
	}
	return false
}

// typedText returns what a person typed in a user message, and the images
// attached, or ok false when the message is text Codex added.
func (m message) typedText() (text string, images int, ok bool) {
	if m.Role != "user" {
		return "", 0, false
	}
	var parts []string
	for i, c := range m.Content {
		switch c.Type {
		case "input_image":
			images++
			continue
		case "input_text":
		default:
			continue
		}
		if m.Passthrough != nil && len(m.Passthrough.Kinds) == len(m.Content) {
			if m.Passthrough.Kinds[i] != "user.text" {
				continue
			}
		} else if isInjected(c.Text) {
			continue
		}
		parts = append(parts, c.Text)
	}
	text = strings.TrimSpace(strings.Join(parts, "\n\n"))
	return text, images, text != ""
}

// item is the item of an event_msg/item_completed line (CLI 0.147 and
// later), which carries what earlier versions wrote as separate events.
type item struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	// CollabAgentToolCall: the agents a collaboration call reached.
	ReceiverThreadIDs []string `json:"receiver_thread_ids"`
	// SubAgentActivity: an agent this thread started or reached.
	AgentThreadID string `json:"agent_thread_id"`
	Kind          string `json:"kind"`
}

// userText returns the text of a UserMessage item and its images.
func (it item) userText() (string, int) {
	var parts []string
	images := 0
	for _, c := range it.Content {
		switch strings.ToLower(c.Type) {
		case "text":
			parts = append(parts, c.Text)
		case "local_image", "image":
			images++
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n\n")), images
}

// toolName joins a function call's namespace and name the way Claude Code
// names MCP tools (mcp__<server>__<tool>), whichever of Codex's three
// spellings a file uses.
func toolName(namespace, name string) string {
	ns := strings.TrimSuffix(namespace, "__")
	if strings.HasPrefix(ns, "mcp__") && !strings.HasPrefix(name, "mcp__") {
		return ns + "__" + name
	}
	return name
}
