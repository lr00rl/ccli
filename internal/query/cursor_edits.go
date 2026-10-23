package query

import (
	"context"
	"os"
	"path/filepath"

	"github.com/lr00rl/ccli/internal/account"
	"github.com/lr00rl/ccli/internal/cursoredits"
	"github.com/lr00rl/ccli/internal/cursoride"
)

// fillCursor adds what Cursor records beside the conversation: the files
// its agents wrote, the main chat's and its subagents', and for an IDE chat
// the header's context and change figures. Both are side reads; one that
// fails leaves its part out rather than fail the session.
func (d *DB) fillCursor(ctx context.Context, det *SessionDetail) {
	if det.Tool != account.Cursor && det.Tool != account.CursorIDE {
		return
	}
	ids := []string{det.ID}
	for _, f := range det.Files {
		if f.Kind == "subagent" && f.AgentID != "" && f.AgentID != det.ID {
			ids = append(ids, f.AgentID)
		}
	}
	if e, err := cursoredits.Read(ctx, d.trackingDB(), ids); err == nil && len(e.Files) > 0 {
		det.Edits = &e
	}
	if det.Tool != account.CursorIDE {
		return
	}
	for _, f := range det.Files {
		if f.Kind == "main" {
			if c, err := cursoride.ReadComposer(f.Path); err == nil {
				det.Composer = &c
			}
			break
		}
	}
}

// trackingDB is Cursor's AI code tracking database. It lives in the Cursor
// CLI data directory, ~/.cursor, which the IDE writes to as well.
func (d *DB) trackingDB() string {
	for _, a := range d.accts {
		if a.Kind() != account.Cursor {
			continue
		}
		if p := cursoredits.DBPath(a.Dir); exists(p) {
			return p
		}
	}
	home, _ := os.UserHomeDir()
	return cursoredits.DBPath(filepath.Join(home, ".cursor"))
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
