package index

import (
	"errors"
	"io/fs"
	"path/filepath"

	"github.com/lr00rl/ccli/internal/account"
	"github.com/lr00rl/ccli/internal/pi"
)

// scanPi lists session JSONL files under <agent>/sessions. The session id
// and cwd come from the header line. A file that is still empty is left
// for the next run.
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
		if d.IsDir() || !d.Type().IsRegular() || filepath.Ext(d.Name()) != ".jsonl" {
			return nil
		}
		id, cwd, ok := pi.Header(path)
		if !ok {
			return nil
		}
		st, err := d.Info()
		if err != nil {
			return nil
		}
		found = append(found, discovered{
			tool: account.Pi,
			cwd:  cwd,
			file: storeFile(a, path, id, "main", "", st),
		})
		return nil
	})
	return found, err
}
