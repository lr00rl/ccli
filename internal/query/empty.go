package query

import (
	"context"
	"sort"
	"time"

	"github.com/lr00rl/ccli/internal/account"
)

// Empty lists the sessions the filter selects that hold nothing: no prompt,
// no reply, no tool call, no tokens or cost, no subagent or workflow run, no
// name given by a person or by the tool, not running, and last active at
// least idle before now. A name means there is something ccli cannot see,
// such as a Cursor chat whose messages it cannot read. With unanswered, a
// session whose prompts never got a reply counts too. Sessions a person hid
// or tagged are left alone, and so are unused Cursor IDE drafts, which lists
// leave out already. A session with no recorded time counts as idle only for
// Claude Code, where the caller checks the transcript's own time. The oldest
// come first.
//
// A transcript that stopped before any line named its folder takes the
// folder another session of its project folder recorded, so the folder
// filter can find it.
func (d *DB) Empty(ctx context.Context, f Filter, unanswered bool, idle time.Duration, now time.Time) ([]SessionRow, error) {
	dirs := f.Dirs
	f.Dirs = nil
	rows, err := d.loadSessions(ctx, f)
	if err != nil {
		return nil, err
	}
	cwds := map[[2]string]string{}
	for _, s := range rows {
		if s.Cwd != "" && s.ProjectKey != "" {
			cwds[[2]string{s.Account, s.ProjectKey}] = s.Cwd
		}
	}
	var out []SessionRow
	for _, s := range rows {
		if s.Cwd == "" {
			s.Cwd = cwds[[2]string{s.Account, s.ProjectKey}]
		}
		if len(dirs) > 0 && UnderAny(s.Cwd, dirs) == "" {
			continue
		}
		if ideDraft(s) || s.Live || s.Hidden || s.Tag != "" {
			continue
		}
		if s.Last.IsZero() && s.Tool != account.Claude || now.Sub(s.Last) < idle {
			continue
		}
		if s.TitleSource != "none" && s.TitleSource != "prompt" {
			continue
		}
		if s.Replies > 0 || s.ToolCalls > 0 || s.Tokens.All() > 0 || s.Cost.USD > 0 || s.Cost.Unlogged > 0 ||
			s.Subagents > 0 || s.Runs > 0 {
			continue
		}
		if !unanswered && s.Prompts+s.OtherPrompts > 0 {
			continue
		}
		out = append(out, s)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Last.Before(out[j].Last) })
	return out, nil
}
