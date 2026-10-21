package transfer

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/lr00rl/ccli/internal/account"
)

// Archive moves a session's files out of its account into
// archiveDir/<now>/<account>/, keeping their paths relative to the account,
// with the same checks move uses. It returns that folder and the paths it
// moved. The transcript moves last, so a failure leaves the session in the
// account.
func Archive(src Files, archiveDir string, now time.Time) (string, []string, error) {
	root, err := archiveRoot(archiveDir, src.Account.Name, now)
	if err != nil {
		return "", nil, err
	}
	moved, err := archiveSource(src, archiveDir, root)
	return root, moved, err
}

// Restore moves what Archive put under root back into the account. It
// takes only session paths (projects/<key>/<id>.jsonl, projects/<key>/<id>/
// and file-history/<id>/) and refuses anything else under root. A path the
// account already has is left where it is and reported as a conflict.
func Restore(root string, a account.Account) (restored, conflicts []string, err error) {
	var dirs, transcripts []string
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		parts := strings.Split(rel, string(filepath.Separator))
		switch {
		case rel == "." || (len(parts) == 1 && (parts[0] == "projects" || parts[0] == "file-history") && d.IsDir()):
			return nil
		case len(parts) == 2 && parts[0] == "projects" && d.IsDir() && validKey(parts[1]):
			return nil
		case len(parts) == 2 && parts[0] == "file-history" && d.IsDir() && validID(parts[1]):
			dirs = append(dirs, rel)
		case len(parts) == 3 && parts[0] == "projects" && validKey(parts[1]) && d.IsDir() && validID(parts[2]):
			dirs = append(dirs, rel)
		case len(parts) == 3 && parts[0] == "projects" && validKey(parts[1]) && d.Type().IsRegular() &&
			strings.HasSuffix(parts[2], ".jsonl") && validID(strings.TrimSuffix(parts[2], ".jsonl")):
			transcripts = append(transcripts, rel)
		default:
			return fmt.Errorf("restore %s: %s is not part of an archived session", root, rel)
		}
		if d.IsDir() {
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	// Folders first, so a restored transcript has its subagents beside it.
	sort.Strings(dirs)
	sort.Strings(transcripts)
	for _, rel := range append(dirs, transcripts...) {
		dst, err := joinWithin(a.Dir, rel)
		if err != nil {
			return restored, conflicts, fmt.Errorf("restore %s: %w", rel, err)
		}
		if _, err := os.Lstat(dst); err == nil {
			conflicts = append(conflicts, dst)
			continue
		}
		if err := relocate(filepath.Join(root, rel), dst); err != nil {
			return restored, conflicts, err
		}
		restored = append(restored, dst)
	}
	removeEmpty(root)
	return restored, conflicts, nil
}

// removeEmpty removes root and the folders under it that Restore emptied.
// os.Remove fails on a folder that still holds anything, which is left.
func removeEmpty(root string) {
	var dirs []string
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			dirs = append(dirs, path)
		}
		return nil
	})
	for i := len(dirs) - 1; i >= 0; i-- {
		os.Remove(dirs[i])
	}
}
