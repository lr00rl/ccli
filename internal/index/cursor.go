package index

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/lr00rl/ccli/internal/account"
	"github.com/lr00rl/ccli/internal/cursorcli"
)

// scanCursor lists Cursor CLI transcripts. A nested
// <id>/<id>.jsonl is the conversation; subagents/<id>.jsonl belongs to it.
// meta.json under chats/ supplies the real working directory when Cursor
// wrote one. Without it, the project is the transcript's project directory.
func scanCursor(a account.Account) ([]discovered, error) {
	metas := cursorMetas(filepath.Join(a.Dir, "chats"))
	root := filepath.Join(a.Dir, "projects")
	var found []discovered
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if d == nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			return nil
		}
		if d.IsDir() || filepath.Ext(d.Name()) != ".jsonl" || !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		parts := strings.Split(rel, string(filepath.Separator))
		// projects/<slug>/agent-transcripts/...
		if len(parts) < 3 || parts[1] != "agent-transcripts" {
			return nil
		}
		slug := parts[0]
		st, err := d.Info()
		if err != nil {
			return nil
		}
		var session, agent, kind string
		switch {
		case len(parts) == 4 && strings.TrimSuffix(parts[3], ".jsonl") == parts[2]:
			session, kind = parts[2], "main"
		case len(parts) == 5 && parts[3] == "subagents":
			session, agent, kind = parts[2], strings.TrimSuffix(parts[4], ".jsonl"), "subagent"
		case len(parts) == 3:
			session, kind = strings.TrimSuffix(parts[2], ".jsonl"), "main"
		default:
			return nil
		}
		m := metas[session]
		title := m.title
		if kind == "subagent" {
			if sub, ok := metas[agent]; ok && sub.title != "" {
				title = sub.title
			}
		}
		cwd := m.cwd
		if cwd == "" {
			cwd = filepath.Join(root, slug)
		}
		found = append(found, discovered{
			tool: account.Cursor, cwd: cwd, title: title, model: m.model, started: m.created, ended: m.updated,
			file: storeFile(a, path, session, kind, agent, st),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return dedupeCursor(found), nil
}

// dedupeCursor keeps one main transcript per conversation id. Cursor
// sometimes leaves the same id under two project directories; the newer
// file is the one resume opens. Subagents are kept only beside that file.
func dedupeCursor(found []discovered) []discovered {
	kept := map[string]discovered{}
	var subs []discovered
	for _, d := range found {
		if d.file.Kind != "main" {
			subs = append(subs, d)
			continue
		}
		old, ok := kept[d.file.SessionID]
		if !ok || d.file.MTime >= old.file.MTime {
			kept[d.file.SessionID] = d
		}
	}
	out := make([]discovered, 0, len(kept)+len(subs))
	dir := map[string]string{}
	for id, d := range kept {
		out = append(out, d)
		dir[id] = filepath.Dir(d.file.Path)
	}
	seenAgent := map[string]bool{}
	for _, s := range subs {
		if filepath.Dir(filepath.Dir(s.file.Path)) != dir[s.file.SessionID] {
			continue
		}
		if seenAgent[s.file.AgentID] {
			continue
		}
		seenAgent[s.file.AgentID] = true
		out = append(out, s)
	}
	return out
}

type cursorMeta struct {
	cwd              string
	title, model     string
	created, updated int64
}

func cursorMetas(root string) map[string]cursorMeta {
	out := map[string]cursorMeta{}
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != "meta.json" {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		var v struct {
			Cwd      string `json:"cwd"`
			Title    string `json:"title"`
			Created  int64  `json:"createdAtMs"`
			Updated  int64  `json:"updatedAtMs"`
			Subagent bool   `json:"isSubagent"`
		}
		if json.Unmarshal(b, &v) != nil {
			return nil
		}
		dir := filepath.Dir(path)
		out[filepath.Base(dir)] = cursorMeta{
			cwd: v.Cwd, title: v.Title, model: cursorcli.StoreModel(filepath.Join(dir, "store.db")),
			created: v.Created, updated: v.Updated,
		}
		return nil
	})
	return out
}
