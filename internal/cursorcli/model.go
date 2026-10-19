package cursorcli

import (
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"strings"

	_ "modernc.org/sqlite"
)

// StoreModel reads lastUsedModel from a chat's store.db. The file is opened
// immutable so a read does not create a -shm next to a store Cursor has
// open. An absent or unreadable store returns "".
func StoreModel(path string) string {
	if path == "" {
		return ""
	}
	u := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro&immutable=1"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return ""
	}
	defer db.Close()
	var raw []byte
	if err := db.QueryRow(`SELECT value FROM meta WHERE key = '0'`).Scan(&raw); err != nil || len(raw) == 0 {
		return ""
	}
	text := string(raw)
	if decoded, err := hex.DecodeString(strings.TrimSpace(text)); err == nil {
		text = string(decoded)
	}
	var meta struct {
		Model string `json:"lastUsedModel"`
	}
	if json.Unmarshal([]byte(text), &meta) != nil {
		return ""
	}
	return strings.TrimSpace(meta.Model)
}
