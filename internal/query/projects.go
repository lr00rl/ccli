package query

import (
	"context"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ProjectRow is one folder sessions started in, with what they used.
type ProjectRow struct {
	Path     string    `json:"path"` // the cwd the sessions recorded
	Rel      string    `json:"rel"`  // Path relative to the folder it was found under; "." for that folder
	Sessions int       `json:"sessions"`
	Live     int       `json:"live"` // sessions a process is running now
	Accounts []string  `json:"accounts"`
	First    time.Time `json:"first"`
	Last     time.Time `json:"last"`
	Prompts  int       `json:"prompts"`
	Tokens   Tokens    `json:"tokens"`
	Cost     Cost      `json:"cost"`
}

// ProjectPage lists the projects a filter selects.
type ProjectPage struct {
	Filter   Filter       `json:"filter"`
	Projects []ProjectRow `json:"projects"`
	Matched  int          `json:"matched"` // projects before the limit
	Sessions int          `json:"sessions"`
	Tokens   Tokens       `json:"tokens"`
	Cost     Cost         `json:"cost"`
}

// Projects groups the sessions a filter selects by the folder each started
// in, or with f.Depth by that folder's first Depth levels below the dir it
// matched, so worktrees and subfolders count toward their project. f.Dirs
// are taken as forms of one folder, as scopeDirs in the CLI makes them.
// Hidden sessions count, as they do in every total. Sort: last (default),
// cost, sessions, path.
func (d *DB) Projects(ctx context.Context, f Filter) (ProjectPage, error) {
	page := ProjectPage{Filter: f}
	rows, err := d.loadSessions(ctx, f)
	if err != nil {
		return page, err
	}
	byPath := map[string]*ProjectRow{}
	accts := map[string]map[string]bool{}
	for _, s := range rows {
		if s.Cwd == "" {
			continue
		}
		path, rel := s.Cwd, s.Cwd
		if dir := UnderAny(s.Cwd, f.Dirs); dir != "" {
			rel, _ = filepath.Rel(dir, s.Cwd)
			if f.Depth > 0 && rel != "." {
				if parts := strings.Split(rel, string(filepath.Separator)); len(parts) > f.Depth {
					rel = filepath.Join(parts[:f.Depth]...)
				}
			}
			// The dirs are forms of one folder: name the row in the first
			// form, so a cwd recorded in each form lands in one row.
			path = filepath.Join(f.Dirs[0], rel)
		}
		p := byPath[path]
		if p == nil {
			p = &ProjectRow{Path: path, Rel: rel, First: s.Started}
			byPath[path] = p
			accts[path] = map[string]bool{}
		}
		p.Sessions++
		if s.Live {
			p.Live++
		}
		if s.Started.Before(p.First) {
			p.First = s.Started
		}
		if s.Last.After(p.Last) {
			p.Last = s.Last
		}
		p.Prompts += s.Prompts
		p.Tokens.add(s.Tokens)
		p.Cost.add(s.Cost)
		accts[path][s.Account] = true
		page.Sessions++
		page.Tokens.add(s.Tokens)
		page.Cost.add(s.Cost)
	}
	for path, p := range byPath {
		for a := range accts[path] {
			p.Accounts = append(p.Accounts, a)
		}
		sort.Strings(p.Accounts)
		page.Projects = append(page.Projects, *p)
	}
	sortProjects(page.Projects, f.Sort)
	page.Matched = len(page.Projects)
	if f.Limit > 0 && len(page.Projects) > f.Limit {
		page.Projects = page.Projects[:f.Limit]
	}
	return page, nil
}

func sortProjects(ps []ProjectRow, by string) {
	less := func(i, j int) bool { return ps[i].Last.After(ps[j].Last) }
	switch by {
	case "cost":
		less = func(i, j int) bool { return ps[i].Cost.USD > ps[j].Cost.USD }
	case "sessions":
		less = func(i, j int) bool { return ps[i].Sessions > ps[j].Sessions }
	case "path":
		less = func(i, j int) bool { return ps[i].Path < ps[j].Path }
	}
	sort.SliceStable(ps, func(i, j int) bool {
		if less(i, j) != less(j, i) {
			return less(i, j)
		}
		return ps[i].Path < ps[j].Path
	})
}

// Under reports whether path is dir or inside it, comparing whole path
// segments: /a/b/c is under /a/b, but /a/bc is not.
func Under(path, dir string) bool {
	if path == "" || dir == "" {
		return false
	}
	if dir == "/" {
		return strings.HasPrefix(path, "/")
	}
	dir = strings.TrimSuffix(dir, "/")
	return path == dir || strings.HasPrefix(path, dir+"/")
}

// UnderAny returns the first of dirs that path is under, or "".
func UnderAny(path string, dirs []string) string {
	for _, d := range dirs {
		if Under(path, d) {
			return d
		}
	}
	return ""
}
