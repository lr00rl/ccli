package index

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/lr00rl/ccli/internal/account"
	"github.com/lr00rl/ccli/internal/pi"
	"github.com/lr00rl/ccli/internal/store"
)

// scanPi lists session JSONL files under <agent>/sessions. The session id
// and cwd come from the header line. A file that is still empty is left
// for the next run.
//
// A pi subagent is a session of its own, run by the pi-subagents extension
// and written beside its parent's file:
//
//	<cwd dir>/<parent file stem>/<run id>/run-<n>/session.jsonl
//
// Its header names the parent's file in parentSession. Such a file is indexed
// as a subagent of the parent session, with its own session id as the agent
// id, so the parent's row carries its children, as a Codex thread does. The
// extension's <cwd dir>/subagent-artifacts/<run id>_<agent>_meta.json names
// the agent (web-researcher, delegate, ...), which becomes its type.
func scanPi(a account.Account) ([]discovered, error) {
	root := filepath.Join(a.Dir, "sessions")
	var found []discovered
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if d == nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			return nil
		}
		if d.IsDir() {
			// The extension's artifacts are copies of runs indexed elsewhere.
			if d.Name() == "subagent-artifacts" {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || filepath.Ext(d.Name()) != ".jsonl" {
			return nil
		}
		h, ok := pi.ReadHeader(path)
		if !ok {
			return nil
		}
		st, err := d.Info()
		if err != nil {
			return nil
		}
		if parent := piParent(path, h.ParentSession); parent != "" {
			found = append(found, discovered{
				tool:      account.Pi,
				cwd:       h.Cwd,
				file:      storeFile(a, path, parent, "subagent", h.ID, st),
				agentType: piAgentType(path),
			})
			return nil
		}
		found = append(found, discovered{
			tool: account.Pi,
			cwd:  h.Cwd,
			file: storeFile(a, path, h.ID, "main", "", st),
		})
		return nil
	})
	return found, err
}

// piParent is the session id of the session a pi subagent run belongs to,
// or "" when path is not one. A run sits at <parent stem>/<run id>/run-<n>/
// session.jsonl and names its parent's file in its header; both must agree,
// so a fork or a session that merely continues another (which also carries
// parentSession) stays a session of its own.
func piParent(path, parentFile string) string {
	if parentFile == "" || filepath.Base(path) != "session.jsonl" {
		return ""
	}
	runDir := filepath.Dir(path)
	if !strings.HasPrefix(filepath.Base(runDir), "run-") {
		return ""
	}
	stem := filepath.Dir(filepath.Dir(runDir))
	if strings.TrimSuffix(filepath.Base(parentFile), ".jsonl") != filepath.Base(stem) {
		return ""
	}
	return pi.FileSessionID(parentFile)
}

// piAgentType is the agent a pi subagent run ran as, from the extension's
// meta file for its run id, or "subagent" when there is none.
func piAgentType(path string) string {
	runID := filepath.Base(filepath.Dir(filepath.Dir(path)))
	cwdDir := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(path))))
	metas, _ := filepath.Glob(filepath.Join(cwdDir, "subagent-artifacts", runID+"_*_meta.json"))
	for _, m := range metas {
		b, err := os.ReadFile(m)
		if err != nil {
			continue
		}
		var v struct {
			Agent string `json:"agent"`
		}
		if json.Unmarshal(b, &v) == nil && v.Agent != "" {
			return v.Agent
		}
	}
	return "subagent"
}

// piAgentMeta is what a pi subagent row says about itself.
func piAgentMeta(agentType, firstPrompt string) *store.AgentMeta {
	m := &store.AgentMeta{AgentType: agentType}
	if m.AgentType == "" {
		m.AgentType = "subagent"
	}
	if firstPrompt != "" {
		m.Description = clean(firstPrompt, 200)
	}
	return m
}
