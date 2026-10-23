package transfer

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/lr00rl/ccli/internal/account"
)

// ForkPlan is a planned fork: the session copied into an account under a
// new id, so both copies can go on separately.
type ForkPlan struct {
	SessionID    string `json:"session_id"`
	NewSessionID string `json:"new_session_id"`
	ProjectKey   string `json:"project_key"`
	From         string `json:"from"`
	To           string `json:"to"`
	Bytes        int64  `json:"bytes"`   // transcript bytes to write
	Folders      int    `json:"folders"` // session folders copied along
	// Turn is the last turn the fork keeps, of Turns; 0 when it keeps the
	// whole conversation. Left counts the agent transcripts left out, and
	// Unread the bytes written after the index last read the transcript,
	// which a fork through its last turn leaves out.
	Turn   int    `json:"turn,omitempty"`
	Turns  int    `json:"turns,omitempty"`
	Left   int    `json:"agents_left_out,omitempty"`
	Unread int64  `json:"unread_bytes,omitempty"`
	Reason string `json:"reason,omitempty"`
	from   account.Account
	to     account.Account
	cut    Cut
}

// Cut ends a fork at a turn: the transcript up to the prompt of the turn
// after it, without the transcripts of agents started later. The zero Cut
// keeps everything.
type Cut struct {
	Turn  int   // last turn kept
	Turns int   // turns in the session
	Upto  int64 // transcript bytes kept: where the next turn's prompt starts; 0 = all
	// End is set when Turn is the last turn the index has read: Upto is then
	// where the index stopped reading, not the start of a prompt.
	End   bool
	Leave []string // agent transcripts (absolute paths) to leave out
	// Prompt reports whether a transcript line starts a turn. The line at
	// Upto must, unless End is set.
	Prompt func(line []byte) bool
}

const reasonStaleCut = "the transcript does not match ccli's index at that turn; run ccli index and try again"

// checkCut says why a cut does not fit the session's files now, or "" when
// it does: Upto ends a line within the complete bytes and, unless the cut
// is at the end, starts a turn; every transcript to leave out is in the
// session's folders.
func checkCut(src Files, cut Cut, complete int64) (string, error) {
	if cut.Upto > 0 {
		if cut.Upto > complete || !cut.End && cut.Upto == complete {
			return reasonStaleCut, nil
		}
		f, err := os.Open(src.Transcript)
		if err != nil {
			return "", err
		}
		defer f.Close()
		var prev [1]byte
		if _, err := f.ReadAt(prev[:], cut.Upto-1); err != nil {
			return "", err
		}
		if prev[0] != '\n' {
			return reasonStaleCut, nil
		}
		if !cut.End {
			ln, err := bufio.NewReader(io.NewSectionReader(f, cut.Upto, complete-cut.Upto)).ReadBytes('\n')
			if err != nil {
				return "", err
			}
			if cut.Prompt == nil || !cut.Prompt(ln) {
				return reasonStaleCut, nil
			}
		}
	}
	if len(cut.Leave) == 0 {
		return "", nil
	}
	have := map[string]bool{}
	for _, dir := range src.sidecars() {
		err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err == nil && d.Type().IsRegular() {
				have[path] = true
			}
			return err
		})
		if err != nil {
			return "", err
		}
	}
	for _, path := range cut.Leave {
		if !have[filepath.Clean(path)] {
			return fmt.Sprintf("ccli's index lists %s as an agent of this session, but it is not in the session's folders; run ccli index and try again", path), nil
		}
	}
	return "", nil
}

// NewID returns a random (version 4) session id.
func NewID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// PlanFork decides what forking the session from one account into another
// (or the same one) would write. A running session can be forked: only its
// complete lines are copied, and the copy gets its own id. A non-zero cut
// ends the copy at a turn.
func PlanFork(sessionID string, from, to account.Account, newID string, cut Cut) (ForkPlan, error) {
	p := ForkPlan{SessionID: sessionID, NewSessionID: newID, From: from.Name, To: to.Name, from: from, to: to,
		Turn: cut.Turn, Turns: cut.Turns, cut: cut}
	if !validID(sessionID) || !validID(newID) {
		p.Reason = reasonBadID
		return p, nil
	}
	src, err := Locate(from, sessionID)
	if err != nil {
		if errors.Is(err, errNotFound) {
			p.Reason = reasonMissing
			return p, nil
		}
		return ForkPlan{}, err
	}
	p.ProjectKey = src.ProjectKey
	if src.Transcript == "" {
		p.Reason = reasonMissing
		return p, nil
	}
	if _, err := Locate(to, newID); err == nil || !errors.Is(err, errNotFound) {
		p.Reason = "the target already has a session with the new id"
		return p, nil
	}
	st, err := os.Lstat(src.Transcript)
	if err != nil {
		return ForkPlan{}, err
	}
	if p.Bytes, err = completeLines(src.Transcript, st.Size()); err != nil {
		return ForkPlan{}, err
	}
	if p.Reason, err = checkCut(src, cut, p.Bytes); err != nil || p.Reason != "" {
		return p, err
	}
	if cut.Upto > 0 {
		if cut.End {
			p.Unread = p.Bytes - cut.Upto
		}
		p.Bytes = cut.Upto
	}
	p.Left = len(cut.Leave)
	p.Folders = len(src.sidecars())
	if src.FileHistory != "" {
		p.Folders++
	}
	return p, nil
}

// Fork carries out a plan. Every transcript line keeps its bytes except the
// session id fields, which name the new session.
func Fork(p ForkPlan, intend Intend) (Result, error) {
	res := Result{Op: Op{SessionID: p.SessionID, ProjectKey: p.ProjectKey, From: p.From, To: p.To, Mode: ForkMode, Action: Forked},
		Written: []string{}, Skipped: []string{}, Conflicts: []string{}, intend: intend}
	if p.Reason != "" {
		return res, fmt.Errorf("fork %s: %s", p.SessionID, p.Reason)
	}
	src, err := Locate(p.from, p.SessionID)
	if err != nil {
		return res, err
	}
	dst, err := sessionPaths(p.to.Dir, src.ProjectKey, p.NewSessionID)
	if err != nil {
		return res, err
	}
	if err := checkParents(p.to.Dir, dst.transcript); err != nil {
		return res, err
	}
	// The plan checked the cut; check it again against the files as they
	// are now, before anything is written.
	if p.cut.Upto > 0 || len(p.cut.Leave) > 0 {
		st, err := os.Lstat(src.Transcript)
		if err != nil {
			return res, err
		}
		complete, err := completeLines(src.Transcript, st.Size())
		if err != nil {
			return res, err
		}
		if reason, err := checkCut(src, p.cut, complete); err != nil || reason != "" {
			if err == nil {
				err = errors.New(reason)
			}
			return res, fmt.Errorf("fork stopped: %w", err)
		}
	}
	rw := idRewriter(p.SessionID, p.NewSessionID)
	if err := forkFile(src.Transcript, dst.transcript, p.cut.Upto, rw, &res); err != nil {
		return res, err
	}
	// An agent started after the cut belongs to the part left out; its
	// .meta.json goes with it.
	leave := map[string]bool{}
	for _, path := range p.cut.Leave {
		path = filepath.Clean(path)
		leave[path] = true
		leave[strings.TrimSuffix(path, ".jsonl")+".meta.json"] = true
	}
	// Session folders: projects/<key>/<id>/ under each key, and file-history.
	type tree struct{ from, to string }
	var trees []tree
	for _, dir := range src.sidecars() {
		rel, err := filepath.Rel(src.Account.Dir, dir)
		if err != nil || !within(src.Account.Dir, dir) {
			return res, fmt.Errorf("fork %s: escapes %s", dir, src.Account.Dir)
		}
		to, err := joinWithin(p.to.Dir, filepath.Join(filepath.Dir(rel), p.NewSessionID))
		if err != nil {
			return res, err
		}
		trees = append(trees, tree{dir, to})
	}
	if src.FileHistory != "" {
		trees = append(trees, tree{src.FileHistory, dst.history})
	}
	for _, tr := range trees {
		err := filepath.WalkDir(tr.from, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return fmt.Errorf("fork %s: %w", path, err)
			}
			if d.Type()&os.ModeSymlink != 0 {
				res.Skipped = append(res.Skipped, path+symlinkMark)
				return nil
			}
			if d.IsDir() || !d.Type().IsRegular() || leave[path] {
				return nil
			}
			rel, err := filepath.Rel(tr.from, path)
			if err != nil {
				return err
			}
			out, err := joinWithin(tr.to, rel)
			if err != nil {
				return err
			}
			if err := checkParents(p.to.Dir, out); err != nil {
				return err
			}
			if _, err := os.Lstat(out); err == nil {
				res.Conflicts = append(res.Conflicts, out)
				return nil
			}
			if strings.HasSuffix(path, ".jsonl") {
				return forkFile(path, out, 0, rw, &res)
			}
			if err := copyRegular(path, out); err != nil {
				return err
			}
			res.Written = append(res.Written, out)
			return nil
		})
		if err != nil {
			return res, err
		}
	}
	sort.Strings(res.Written)
	return res, nil
}

// idRewriter replaces the session id fields of a transcript line.
func idRewriter(oldID, newID string) func([]byte) []byte {
	var pairs [][2][]byte
	for _, f := range []string{"sessionId", "session_id"} {
		pairs = append(pairs, [2][]byte{[]byte(`"` + f + `":"` + oldID + `"`), []byte(`"` + f + `":"` + newID + `"`)})
	}
	return func(line []byte) []byte {
		for _, p := range pairs {
			line = bytes.ReplaceAll(line, p[0], p[1])
		}
		return line
	}
}

// forkFile writes the complete lines of src to dst, rewritten, through a
// temporary file, and records them as imported. A positive upto stops at
// that offset, which is the start of a line.
func forkFile(src, dst string, upto int64, rw func([]byte) []byte, res *Result) error {
	if err := mkdirAll(filepath.Dir(dst)); err != nil {
		return fmt.Errorf("fork %s: %w", dst, err)
	}
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("fork %s: %w", src, err)
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".ccli-tmp-*")
	if err != nil {
		return fmt.Errorf("fork %s: %w", dst, err)
	}
	name := tmp.Name()
	done := false
	defer func() {
		if !done {
			os.Remove(name)
		}
	}()
	br := bufio.NewReaderSize(in, 1<<20)
	bw := bufio.NewWriterSize(tmp, 1<<20)
	var n, read int64
	for {
		ln, err := br.ReadBytes('\n')
		if err == nil {
			if read += int64(len(ln)); upto > 0 && read > upto {
				break
			}
			w, werr := bw.Write(rw(ln))
			if werr != nil {
				tmp.Close()
				return fmt.Errorf("fork %s: %w", dst, werr)
			}
			n += int64(w)
			continue
		}
		if errors.Is(err, io.EOF) {
			break // a partial last line is still being written: left out
		}
		tmp.Close()
		return fmt.Errorf("fork %s: %w", src, err)
	}
	if err := bw.Flush(); err != nil {
		tmp.Close()
		return fmt.Errorf("fork %s: %w", dst, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("fork %s: %w", dst, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("fork %s: %w", dst, err)
	}
	im := Import{Path: dst, From: 0, To: n}
	jsonl := strings.HasSuffix(dst, ".jsonl") && n > 0
	var undo func()
	if jsonl && res.intend != nil {
		if undo, err = res.intend(im); err != nil {
			return fmt.Errorf("record the fork of %s: %w", dst, err)
		}
	}
	// A hard link puts the file in place only if nothing has appeared at
	// dst since the plan; a rename would replace it.
	if err := os.Link(name, dst); err != nil {
		if undo != nil {
			undo()
		}
		return fmt.Errorf("fork %s: %w", dst, err)
	}
	os.Remove(name)
	done = true
	if err := syncDir(filepath.Dir(dst)); err != nil {
		return fmt.Errorf("fork %s: %w", dst, err)
	}
	res.Written = append(res.Written, dst)
	if jsonl {
		res.Imports = append(res.Imports, im)
	}
	return nil
}

// completeLines returns how many of a file's first size bytes end in a
// newline: what Fork copies, since it leaves out a partial last line.
func completeLines(path string, size int64) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	buf := make([]byte, 64<<10)
	for end := size; end > 0; {
		start := max(0, end-int64(len(buf)))
		b := buf[:end-start]
		if _, err := f.ReadAt(b, start); err != nil {
			return 0, err
		}
		if i := bytes.LastIndexByte(b, '\n'); i >= 0 {
			return start + int64(i) + 1, nil
		}
		end = start
	}
	return 0, nil
}
