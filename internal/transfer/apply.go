package transfer

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/lr00rl/ccli/internal/account"
)

// Mode selects copy or move.
type Mode int

const (
	Copy Mode = iota
	Move
	ForkMode // a copy under a new session id
)

// Action is what Plan decided to do with the transcript.
type Action string

const (
	Create      Action = "create"       // target has no transcript
	FastForward Action = "fast-forward" // target transcript is a prefix of the source
	UpToDate    Action = "up-to-date"   // identical transcript
	Forked      Action = "fork"         // copied under a new session id
	Refuse      Action = "refuse"
)

// Op is a planned copy or move of one session.
type Op struct {
	SessionID  string          `json:"session_id"`
	ProjectKey string          `json:"project_key"`
	From       string          `json:"from"` // account names
	To         string          `json:"to"`
	Mode       Mode            `json:"mode"`
	Action     Action          `json:"action"`
	Reason     string          `json:"reason,omitempty"` // why refused
	Bytes      int64           `json:"bytes"`            // transcript bytes to write
	from, to   account.Account // unexported
}

// Options controls planning and applying.
type Options struct {
	// Live reports whether a Claude Code process is running the session in
	// any account. Nil means never live.
	Live func(sessionID string) bool
	// ArchiveDir is where Move puts the source files before removing them.
	// Required for Move.
	ArchiveDir string
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Intend, when set, is told about each transcript range before it is
	// written (see Intend).
	Intend Intend
}

// Result is what Apply did.
type Result struct {
	Op        Op       `json:"op"`
	Written   []string `json:"written"`   // files created or replaced in the target
	Skipped   []string `json:"skipped"`   // sidecar files already identical
	Conflicts []string `json:"conflicts"` // target files left alone because they differ and are not a prefix of the source
	Archive   string   `json:"archive,omitempty"`
	Removed   []string `json:"removed,omitempty"`
	Imports   []Import `json:"imports,omitempty"` // transcript bytes written into the target

	intend Intend
}

// Intend is called with the transcript bytes a copy is about to write, before
// the file is put in place. An error abandons that file; undo (when not nil)
// is called if putting it in place then fails. It lets the caller record the
// copy before the bytes exist, so nothing can read them unrecorded.
type Intend func(Import) (undo func(), err error)

const (
	reasonSameDir   = "source and target are the same directory"
	reasonBadID     = "invalid session id"
	reasonMissing   = "transcript not found in source"
	reasonLive      = "a Claude Code process is running this session"
	reasonNewer     = "target has a newer copy; move it the other way"
	reasonDiverged  = "the copies diverged; continue one as a new session with ccli fork"
	reasonNoArchive = "archive dir is required for move"
	reasonShared    = "the two accounts share these folders (a symlink); nothing to copy"
	reasonOtherKey  = "the target holds this session under another project folder (%s)"

	symlinkMark = " (symlink)"
)

// Plan decides what copying or moving the session from `from` to `to` would
// do, without writing anything.
func Plan(sessionID string, from, to account.Account, mode Mode, opt Options) (Op, error) {
	op := Op{
		SessionID: sessionID,
		From:      from.Name,
		To:        to.Name,
		Mode:      mode,
		from:      from,
		to:        to,
	}
	if !validID(sessionID) {
		return refuse(op, reasonBadID), nil
	}
	if sameDir(from.Dir, to.Dir) {
		return refuse(op, reasonSameDir), nil
	}
	if mode == Move && opt.ArchiveDir == "" {
		return refuse(op, reasonNoArchive), nil
	}
	if opt.Live != nil && opt.Live(sessionID) {
		return refuse(op, reasonLive), nil
	}
	src, err := Locate(from, sessionID)
	if err != nil {
		if errors.Is(err, errNotFound) {
			return refuse(op, reasonMissing), nil
		}
		return Op{}, err
	}
	op.ProjectKey = src.ProjectKey
	if src.Transcript == "" {
		return refuse(op, reasonMissing), nil
	}
	// Two accounts can share folders through a symlink; then source and
	// target are the same files and a move would archive them out of both.
	for _, rel := range []string{"", "projects", "file-history", filepath.Join("projects", src.ProjectKey)} {
		if sameFile(filepath.Join(from.Dir, rel), filepath.Join(to.Dir, rel)) {
			return refuse(op, reasonShared), nil
		}
	}
	// The target may hold the session under another project folder.
	if have, err := Locate(to, sessionID); err == nil {
		if have.ProjectKey != src.ProjectKey {
			return refuse(op, fmt.Sprintf(reasonOtherKey, have.ProjectKey)), nil
		}
	} else if !errors.Is(err, errNotFound) {
		if errors.Is(err, errMultiple) {
			return refuse(op, err.Error()), nil
		}
		return Op{}, err
	}
	info, err := os.Lstat(src.Transcript)
	if err != nil {
		return Op{}, fmt.Errorf("copy %s: %w", src.Transcript, err)
	}
	if !info.Mode().IsRegular() {
		return Op{}, fmt.Errorf("copy %s: not a regular file", src.Transcript)
	}
	dst, err := sessionPaths(to.Dir, src.ProjectKey, sessionID)
	if err != nil {
		return Op{}, err
	}
	dstInfo, err := os.Lstat(dst.transcript)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			op.Action = Create
			op.Bytes = info.Size()
			return op, nil
		}
		return Op{}, fmt.Errorf("copy %s: %w", dst.transcript, err)
	}
	if !dstInfo.Mode().IsRegular() {
		return Op{}, fmt.Errorf("copy %s: not a regular file", dst.transcript)
	}
	st, err := byteState(src.Transcript, dst.transcript)
	if err != nil {
		return Op{}, err
	}
	switch st {
	case Same:
		op.Action = UpToDate
	case AAhead:
		op.Action = FastForward
		op.Bytes = info.Size()
	case BAhead:
		return refuse(op, reasonNewer), nil
	case Diverged:
		return refuse(op, reasonDiverged), nil
	default:
		return Op{}, fmt.Errorf("compare %s: unexpected state %q", src.Transcript, st)
	}
	return op, nil
}

func refuse(op Op, reason string) Op {
	op.Action = Refuse
	op.Reason = reason
	op.Bytes = 0
	return op
}

// Apply carries out a planned Op. It re-checks the plan first (the files may
// have changed since Plan) and refuses if the action changed.
func Apply(op Op, opt Options) (Result, error) {
	planned, err := Plan(op.SessionID, op.from, op.to, op.Mode, opt)
	if err != nil {
		return Result{}, err
	}
	if planned.Action != op.Action || planned.Action == Refuse {
		if planned.Reason != "" {
			return Result{}, fmt.Errorf("apply %s: %s", op.SessionID, planned.Reason)
		}
		return Result{}, fmt.Errorf("apply %s: plan changed from %s to %s", op.SessionID, op.Action, planned.Action)
	}
	var archiveAt string
	if planned.Mode == Move {
		archiveAt, err = archiveRoot(opt.ArchiveDir, planned.from.Name, opt.now())
		if err != nil {
			return Result{}, err
		}
	}
	src, err := Locate(planned.from, planned.SessionID)
	if err != nil {
		return Result{}, err
	}
	if src.ProjectKey != planned.ProjectKey || src.Transcript == "" {
		return Result{}, fmt.Errorf("apply %s: session changed", planned.SessionID)
	}
	dst, err := sessionPaths(planned.to.Dir, src.ProjectKey, src.SessionID)
	if err != nil {
		return Result{}, err
	}
	res := Result{
		Op:        planned,
		Written:   []string{},
		Skipped:   []string{},
		Conflicts: []string{},
		intend:    opt.Intend,
	}
	if planned.Action == Create || planned.Action == FastForward {
		if err := checkParents(planned.to.Dir, dst.transcript); err != nil {
			return Result{}, err
		}
		if err := copyTranscript(src.Transcript, dst.transcript, &res); err != nil {
			return Result{}, err
		}
	}
	for _, dir := range src.sidecars() {
		if err := mergeTree(dir, src.Account.Dir, planned.to.Dir, &res); err != nil {
			return res, err
		}
	}
	if src.FileHistory != "" {
		if err := mergeTree(src.FileHistory, src.Account.Dir, planned.to.Dir, &res); err != nil {
			return res, err
		}
	}
	sort.Strings(res.Written)
	sort.Strings(res.Skipped)
	sort.Strings(res.Conflicts)
	if planned.Mode != Move {
		return res, nil
	}
	if len(res.Conflicts) > 0 {
		return res, fmt.Errorf("move stopped: %s differs and was left unchanged", res.Conflicts[0])
	}
	for _, s := range res.Skipped {
		if strings.HasSuffix(s, symlinkMark) {
			return res, fmt.Errorf("move stopped: %s cannot be copied; use copy instead", strings.TrimSuffix(s, symlinkMark))
		}
	}
	// Everything the archive will take must now be in the target, unchanged.
	st, err := byteState(src.Transcript, dst.transcript)
	if err != nil {
		return res, err
	}
	if st != Same {
		return res, fmt.Errorf("copy %s: target transcript does not match the source", dst.transcript)
	}
	for _, root := range append(src.sidecars(), src.FileHistory) {
		if err := verifyTree(root, src.Account.Dir, planned.to.Dir); err != nil {
			return res, err
		}
	}
	removed, err := archiveSource(src, opt.ArchiveDir, archiveAt)
	if err != nil {
		return res, err
	}
	sort.Strings(removed)
	res.Removed = removed
	res.Archive = archiveAt
	return res, nil
}

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// copyRegular writes src to dst via a temporary file in the destination
// directory, then renames it into place. The target is never truncated in place.
// The copy keeps its own times: a copy must look newer than its original, since
// the index gives a session's usage to the file created first.
func copyRegular(src, dst string) error {
	if err := mkdirAll(filepath.Dir(dst)); err != nil {
		return fmt.Errorf("copy %s: %w", dst, err)
	}
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("copy %s: %w", src, err)
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return fmt.Errorf("copy %s: %w", src, err)
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("copy %s: not a regular file", src)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".ccli-tmp-*")
	if err != nil {
		return fmt.Errorf("copy %s: %w", dst, err)
	}
	tmpName := tmp.Name()
	done := false
	defer func() {
		if !done {
			os.Remove(tmpName)
		}
	}()
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return fmt.Errorf("copy %s: %w", src, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("copy %s: %w", dst, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("copy %s: %w", dst, err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("copy %s: %w", dst, err)
	}
	done = true
	return nil
}

// mergeTree copies regular files under srcRoot into the same relative path
// under toDir. Identical files are skipped. A target that is a byte prefix
// of the source is replaced. Anything else is left in place and recorded
// as a conflict. Nothing in the target is deleted.
func mergeTree(srcRoot, fromDir, toDir string, res *Result) error {
	return filepath.WalkDir(srcRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("copy %s: %w", path, err)
		}
		if path == srcRoot {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			res.Skipped = append(res.Skipped, path+symlinkMark)
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(fromDir, path)
		if err != nil || !within(fromDir, path) {
			return fmt.Errorf("copy %s: escapes %s", path, fromDir)
		}
		if isMemoryRel(rel) {
			return fmt.Errorf("copy %s: refusing to copy project memory", path)
		}
		dst, err := joinWithin(toDir, rel)
		if err != nil {
			return fmt.Errorf("copy %s: %w", path, err)
		}
		if err := checkParents(toDir, dst); err != nil {
			return err
		}
		return mergeFile(path, dst, res)
	})
}

func mergeFile(src, dst string, res *Result) error {
	_, err := os.Lstat(dst)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return copyTranscript(src, dst, res)
		}
		return fmt.Errorf("copy %s: %w", dst, err)
	}
	st, err := byteState(src, dst)
	if err != nil {
		return err
	}
	switch st {
	case Same:
		res.Skipped = append(res.Skipped, dst)
	case AAhead:
		return copyTranscript(src, dst, res)
	default:
		res.Conflicts = append(res.Conflicts, dst)
	}
	return nil
}

// verifyTree checks that every file under srcRoot is in toDir at the same
// relative path with the same bytes. Anything that is not a regular file or a
// directory fails, since it was not copied.
func verifyTree(srcRoot, fromDir, toDir string) error {
	if srcRoot == "" {
		return nil
	}
	return filepath.WalkDir(srcRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("verify %s: %w", path, err)
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("move stopped: %s is not a regular file and was not copied; use copy instead", path)
		}
		rel, err := filepath.Rel(fromDir, path)
		if err != nil || !within(fromDir, path) {
			return fmt.Errorf("verify %s: escapes %s", path, fromDir)
		}
		dst, err := joinWithin(toDir, rel)
		if err != nil {
			return fmt.Errorf("verify %s: %w", path, err)
		}
		if !isRegular(dst) {
			return fmt.Errorf("move stopped: %s is missing in the target", dst)
		}
		st, err := byteState(path, dst)
		if err != nil {
			return err
		}
		if st != Same {
			return fmt.Errorf("move stopped: %s changed after it was copied", path)
		}
		return nil
	})
}

// checkParents refuses a write whose directory, below the account's
// projects/ or file-history/ folder, passes through a symlink: the write
// would land outside the session's own folders.
func checkParents(root, path string) error {
	rel, err := filepath.Rel(root, filepath.Dir(path))
	if err != nil || !within(root, path) {
		return fmt.Errorf("write %s: escapes %s", path, root)
	}
	parts := strings.Split(rel, string(filepath.Separator))
	cur := filepath.Join(root, parts[0])
	for _, p := range parts[1:] {
		cur = filepath.Join(cur, p)
		fi, err := os.Lstat(cur)
		if errors.Is(err, os.ErrNotExist) {
			return nil // created later by mkdirAll as a real directory
		}
		if err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
		if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
			return fmt.Errorf("write %s: %s is not a plain directory", path, cur)
		}
	}
	return nil
}

func archiveRoot(dir, name string, now time.Time) (string, error) {
	if dir == "" {
		return "", errors.New(reasonNoArchive)
	}
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") {
		return "", fmt.Errorf("archive %s: invalid account name %q", dir, name)
	}
	root := filepath.Join(dir, now.Format("20060102-150405"), name)
	if !within(dir, root) {
		return "", fmt.Errorf("archive %s: escapes %s", root, dir)
	}
	return root, nil
}

func archiveSource(src Files, archiveDir, root string) ([]string, error) {
	removed, err := sessionTree(src)
	if err != nil {
		return nil, err
	}
	// The transcript goes last: if an earlier step fails, the session is still
	// in the source and the move can simply be run again.
	paths := append(append([]string{src.FileHistory}, src.sidecars()...), src.Transcript)
	for _, path := range paths {
		if path == "" {
			continue
		}
		rel, err := filepath.Rel(src.Account.Dir, path)
		if err != nil || !within(src.Account.Dir, path) || !sessionRel(rel, src.ProjectKey, src.SessionID) {
			return nil, fmt.Errorf("archive %s: escapes %s", path, src.Account.Dir)
		}
		dst, err := joinWithin(root, rel)
		if err != nil {
			return nil, fmt.Errorf("archive %s: %w", path, err)
		}
		if !within(archiveDir, dst) {
			return nil, fmt.Errorf("archive %s: escapes %s", dst, archiveDir)
		}
		if err := relocate(path, dst); err != nil {
			return nil, err
		}
	}
	return removed, nil
}

// sessionRel reports whether rel is the transcript, a session folder under
// projects/ (the transcript's key or another), or the file-history directory
// of this session. Memory and other sessions are not session paths.
func sessionRel(rel, key, id string) bool {
	rel = filepath.Clean(rel)
	if rel == filepath.Join("projects", key, id+".jsonl") || rel == filepath.Join("file-history", id) {
		return true
	}
	dir, base := filepath.Split(rel)
	proj, k := filepath.Split(filepath.Clean(dir))
	return base == id && filepath.Clean(proj) == "projects" && validKey(k)
}

func sessionTree(src Files) ([]string, error) {
	var out []string
	var walk func(path string) error
	walk = func(path string) error {
		if path == "" {
			return nil
		}
		st, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("archive %s: %w", path, err)
		}
		out = append(out, path)
		if !st.IsDir() {
			return nil
		}
		return filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return fmt.Errorf("archive %s: %w", p, err)
			}
			if p == path {
				return nil
			}
			if d.Type()&os.ModeSymlink != 0 {
				return nil
			}
			if d.IsDir() || d.Type().IsRegular() {
				out = append(out, p)
			}
			return nil
		})
	}
	for _, p := range append(append([]string{src.Transcript}, src.sidecars()...), src.FileHistory) {
		if err := walk(p); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// relocate moves src to dst. Cross-device renames fall back to a copy
// followed by removing the source.
func relocate(src, dst string) error {
	if err := mkdirAll(filepath.Dir(dst)); err != nil {
		return fmt.Errorf("archive %s: %w", dst, err)
	}
	if err := os.Rename(src, dst); err != nil {
		if !errors.Is(err, syscall.EXDEV) {
			return fmt.Errorf("archive %s: %w", src, err)
		}
		if err := copyEntry(src, dst); err != nil {
			return err
		}
		if err := os.RemoveAll(src); err != nil {
			return fmt.Errorf("remove %s: %w", src, err)
		}
	}
	return nil
}

func copyEntry(src, dst string) error {
	st, err := os.Lstat(src)
	if err != nil {
		return fmt.Errorf("archive %s: %w", src, err)
	}
	switch {
	case st.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return fmt.Errorf("archive %s: %w", src, err)
		}
		if err := os.Symlink(target, dst); err != nil {
			return fmt.Errorf("archive %s: %w", dst, err)
		}
		return nil
	case st.IsDir():
		if err := mkdirAll(dst); err != nil {
			return fmt.Errorf("archive %s: %w", dst, err)
		}
		ents, err := os.ReadDir(src)
		if err != nil {
			return fmt.Errorf("archive %s: %w", src, err)
		}
		for _, e := range ents {
			if err := copyEntry(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
				return err
			}
		}
		return nil
	case st.Mode().IsRegular():
		if err := copyRegular(src, dst); err != nil {
			return err
		}
		// A moved file keeps its mode and time, as a rename would.
		if err := os.Chmod(dst, st.Mode().Perm()); err != nil {
			return fmt.Errorf("archive %s: %w", dst, err)
		}
		return os.Chtimes(dst, st.ModTime(), st.ModTime())
	default:
		return fmt.Errorf("archive %s: not a regular file or directory", src)
	}
}

// mkdirAll creates path and any missing parents with mode 0700.
// Directories that already exist are left unchanged.
func mkdirAll(path string) error {
	path = filepath.Clean(path)
	st, err := os.Lstat(path)
	if err == nil {
		if !st.IsDir() {
			return fmt.Errorf("%s is not a directory", path)
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	parent := filepath.Dir(path)
	if parent == path {
		return fmt.Errorf("%s has no parent", path)
	}
	if err := mkdirAll(parent); err != nil {
		return err
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil
		}
		return err
	}
	return os.Chmod(path, 0o700)
}

// copyTranscript copies src over dst (see copyRegular) and, for a
// transcript (.jsonl), records the bytes the target did not have before.
func copyTranscript(src, dst string, res *Result) error {
	var before int64
	if st, err := os.Lstat(dst); err == nil && st.Mode().IsRegular() {
		before = st.Size()
	}
	jsonl := strings.HasSuffix(dst, ".jsonl")
	var undo func()
	if jsonl && res.intend != nil {
		if st, err := os.Stat(src); err == nil && st.Size() > before {
			if undo, err = res.intend(Import{Path: dst, From: before, To: st.Size()}); err != nil {
				return fmt.Errorf("record the copy of %s: %w", dst, err)
			}
		}
	}
	if err := copyRegular(src, dst); err != nil {
		if undo != nil {
			undo()
		}
		return err
	}
	res.Written = append(res.Written, dst)
	if jsonl {
		if st, err := os.Stat(dst); err == nil && st.Size() > before {
			res.Imports = append(res.Imports, Import{Path: dst, From: before, To: st.Size()})
		}
	}
	return nil
}
