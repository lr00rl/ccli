package index

import (
	"fmt"
	"os"

	"github.com/lr00rl/ccli/internal/account"
	"github.com/lr00rl/ccli/internal/cursoride"
	"github.com/lr00rl/ccli/internal/store"
)

// listComposers is cursoride.List; tests count the calls.
var listComposers = cursoride.List

// scanCursorIDE lists composers in the IDE database. A subagent is filed
// under its root conversation. The path is "<db>#<id>" and is not a file
// on disk; parsing reads the database.
func scanCursorIDE(a account.Account) ([]discovered, error) {
	heads, err := listComposers(a.Dir)
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

// ideStamp says when a Cursor IDE database changed: the size and time of
// the database and its WAL, the database header with its change counter,
// which every commit in rollback mode moves, and the WAL header, whose salt
// changes when the WAL starts over. A commit in WAL mode grows the WAL. The
// headers catch the writes a coarse file clock would miss.
func ideStamp(user string) string {
	db := cursoride.DBPath(user)
	var b []byte
	for _, part := range []struct {
		path string
		head int
	}{{db, 100}, {db + "-wal", 32}} {
		st, err := os.Stat(part.path)
		if err != nil {
			b = append(b, "-;"...)
			continue
		}
		b = fmt.Appendf(b, "%d:%d:", st.Size(), st.ModTime().UnixNano())
		if f, err := os.Open(part.path); err == nil {
			head := make([]byte, part.head)
			n, _ := f.ReadAt(head, 0)
			f.Close()
			b = fmt.Appendf(b, "%x", head[:n])
		}
		b = append(b, ';')
	}
	return string(b)
}

// unchangedIDE lists an IDE account's composers as the index holds them,
// when the database has the stamp it had when last read, so a database of
// gigabytes is not listed again for nothing. A row that a newer parser must
// read again turns it down. A composer marked missing stays missing.
func unchangedIDE(a account.Account, stamp, last string, known map[string]store.File) ([]discovered, bool) {
	if stamp != last {
		return nil, false
	}
	var found []discovered
	for _, f := range known {
		if f.Account != a.Name || f.Missing {
			continue
		}
		if f.Parser != fileParser(account.CursorIDE) {
			return nil, false
		}
		found = append(found, discovered{tool: account.CursorIDE, file: f})
	}
	return found, true
}
