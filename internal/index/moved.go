package index

import "github.com/lr00rl/ccli/internal/store"

// staleCopy is a row whose file is gone and whose transcript is the file at
// Keep, which has a row of its own.
type staleCopy struct {
	ID         int64
	Path, Keep string
}

// claudeMoves follows transcripts Claude Code moved to another project
// directory. A session that enters or leaves a worktree has its transcript
// moved between the two folders' project directories under the same name,
// so one path vanishes and another with the same session appears. A file
// whose session, kind and agent match a vanished row, and that starts with
// the bytes that row read, is that transcript. At a path the index does not
// know, the row moves to it and keeps what was read. At a path the index
// already knows (the transcript went back to where it was before, and ccli
// saw both), the vanished row is a stale copy: it is dropped and the file
// read again, so their work is counted once.
func claudeMoves(acct string, found []discovered, known map[string]store.File) ([]moved, []staleCopy) {
	here := make(map[string]bool, len(found))
	for _, d := range found {
		here[d.file.Path] = true
	}
	type key struct{ session, kind, agent string }
	gone := map[key][]store.File{}
	for p, f := range known {
		if f.Account == acct && !here[p] && f.HeadHash != "" {
			k := key{f.SessionID, f.Kind, f.AgentID}
			gone[k] = append(gone[k], f)
		}
	}
	if len(gone) == 0 {
		return nil, nil
	}
	var moves []moved
	var stale []staleCopy
	claimed := map[int64]bool{}
	for _, d := range found {
		f := d.file
		olds := gone[key{f.SessionID, f.Kind, f.AgentID}]
		if len(olds) == 0 {
			continue
		}
		has := known[f.Path].ID != 0
		for _, old := range olds {
			if claimed[old.ID] || old.Offset > f.Size {
				continue
			}
			if h, err := headHash(f.Path, old.Offset); err != nil || h != old.HeadHash {
				continue
			}
			claimed[old.ID] = true
			if has {
				stale = append(stale, staleCopy{ID: old.ID, Path: old.Path, Keep: f.Path})
				continue
			}
			moves = append(moves, moved{ID: old.ID, From: old.Path, To: f.Path, Key: f.ProjectKey})
			has = true
		}
	}
	return moves, stale
}
