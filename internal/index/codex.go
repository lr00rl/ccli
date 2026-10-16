package index

import (
	"errors"
	"io/fs"
	"path/filepath"
	"time"

	"github.com/lr00rl/ccli/internal/account"
	"github.com/lr00rl/ccli/internal/codex"
	"github.com/lr00rl/ccli/internal/quota"
	"github.com/lr00rl/ccli/internal/store"
	"github.com/lr00rl/ccli/internal/transcript"
)

// scanCodex lists a Codex home's rollout files: sessions/YYYY/MM/DD/ and
// archived_sessions/, where Codex moves the threads archived in it.
//
// Each rollout is one thread. A thread another thread started (a subagent)
// belongs to the session of the user's thread it descends from, so for a
// file not yet in the index scanCodex reads its first line to tell. A file
// whose first line is not there yet waits for the next run.
func scanCodex(a account.Account, known map[string]store.File) ([]discovered, error) {
	var found []discovered
	metas := map[string]codex.Meta{} // new files, by path
	for _, root := range []string{filepath.Join(a.Dir, "sessions"), filepath.Join(a.Dir, "archived_sessions")} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) || d != nil && d.IsDir() {
					return nil // a folder that vanished or cannot be read is left out
				}
				return err
			}
			if d.IsDir() {
				return nil
			}
			id, ok := codex.SessionID(d.Name())
			if !ok || !d.Type().IsRegular() {
				return nil
			}
			st, err := d.Info()
			if err != nil {
				return nil
			}
			f := store.File{Path: path, Account: a.Name, SessionID: id, Kind: "main",
				Size: st.Size(), MTime: st.ModTime().UnixMilli(), Born: birth(path, st)}
			if old, ok := known[path]; ok {
				f.SessionID, f.Kind, f.AgentID = old.SessionID, old.Kind, old.AgentID
			} else {
				m, err := codex.ReadMeta(path)
				if err != nil || m.ID != id {
					return nil
				}
				metas[path] = m
			}
			found = append(found, discovered{tool: account.Codex, file: f})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	// A subagent's session is the user's thread it descends from. Newer
	// rollouts name it; for older ones, follow the parents.
	rootOf := map[string]string{} // thread id -> its session
	parentOf := map[string]string{}
	for _, f := range known {
		if f.Account != a.Name {
			continue
		}
		thread := f.SessionID
		if f.Kind == "subagent" {
			thread = f.AgentID
		}
		rootOf[thread] = f.SessionID
	}
	for _, m := range metas {
		switch {
		case m.Subagent == "":
			rootOf[m.ID] = m.ID
		case m.Root != "":
			rootOf[m.ID] = m.Root
		case m.Parent != "":
			parentOf[m.ID] = m.Parent
		}
	}
	var resolve func(thread string, depth int) string
	resolve = func(thread string, depth int) string {
		if r, ok := rootOf[thread]; ok {
			return r
		}
		p, ok := parentOf[thread]
		if !ok || depth > 16 {
			return thread // the oldest thread on record stands for the session
		}
		r := resolve(p, depth+1)
		rootOf[thread] = r
		return r
	}
	for i := range found {
		m, ok := metas[found[i].file.Path]
		if !ok || m.Subagent == "" {
			continue
		}
		f := &found[i].file
		f.Kind, f.AgentID, f.SessionID = "subagent", m.ID, resolve(m.ID, 0)
	}
	return found, nil
}

// codexAgent is what the index keeps about a Codex subagent, from its first
// line. The agent that started it is its parent unless that is the user's
// thread itself.
func codexAgent(m *codex.Meta, session string) *store.AgentMeta {
	typ := m.Role
	if typ == "" {
		typ = m.Subagent
		if typ == "spawn" {
			typ = "agent"
		}
	}
	a := &store.AgentMeta{AgentType: typ, SpawnDepth: m.Depth}
	if m.Parent != session {
		a.ParentAgent = m.Parent
	}
	return a
}

// saveLimits keeps each account's latest rate-limit reading as its quota
// snapshot; quota.Save keeps a newer snapshot already there. Quota is a
// convenience, so a snapshot that cannot be written does not fail the run.
func saveLimits(dir string, accts []account.Account, limits map[string]*transcript.Limits) {
	for _, a := range accts {
		l := limits[a.Name]
		if l == nil {
			continue
		}
		s := quota.Snapshot{Account: a.Name, ConfigDir: a.Dir, CapturedAt: time.UnixMilli(l.At), Source: quota.FromCodexLog, Plan: l.Plan}
		if w := l.FiveHour; w != nil {
			s.FiveHour = &quota.Window{UsedPercent: w.UsedPercent, ResetsAt: time.Unix(w.ResetsAt, 0)}
		}
		if w := l.Weekly; w != nil {
			s.SevenDay = &quota.Window{UsedPercent: w.UsedPercent, ResetsAt: time.Unix(w.ResetsAt, 0)}
		}
		_ = quota.Save(dir, s)
	}
}
