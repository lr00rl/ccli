package cursorcli

import (
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"strings"

	"github.com/lr00rl/ccli/internal/sqlitero"
)

// StoreInfo is the chat metadata Cursor keeps in store.db.
type StoreInfo struct {
	Model     string
	Parent    string
	Root      string
	ToolCall  string
	AgentType string
	// Unread is set when store.db exists but could not be read, so the
	// fields above say nothing about the chat.
	Unread bool
}

// StoreModel reads lastUsedModel from a chat's store.db. The read creates no
// file next to a store Cursor has open (see sqlitero). An absent or
// unreadable store returns "".
func StoreModel(path string) string { return ReadStore(path).Model }

// ReadStore reads the model and, when this chat is a subagent, who started it.
// A missing store.db, or one with no metadata yet, is a chat with no such
// details; one that exists but cannot be read is Unread.
func ReadStore(path string) StoreInfo {
	if path == "" {
		return StoreInfo{}
	}
	if _, err := os.Stat(path); err != nil {
		return StoreInfo{Unread: !errors.Is(err, fs.ErrNotExist)}
	}
	db, err := sqlitero.Open(path)
	if err != nil {
		return StoreInfo{Unread: true}
	}
	defer db.Close()
	var raw []byte
	if err := db.QueryRow(`SELECT value FROM meta WHERE key = '0'`).Scan(&raw); err != nil || len(raw) == 0 {
		return StoreInfo{Unread: err != nil && !errors.Is(err, sql.ErrNoRows)}
	}
	text := string(raw)
	if decoded, err := hex.DecodeString(strings.TrimSpace(text)); err == nil {
		text = string(decoded)
	}
	var meta struct {
		Model string `json:"lastUsedModel"`
		Info  *struct {
			Parent    string `json:"parentAgentId"`
			Root      string `json:"rootParentAgentId"`
			ToolCall  string `json:"toolCallId"`
			TypeName  string `json:"typeName"`
			AgentType string `json:"subagentTypeName"`
		} `json:"subagentInfo"`
	}
	if json.Unmarshal([]byte(text), &meta) != nil {
		return StoreInfo{Unread: true}
	}
	out := StoreInfo{Model: strings.TrimSpace(meta.Model)}
	if meta.Info != nil {
		out.Parent = meta.Info.Parent
		out.Root = meta.Info.Root
		out.ToolCall = meta.Info.ToolCall
		out.AgentType = meta.Info.TypeName
		if out.AgentType == "" {
			out.AgentType = meta.Info.AgentType
		}
	}
	return out
}
