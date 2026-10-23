package transfer

import (
	"errors"
	"fmt"
	"io"
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

// Restore moves what Archive put under root back into the account. It moves
// only session paths (projects/<key>/<id>.jsonl, projects/<key>/<id>/ and
// file-history/<id>/). Anything else under root, such as a file another
// program left there, stays and is reported in left. A path the account has
// again is not replaced: it stays in the archive and is reported as a
// conflict.
func Restore(root string, a account.Account) (restored, conflicts, left []string, err error) {
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
			left = append(left, path)
		}
		if d.IsDir() {
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		return nil, nil, left, err
	}
	// Folders first, so a restored transcript has its subagents beside it.
	sort.Strings(dirs)
	sort.Strings(transcripts)
	for _, rel := range append(dirs, transcripts...) {
		dst, err := joinWithin(a.Dir, rel)
		if err != nil {
			return restored, conflicts, left, fmt.Errorf("restore %s: %w", rel, err)
		}
		if err := checkParents(a.Dir, dst); err != nil {
			return restored, conflicts, left, err
		}
		ok, err := moveBack(filepath.Join(root, rel), dst)
		if err != nil {
			return restored, conflicts, left, err
		}
		if !ok {
			conflicts = append(conflicts, dst)
			continue
		}
		restored = append(restored, dst)
	}
	removeEmpty(root)
	return restored, conflicts, left, nil
}

// moveBack moves src to dst unless dst exists, and reports whether it did.
// A file is hard-linked into place first: unlike a rename, a link fails if
// the tool wrote dst in the meantime.
func moveBack(src, dst string) (bool, error) {
	if _, err := os.Lstat(dst); err == nil {
		return false, nil
	}
	if st, err := os.Lstat(src); err == nil && st.Mode().IsRegular() {
		if err := mkdirAll(filepath.Dir(dst)); err != nil {
			return false, fmt.Errorf("restore %s: %w", dst, err)
		}
		switch err := os.Link(src, dst); {
		case err == nil:
			if err := syncDir(filepath.Dir(dst)); err != nil {
				return false, fmt.Errorf("restore %s: %w", dst, err)
			}
			return true, os.Remove(src)
		case errors.Is(err, os.ErrExist):
			return false, nil
		}
		// Another device, or no hard links. A rename would replace a dst
		// written since the check above, so copy into a file that is
		// created only if dst still does not exist.
		switch err := copyExclusive(src, dst, st); {
		case errors.Is(err, os.ErrExist):
			return false, nil
		case err != nil:
			return false, err
		}
		return true, os.Remove(src)
	}
	return true, relocate(src, dst)
}

// copyExclusive copies the regular file src (st) to dst, which it creates
// and which must not exist yet, keeping src's mode and time. The copy is
// synced before it returns.
func copyExclusive(src, dst string, st os.FileInfo) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("restore %s: %w", src, err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, st.Mode().Perm())
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return err
		}
		return fmt.Errorf("restore %s: %w", dst, err)
	}
	_, err = io.Copy(out, in)
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chtimes(dst, st.ModTime(), st.ModTime())
	}
	if err == nil {
		err = syncDir(filepath.Dir(dst))
	}
	if err != nil {
		os.Remove(dst)
		return fmt.Errorf("restore %s: %w", dst, err)
	}
	return nil
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
