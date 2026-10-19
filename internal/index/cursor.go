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
		var session, agent, kind, chat string
		switch {
		case len(parts) == 4 && strings.TrimSuffix(parts[3], ".jsonl") == parts[2]:
			chat, session, kind = parts[2], parts[2], "main"
		case len(parts) == 5 && parts[3] == "subagents":
			chat = strings.TrimSuffix(parts[4], ".jsonl")
			session, agent, kind = parts[2], chat, "subagent"
		case len(parts) == 3:
			chat = strings.TrimSuffix(parts[2], ".jsonl")
			session, kind = chat, "main"
		default:
			return nil
		}
		own := metas[chat]
		parentAgent, agentType, toolUse := "", own.kind, own.tool
		if kind == "main" && own.sub {
			rootID := own.root
			if rootID == "" {
				rootID = own.parent
			}
			if rootID != "" && rootID != chat {
				if own.parent != "" && own.parent != rootID && own.parent != chat {
					parentAgent = own.parent
				}
				session, agent, kind = rootID, chat, "subagent"
			}
		}
		if kind == "subagent" && len(parts) == 5 {
			if folder, ok := metas[parts[2]]; ok && folder.sub && parts[2] != session {
				rootID := folder.root
				if rootID == "" {
					rootID = folder.parent
				}
				if rootID != "" && rootID != parts[2] {
					if parentAgent == "" {
						parentAgent = parts[2]
					}
					session = rootID
				}
			}
		}
		title := own.title
		if title == "" {
			title = metas[session].title
		}
		cwd := own.cwd
		if cwd == "" {
			cwd = metas[session].cwd
		}
		if cwd == "" {
			cwd = filepath.Join(root, slug)
		}
		started, ended, model := own.created, own.updated, own.model
		if started == 0 && ended == 0 {
			started, ended = metas[session].created, metas[session].updated
		}
		found = append(found, discovered{
			tool: account.Cursor, cwd: cwd, title: title, model: model, started: started, ended: ended,
			agentType: agentType, toolUse: toolUse, parentAgent: parentAgent,
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
	best := map[string]discovered{}
	for _, s := range subs {
		main, ok := kept[s.file.SessionID]
		if !ok || s.file.AgentID == "" {
			continue
		}
		beside := filepath.Dir(filepath.Dir(s.file.Path)) == dir[s.file.SessionID]
		prev, seen := best[s.file.AgentID]
		if !seen {
			best[s.file.AgentID] = s
			continue
		}
		prevBeside := filepath.Dir(filepath.Dir(prev.file.Path)) == filepath.Dir(main.file.Path)
		if prevBeside && !beside {
			continue
		}
		if beside == prevBeside && prev.file.MTime >= s.file.MTime {
			continue
		}
		best[s.file.AgentID] = s
	}
	for _, s := range best {
		out = append(out, s)
	}
	return out
}

type cursorMeta struct {
	cwd                      string
	title, model             string
	created, updated         int64
	sub                      bool
	parent, root, tool, kind string
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
		info := cursorcli.ReadStore(filepath.Join(dir, "store.db"))
		out[filepath.Base(dir)] = cursorMeta{
			cwd: v.Cwd, title: v.Title, model: info.Model,
			created: v.Created, updated: v.Updated, sub: v.Subagent,
			parent: info.Parent, root: info.Root, tool: info.ToolCall, kind: info.AgentType,
		}
		return nil
	})
	return out
}
