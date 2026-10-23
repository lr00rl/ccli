// Package cursoredits reads which files Cursor's agents wrote, and with which
// models, from the AI code tracking database Cursor keeps in
// ~/.cursor/ai-tracking. Cursor CLI chats and Cursor IDE composers both
// record there, under the conversation id. A read creates no file beside the
// database (see sqlitero).
package cursoredits

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/lr00rl/ccli/internal/sqlitero"
)

// Edits is what the agents of one conversation wrote.
type Edits struct {
	Files  []File   `json:"files"`  // newest write first
	Models []string `json:"models"` // the models that wrote them, most written first
}

// File is one file an agent wrote to.
type File struct {
	Path   string    `json:"path"`
	Models []string  `json:"models,omitempty"` // most written first
	Last   time.Time `json:"last"`             // newest write
}

// DBPath is the tracking database inside a Cursor CLI data directory.
func DBPath(cursorDir string) string {
	return filepath.Join(cursorDir, "ai-tracking", "ai-code-tracking.db")
}

// Read lists the files the given conversations wrote. Cursor keeps one row
// per line an agent wrote, so the row count orders the models. A missing
// database reads as nothing written.
func Read(ctx context.Context, dbPath string, ids []string) (Edits, error) {
	var out Edits
	if len(ids) == 0 {
		return out, nil
	}
	if _, err := os.Stat(dbPath); err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return out, err
	}
	db, err := sqlitero.Open(dbPath)
	if err != nil {
		return out, err
	}
	defer db.Close()
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := db.QueryContext(ctx, `SELECT fileName, COALESCE(model, ''), count(*), max(COALESCE(timestamp, createdAt))
		FROM ai_code_hashes WHERE conversationId IN (?`+strings.Repeat(",?", len(ids)-1)+`) AND COALESCE(fileName, '') <> ''
		GROUP BY fileName, model`, args...)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	type tally map[string]int
	byFile := map[string]*File{}
	lastMS := map[string]int64{}
	fileModels := map[string]tally{}
	all := tally{}
	for rows.Next() {
		var path, model string
		var n int
		var last int64
		if err := rows.Scan(&path, &model, &n, &last); err != nil {
			return out, err
		}
		f := byFile[path]
		if f == nil {
			f = &File{Path: path}
			byFile[path] = f
			fileModels[path] = tally{}
		}
		lastMS[path] = max(lastMS[path], last)
		if model != "" {
			fileModels[path][model] += n
			all[model] += n
		}
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	ranked := func(t tally) []string {
		var ms []string
		for m := range t {
			ms = append(ms, m)
		}
		sort.Slice(ms, func(i, j int) bool {
			if t[ms[i]] != t[ms[j]] {
				return t[ms[i]] > t[ms[j]]
			}
			return ms[i] < ms[j]
		})
		return ms
	}
	for path, f := range byFile {
		f.Models = ranked(fileModels[path])
		f.Last = time.UnixMilli(lastMS[path])
		out.Files = append(out.Files, *f)
	}
	sort.Slice(out.Files, func(i, j int) bool {
		if !out.Files[i].Last.Equal(out.Files[j].Last) {
			return out.Files[i].Last.After(out.Files[j].Last)
		}
		return out.Files[i].Path < out.Files[j].Path
	})
	out.Models = ranked(all)
	return out, nil
}
