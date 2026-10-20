package cursorcli

import (
	"encoding/hex"
	"encoding/json"
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
}

// StoreModel reads lastUsedModel from a chat's store.db. The read creates no
// file next to a store Cursor has open (see sqlitero). An absent or
// unreadable store returns "".
func StoreModel(path string) string { return ReadStore(path).Model }

// ReadStore reads the model and, when this chat is a subagent, who started it.
func ReadStore(path string) StoreInfo {
	if path == "" {
		return StoreInfo{}
	}
	db, err := sqlitero.Open(path)
	if err != nil {
		return StoreInfo{}
	}
	defer db.Close()
	var raw []byte
	if err := db.QueryRow(`SELECT value FROM meta WHERE key = '0'`).Scan(&raw); err != nil || len(raw) == 0 {
		return StoreInfo{}
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
		return StoreInfo{}
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
