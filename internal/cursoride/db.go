package cursoride

import (
	"database/sql"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/lr00rl/ccli/internal/sqlitero"
)

// one reader at a time: the database is a few gigabytes, and several
// connections at once cost more than they save.
var gate sync.Mutex

func open(dbPath string) (*sql.DB, error) {
	db, err := sqlitero.Open(dbPath)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// Split separates the index path "<db>#<composerId>".
func Split(path string) (db, id string, ok bool) {
	i := strings.LastIndex(path, "#")
	if i <= 0 || i == len(path)-1 {
		return "", "", false
	}
	return path[:i], path[i+1:], true
}

// Path is the index path of one composer in a database.
func Path(db, id string) string { return db + "#" + id }

// DBPath is state.vscdb inside a Cursor user directory.
func DBPath(user string) string {
	return filepath.Join(user, "globalStorage", "state.vscdb")
}

func userDir(db string) string {
	// <User>/globalStorage/state.vscdb
	return filepath.Dir(filepath.Dir(db))
}

func workspacePath(user, id string) string {
	if id == "" || id == "empty-window" {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(user, "workspaceStorage", id, "workspace.json"))
	if err != nil {
		return ""
	}
	var v struct {
		Workspace string `json:"workspace"`
		Folder    string `json:"folder"`
	}
	if json.Unmarshal(b, &v) != nil {
		return ""
	}
	if p := fileURI(v.Folder); p != "" {
		return p
	}
	// A multi-root workspace names its .code-workspace file; the folder that
	// holds it stands for the project.
	if p := fileURI(v.Workspace); p != "" {
		return filepath.Dir(p)
	}
	return ""
}

func fileURI(s string) string {
	if s == "" {
		return ""
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "file" {
		return ""
	}
	p := u.Path
	if runtime.GOOS == "windows" && len(p) >= 3 && p[0] == '/' && p[2] == ':' {
		p = p[1:]
	}
	return p
}

// Composer is what a composer's header says about its state now.
type Composer struct {
	// ContextPercent is how full the model's context window is
	// (contextUsagePercent); nil when the header has none.
	ContextPercent *float64 `json:"context_used_percent,omitempty"`
	// LinesAdded, LinesRemoved and FilesChanged are Cursor's own count of
	// the chat's changes. Some chats that edited files read zero here.
	LinesAdded   int `json:"lines_added"`
	LinesRemoved int `json:"lines_removed"`
	FilesChanged int `json:"files_changed"`
}

// ReadComposer reads the header of one composer. path is "<db>#<composerId>".
func ReadComposer(path string) (Composer, error) {
	var c Composer
	dbPath, id, ok := Split(path)
	if !ok {
		return c, os.ErrNotExist
	}
	gate.Lock()
	defer gate.Unlock()
	db, err := open(dbPath)
	if err != nil {
		return c, err
	}
	defer db.Close()
	var raw []byte
	if err := db.QueryRow(`SELECT value FROM composerHeaders WHERE composerId = ?`, id).Scan(&raw); err != nil {
		return c, err
	}
	var v struct {
		Context *float64 `json:"contextUsagePercent"`
		Added   int      `json:"totalLinesAdded"`
		Removed int      `json:"totalLinesRemoved"`
		Files   int      `json:"filesChangedCount"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return c, err
	}
	return Composer{ContextPercent: v.Context, LinesAdded: v.Added, LinesRemoved: v.Removed, FilesChanged: v.Files}, nil
}
