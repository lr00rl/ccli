package index

import (
	"fmt"
	"os"

	"github.com/lr00rl/ccli/internal/account"
	"github.com/lr00rl/ccli/internal/cursoride"
	"github.com/lr00rl/ccli/internal/store"
)

// scanCursorIDE lists composers in the IDE database. A subagent is filed
// under its root conversation. The path is "<db>#<id>" and is not a file
// on disk; parsing reads the database.
func scanCursorIDE(a account.Account) ([]discovered, error) {
	heads, err := cursoride.List(a.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	db := cursoride.DBPath(a.Dir)
	var found []discovered
	for _, h := range heads {
		kind, session, agent := "main", h.ID, ""
		if h.Sub && h.Root != "" && h.Root != h.ID {
			kind, session, agent = "subagent", h.Root, h.ID
		}
		stamp := h.Updated
		if stamp == 0 {
			stamp = h.Created
		}
		if stamp == 0 {
			stamp = 1
		}
		found = append(found, discovered{
			tool: account.CursorIDE, cwd: h.Cwd, title: h.Title, started: h.Created, ended: h.Updated,
			file: store.File{
				Path: cursoride.Path(db, h.ID), Account: a.Name, SessionID: session, ProjectKey: h.Cwd,
				Kind: kind, AgentID: agent, Size: stamp, MTime: stamp, Born: h.Created,
				HeadHash: fmt.Sprintf("%d:%d:%d", h.Bubbles, h.LastRow, h.Bytes),
			},
		})
	}
	return found, nil
}
