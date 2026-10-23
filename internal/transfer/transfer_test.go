package transfer

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lr00rl/ccli/internal/account"
)

const (
	id1 = "11111111-1111-1111-1111-111111111111"
	id2 = "22222222-2222-2222-2222-222222222222"
	id3 = "33333333-3333-3333-3333-333333333333"
	id4 = "44444444-4444-4444-4444-444444444444"
	id5 = "55555555-5555-5555-5555-555555555555"
	id6 = "66666666-6666-6666-6666-666666666666"
	id7 = "77777777-7777-7777-7777-777777777777"
	id8 = "88888888-8888-8888-8888-888888888888"
	id9 = "99999999-9999-9999-9999-999999999999"
)

func makeAcct(t *testing.T, root, name string) account.Account {
	t.Helper()
	dir := filepath.Join(root, "accounts", name)
	if err := os.MkdirAll(filepath.Join(dir, "projects"), 0o700); err != nil {
		t.Fatal(err)
	}
	return account.Account{Name: name, Dir: dir}
}

func writeFile(t *testing.T, path, content string, mtime time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if !mtime.IsZero() {
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
}

func writeBytes(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readStr(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func mustContain(t *testing.T, list []string, want string) {
	t.Helper()
	for _, s := range list {
		if s == want {
			return
		}
	}
	t.Fatalf("missing %s in %#v", want, list)
}

func mustNotContain(t *testing.T, list []string, want string) {
	t.Helper()
	for _, s := range list {
		if s == want {
			t.Fatalf("unexpected %s in %#v", want, list)
		}
	}
}

func transcriptPath(a account.Account, key, id string) string {
	return filepath.Join(a.Dir, "projects", key, id+".jsonl")
}

func TestLocate(t *testing.T) {
	root := t.TempDir()
	a := makeAcct(t, root, "work")
	writeFile(t, transcriptPath(a, "proj", id1), "hello\n", time.Time{})
	writeFile(t, filepath.Join(a.Dir, "projects", "proj", id1, "subagents", "agent-a.jsonl"), "sub\n", time.Time{})
	writeFile(t, filepath.Join(a.Dir, "file-history", id1, "snap.txt"), "snap\n", time.Time{})

	got, err := Locate(a, id1)
	if err != nil {
		t.Fatal(err)
	}
	if got.ProjectKey != "proj" || got.SessionID != id1 {
		t.Fatalf("key %s id %s", got.ProjectKey, got.SessionID)
	}
	if got.Transcript != transcriptPath(a, "proj", id1) {
		t.Fatalf("transcript %s", got.Transcript)
	}
	if got.SidecarDir != filepath.Join(a.Dir, "projects", "proj", id1) {
		t.Fatalf("sidecar %s", got.SidecarDir)
	}
	if got.FileHistory != filepath.Join(a.Dir, "file-history", id1) {
		t.Fatalf("history %s", got.FileHistory)
	}

	// Sidecar only, no transcript and no file-history.
	writeFile(t, filepath.Join(a.Dir, "projects", "other", id2, "tool-results", "out.txt"), "x", time.Time{})
	got, err = Locate(a, id2)
	if err != nil {
		t.Fatal(err)
	}
	if got.Transcript != "" || got.FileHistory != "" {
		t.Fatalf("expected absent transcript and history, got %#v", got)
	}
	if got.ProjectKey != "other" || got.SidecarDir == "" {
		t.Fatalf("sidecar locate: %#v", got)
	}

	// file-history alone is not a session.
	writeFile(t, filepath.Join(a.Dir, "file-history", id3, "snap.txt"), "only-history", time.Time{})
	if _, err := Locate(a, id3); !errors.Is(err, errNotFound) {
		t.Fatalf("history-only: %v", err)
	}

	if _, err := Locate(a, id9); !errors.Is(err, errNotFound) {
		t.Fatalf("missing: %v", err)
	}
	upper := strings.ToUpper("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	for _, id := range []string{"nope", upper, "../" + id1, id1 + "/.."} {
		if _, err := Locate(a, id); !errors.Is(err, errInvalidID) {
			t.Fatalf("id %q: %v", id, err)
		}
	}

	writeFile(t, transcriptPath(a, "alpha", id4), "a", time.Time{})
	writeFile(t, transcriptPath(a, "beta", id4), "b", time.Time{})
	if _, err := Locate(a, id4); !errors.Is(err, errMultiple) {
		t.Fatalf("two keys: %v", err)
	}

	// A hidden project key is ignored.
	writeFile(t, filepath.Join(a.Dir, "projects", ".secret", id5+".jsonl"), "hidden", time.Time{})
	if _, err := Locate(a, id5); !errors.Is(err, errNotFound) {
		t.Fatalf("hidden key: %v", err)
	}
}

func TestCompare(t *testing.T) {
	root := t.TempDir()
	a := makeAcct(t, root, "a")
	b := makeAcct(t, root, "b")

	// Decoys: invalid id, hidden project, memory. None of these are sessions.
	writeFile(t, filepath.Join(a.Dir, "projects", "m", "not-a-session.jsonl"), "nope", time.Time{})
	writeFile(t, filepath.Join(a.Dir, "projects", ".hidden", id1+".jsonl"), "hidden", time.Time{})
	writeFile(t, filepath.Join(a.Dir, "projects", "m", "memory", "note.md"), "remember", time.Time{})

	writeFile(t, transcriptPath(a, "m", id4), "hello\n", time.Time{})
	writeFile(t, transcriptPath(b, "m", id4), "hello\nworld\n", time.Time{})

	writeFile(t, transcriptPath(a, "m", id5), "aaa\n", time.Time{})
	writeFile(t, transcriptPath(b, "m", id5), "bbb\n", time.Time{})

	writeFile(t, transcriptPath(a, "p", id1), "hello\n", time.Time{})
	writeFile(t, transcriptPath(b, "p", id1), "hello\n", time.Time{})

	writeFile(t, transcriptPath(a, "p", id2), "only-a\n", time.Time{})

	writeFile(t, transcriptPath(a, "q", id3), "hello\nworld\n", time.Time{})
	writeFile(t, transcriptPath(b, "q", id3), "hello\n", time.Time{})

	writeFile(t, transcriptPath(b, "z", id6), "only-b\n", time.Time{})

	// Prefix longer than the compare buffer, so the match crosses a chunk.
	prefix := bytes.Repeat([]byte("ab"), 80*1024)
	longer := append(append([]byte{}, prefix...), []byte("MORE")...)
	writeBytes(t, transcriptPath(a, "c", id7), longer)
	writeBytes(t, transcriptPath(b, "c", id7), prefix)

	got, err := Compare(a, b)
	if err != nil {
		t.Fatal(err)
	}
	want := []Entry{
		{SessionID: id7, ProjectKey: "c", State: AAhead, ASize: int64(len(longer)), BSize: int64(len(prefix))},
		{SessionID: id4, ProjectKey: "m", State: BAhead, ASize: int64(len("hello\n")), BSize: int64(len("hello\nworld\n"))},
		{SessionID: id5, ProjectKey: "m", State: Diverged, ASize: int64(len("aaa\n")), BSize: int64(len("bbb\n"))},
		{SessionID: id1, ProjectKey: "p", State: Same, ASize: int64(len("hello\n")), BSize: int64(len("hello\n"))},
		{SessionID: id2, ProjectKey: "p", State: OnlyA, ASize: int64(len("only-a\n"))},
		{SessionID: id3, ProjectKey: "q", State: AAhead, ASize: int64(len("hello\nworld\n")), BSize: int64(len("hello\n"))},
		{SessionID: id6, ProjectKey: "z", State: OnlyB, BSize: int64(len("only-b\n"))},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("entry %d:\n got %#v\nwant %#v", i, got[i], want[i])
		}
	}

	// Same id under two keys in one account is an error.
	writeFile(t, transcriptPath(a, "other", id8), "x", time.Time{})
	writeFile(t, transcriptPath(a, "extra", id8), "y", time.Time{})
	if _, err := Compare(a, b); !errors.Is(err, errMultiple) {
		t.Fatalf("duplicate id: %v", err)
	}
}

func TestCompareLarge(t *testing.T) {
	root := t.TempDir()
	a := makeAcct(t, root, "a")
	b := makeAcct(t, root, "b")
	const size = 3 << 20
	body := bytes.Repeat([]byte{'x'}, size)
	other := bytes.Repeat([]byte{'x'}, size)
	other[size-1] = 'y'
	writeBytes(t, transcriptPath(a, "big", id1), body)
	writeBytes(t, transcriptPath(b, "big", id1), other)

	got, err := Compare(a, b)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %#v", got)
	}
	// Equal length, so the state depends on reading the body. The last
	// byte is the only difference, past many read buffers.
	want := Entry{SessionID: id1, ProjectKey: "big", State: Diverged, ASize: size, BSize: size}
	if got[0] != want {
		t.Fatalf("got %#v", got[0])
	}
}

func TestPlan(t *testing.T) {
	root := t.TempDir()
	from := makeAcct(t, root, "from")
	to := makeAcct(t, root, "to")
	writeFile(t, transcriptPath(from, "proj", id1), "hello\nworld\n", time.Time{})

	t.Run("create", func(t *testing.T) {
		op, err := Plan(id1, from, to, Copy, Options{Live: func(string) bool { return false }})
		if err != nil {
			t.Fatal(err)
		}
		if op.Action != Create || op.Bytes != int64(len("hello\nworld\n")) || op.ProjectKey != "proj" {
			t.Fatalf("%#v", op)
		}
		if op.From != "from" || op.To != "to" || op.Mode != Copy || op.Reason != "" {
			t.Fatalf("%#v", op)
		}
		if _, err := os.Lstat(transcriptPath(to, "proj", id1)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("plan wrote a transcript")
		}
	})

	t.Run("fast-forward", func(t *testing.T) {
		writeFile(t, transcriptPath(to, "proj", id1), "hello\n", time.Time{})
		op, err := Plan(id1, from, to, Copy, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if op.Action != FastForward || op.Bytes != int64(len("hello\nworld\n")) {
			t.Fatalf("%#v", op)
		}
	})

	t.Run("up-to-date", func(t *testing.T) {
		writeFile(t, transcriptPath(to, "proj", id1), "hello\nworld\n", time.Time{})
		op, err := Plan(id1, from, to, Copy, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if op.Action != UpToDate || op.Bytes != 0 {
			t.Fatalf("%#v", op)
		}
	})

	t.Run("newer", func(t *testing.T) {
		writeFile(t, transcriptPath(to, "proj", id1), "hello\nworld\nmore\n", time.Time{})
		op, err := Plan(id1, from, to, Copy, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if op.Action != Refuse || op.Reason != reasonNewer || op.Bytes != 0 {
			t.Fatalf("%#v", op)
		}
	})

	t.Run("diverged", func(t *testing.T) {
		writeFile(t, transcriptPath(to, "proj", id1), "other\n", time.Time{})
		op, err := Plan(id1, from, to, Copy, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if op.Action != Refuse || op.Reason != reasonDiverged {
			t.Fatalf("%#v", op)
		}
	})

	t.Run("live", func(t *testing.T) {
		op, err := Plan(id1, from, to, Copy, Options{Live: func(id string) bool { return id == id1 }})
		if err != nil {
			t.Fatal(err)
		}
		if op.Action != Refuse || op.Reason != reasonLive {
			t.Fatalf("%#v", op)
		}
	})

	t.Run("missing", func(t *testing.T) {
		op, err := Plan(id9, from, to, Copy, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if op.Action != Refuse || op.Reason != reasonMissing {
			t.Fatalf("%#v", op)
		}
	})

	t.Run("sidecar without transcript", func(t *testing.T) {
		writeFile(t, filepath.Join(from.Dir, "projects", "proj", id8, "scratchpad", "n.txt"), "x", time.Time{})
		op, err := Plan(id8, from, to, Copy, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if op.Action != Refuse || op.Reason != reasonMissing || op.ProjectKey != "proj" {
			t.Fatalf("%#v", op)
		}
	})

	t.Run("invalid id", func(t *testing.T) {
		op, err := Plan("../"+id1, from, to, Copy, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if op.Action != Refuse || op.Reason != reasonBadID {
			t.Fatalf("%#v", op)
		}
	})

	t.Run("same dir", func(t *testing.T) {
		op, err := Plan(id1, from, from, Copy, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if op.Action != Refuse || op.Reason != reasonSameDir {
			t.Fatalf("%#v", op)
		}
	})

	t.Run("move without archive", func(t *testing.T) {
		op, err := Plan(id1, from, to, Move, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if op.Action != Refuse || op.Reason != reasonNoArchive || op.Mode != Move {
			t.Fatalf("%#v", op)
		}
	})
}

func TestApplyCopy(t *testing.T) {
	root := t.TempDir()
	from := makeAcct(t, root, "from")
	to := makeAcct(t, root, "to")
	key := "proj"
	srcTime := time.Unix(1_700_000_000, 0)
	oldTime := time.Unix(1_600_000_000, 0)

	writeFile(t, transcriptPath(from, key, id1), "line-1\nline-2\n", srcTime)
	writeFile(t, filepath.Join(from.Dir, "projects", key, id1, "subagents", "workflows", "run1", "journal.jsonl"), "journal\n", srcTime)
	writeFile(t, filepath.Join(from.Dir, "projects", key, id1, "same.txt"), "same-bytes", srcTime)
	writeFile(t, filepath.Join(from.Dir, "projects", key, id1, "prefix.txt"), "hello world", srcTime)
	writeFile(t, filepath.Join(from.Dir, "projects", key, id1, "conflict.txt"), "source-version", srcTime)
	writeFile(t, filepath.Join(from.Dir, "file-history", id1, "snap.txt"), "snap\n", srcTime)
	writeFile(t, filepath.Join(from.Dir, "projects", key, "memory", "deep", "note.md"), "remember", srcTime)
	writeFile(t, filepath.Join(from.Dir, "projects", key, "readme.txt"), "leave me", srcTime)

	outside := filepath.Join(root, "outside")
	writeFile(t, filepath.Join(outside, "secret.txt"), "super-secret", time.Time{})
	writeFile(t, filepath.Join(outside, "dir", "hidden.txt"), "hidden-secret", time.Time{})
	link := filepath.Join(from.Dir, "projects", key, id1, "link")
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), link); err != nil {
		t.Fatal(err)
	}
	dirlink := filepath.Join(from.Dir, "projects", key, id1, "dirlink")
	if err := os.Symlink(filepath.Join(outside, "dir"), dirlink); err != nil {
		t.Fatal(err)
	}

	sidecar := filepath.Join(to.Dir, "projects", key, id1)
	writeFile(t, filepath.Join(sidecar, "same.txt"), "same-bytes", oldTime)
	writeFile(t, filepath.Join(sidecar, "prefix.txt"), "hello", oldTime)
	writeFile(t, filepath.Join(sidecar, "conflict.txt"), "target-version", oldTime)

	before := map[string]string{
		transcriptPath(from, key, id1):                                                                   readStr(t, transcriptPath(from, key, id1)),
		filepath.Join(from.Dir, "projects", key, id1, "same.txt"):                                        readStr(t, filepath.Join(from.Dir, "projects", key, id1, "same.txt")),
		filepath.Join(from.Dir, "projects", key, id1, "subagents", "workflows", "run1", "journal.jsonl"): readStr(t, filepath.Join(from.Dir, "projects", key, id1, "subagents", "workflows", "run1", "journal.jsonl")),
		filepath.Join(from.Dir, "file-history", id1, "snap.txt"):                                         readStr(t, filepath.Join(from.Dir, "file-history", id1, "snap.txt")),
		filepath.Join(from.Dir, "projects", key, "memory", "deep", "note.md"):                            readStr(t, filepath.Join(from.Dir, "projects", key, "memory", "deep", "note.md")),
		filepath.Join(outside, "secret.txt"):                                                             readStr(t, filepath.Join(outside, "secret.txt")),
	}
	srcMtime := mustMtime(t, transcriptPath(from, key, id1))

	op, err := Plan(id1, from, to, Copy, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if op.Action != Create {
		t.Fatalf("action %s", op.Action)
	}
	res, err := Apply(op, Options{})
	if err != nil {
		t.Fatal(err)
	}

	dstTranscript := transcriptPath(to, key, id1)
	if readStr(t, dstTranscript) != "line-1\nline-2\n" {
		t.Fatalf("transcript %q", readStr(t, dstTranscript))
	}
	// A copy keeps its own times, so it never looks older than its original.
	if got := mustMtime(t, dstTranscript); !got.After(srcTime) {
		t.Fatalf("mtime %v, want later than the source's %v", got, srcTime)
	}
	journal := filepath.Join(to.Dir, "projects", key, id1, "subagents", "workflows", "run1", "journal.jsonl")
	if readStr(t, journal) != "journal\n" {
		t.Fatalf("journal %q", readStr(t, journal))
	}
	if readStr(t, filepath.Join(to.Dir, "file-history", id1, "snap.txt")) != "snap\n" {
		t.Fatal("file-history")
	}
	if readStr(t, filepath.Join(sidecar, "prefix.txt")) != "hello world" {
		t.Fatal("prefix was not extended")
	}
	if got := mustMtime(t, filepath.Join(sidecar, "prefix.txt")); !got.After(srcTime) {
		t.Fatalf("prefix mtime %v", got)
	}
	if readStr(t, filepath.Join(sidecar, "conflict.txt")) != "target-version" {
		t.Fatal("conflict was overwritten")
	}
	if got := mustMtime(t, filepath.Join(sidecar, "same.txt")); !got.Equal(oldTime) {
		t.Fatalf("identical file was rewritten, mtime %v", got)
	}

	mustContain(t, res.Written, dstTranscript)
	mustContain(t, res.Written, journal)
	mustContain(t, res.Written, filepath.Join(sidecar, "prefix.txt"))
	mustContain(t, res.Written, filepath.Join(to.Dir, "file-history", id1, "snap.txt"))
	mustContain(t, res.Skipped, filepath.Join(sidecar, "same.txt"))
	mustContain(t, res.Skipped, link+symlinkMark)
	mustContain(t, res.Skipped, dirlink+symlinkMark)
	mustContain(t, res.Conflicts, filepath.Join(sidecar, "conflict.txt"))
	mustNotContain(t, res.Written, filepath.Join(sidecar, "conflict.txt"))
	mustNotContain(t, res.Written, filepath.Join(sidecar, "same.txt"))

	if _, err := os.Lstat(filepath.Join(sidecar, "link")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("symlink was copied")
	}
	if _, err := os.Lstat(filepath.Join(sidecar, "dirlink", "hidden.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("symlink directory was followed")
	}
	if _, err := os.Lstat(filepath.Join(to.Dir, "projects", key, "memory", "deep", "note.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("memory was copied")
	}
	if _, err := os.Lstat(filepath.Join(to.Dir, "projects", key, "readme.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unrelated project file was copied")
	}
	info, err := os.Stat(filepath.Join(to.Dir, "projects", key))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("project dir mode %o", info.Mode().Perm())
	}

	for path, content := range before {
		if readStr(t, path) != content {
			t.Fatalf("source changed: %s", path)
		}
	}
	if got := mustMtime(t, transcriptPath(from, key, id1)); !got.Equal(srcMtime) {
		t.Fatal("source mtime changed")
	}
	if readStr(t, filepath.Join(outside, "secret.txt")) != "super-secret" {
		t.Fatal("symlink target changed")
	}

	t.Run("fast-forward", func(t *testing.T) {
		ffFrom := makeAcct(t, root, "ff-from")
		ffTo := makeAcct(t, root, "ff-to")
		writeFile(t, transcriptPath(ffFrom, key, id2), "hello\nworld\n", srcTime)
		writeFile(t, transcriptPath(ffTo, key, id2), "hello\n", oldTime)
		op, err := Plan(id2, ffFrom, ffTo, Copy, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if op.Action != FastForward {
			t.Fatalf("action %s", op.Action)
		}
		if _, err := Apply(op, Options{}); err != nil {
			t.Fatal(err)
		}
		if readStr(t, transcriptPath(ffTo, key, id2)) != "hello\nworld\n" {
			t.Fatal("fast-forward content")
		}
		if got := mustMtime(t, transcriptPath(ffTo, key, id2)); !got.After(srcTime) {
			t.Fatalf("fast-forward mtime %v", got)
		}
	})

	t.Run("up to date still copies a missing sidecar", func(t *testing.T) {
		uFrom := makeAcct(t, root, "up-from")
		uTo := makeAcct(t, root, "up-to")
		writeFile(t, transcriptPath(uFrom, key, id3), "same\n", srcTime)
		writeFile(t, transcriptPath(uTo, key, id3), "same\n", oldTime)
		writeFile(t, filepath.Join(uFrom.Dir, "projects", key, id3, "tool-results", "out.txt"), "tool\n", srcTime)
		op, err := Plan(id3, uFrom, uTo, Copy, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if op.Action != UpToDate || op.Bytes != 0 {
			t.Fatalf("%#v", op)
		}
		res, err := Apply(op, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if got := mustMtime(t, transcriptPath(uTo, key, id3)); !got.Equal(oldTime) {
			t.Fatal("up-to-date transcript was rewritten")
		}
		mustNotContain(t, res.Written, transcriptPath(uTo, key, id3))
		got := filepath.Join(uTo.Dir, "projects", key, id3, "tool-results", "out.txt")
		if readStr(t, got) != "tool\n" {
			t.Fatal("sidecar was not copied")
		}
		mustContain(t, res.Written, got)
	})
}

func TestApplyMove(t *testing.T) {
	root := t.TempDir()
	from := makeAcct(t, root, "from")
	to := makeAcct(t, root, "to")
	archive := filepath.Join(root, "archive")
	key := "proj"
	when := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	opt := Options{ArchiveDir: archive, Now: func() time.Time { return when }}

	writeFile(t, transcriptPath(from, key, id1), "moved\n", time.Time{})
	writeFile(t, filepath.Join(from.Dir, "projects", key, id1, "subagents", "agent-a.jsonl"), "agent\n", time.Time{})
	writeFile(t, filepath.Join(from.Dir, "projects", key, id1, "scratchpad", "n.txt"), "note\n", time.Time{})
	writeFile(t, filepath.Join(from.Dir, "file-history", id1, "snap.txt"), "snap\n", time.Time{})
	writeFile(t, filepath.Join(from.Dir, "projects", key, "memory", "MEMORY.md"), "remember\n", time.Time{})
	writeFile(t, transcriptPath(from, key, id2), "other session\n", time.Time{})
	writeFile(t, filepath.Join(from.Dir, "projects", key, id2, "keep.txt"), "stay\n", time.Time{})

	op, err := Plan(id1, from, to, Move, opt)
	if err != nil {
		t.Fatal(err)
	}
	if op.Action != Create {
		t.Fatalf("action %s", op.Action)
	}
	res, err := Apply(op, opt)
	if err != nil {
		t.Fatal(err)
	}
	wantArchive := filepath.Join(archive, "20260304-050607", "from")
	if res.Archive != wantArchive {
		t.Fatalf("archive %s", res.Archive)
	}
	if readStr(t, transcriptPath(to, key, id1)) != "moved\n" {
		t.Fatal("target transcript")
	}
	if readStr(t, filepath.Join(to.Dir, "projects", key, id1, "subagents", "agent-a.jsonl")) != "agent\n" {
		t.Fatal("target sidecar")
	}
	if readStr(t, filepath.Join(to.Dir, "file-history", id1, "snap.txt")) != "snap\n" {
		t.Fatal("target history")
	}

	for _, p := range []string{
		transcriptPath(from, key, id1),
		filepath.Join(from.Dir, "projects", key, id1),
		filepath.Join(from.Dir, "file-history", id1),
	} {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("source still present: %s (%v)", p, err)
		}
	}
	if readStr(t, filepath.Join(wantArchive, "projects", key, id1+".jsonl")) != "moved\n" {
		t.Fatal("archived transcript")
	}
	if readStr(t, filepath.Join(wantArchive, "projects", key, id1, "subagents", "agent-a.jsonl")) != "agent\n" {
		t.Fatal("archived sidecar")
	}
	if readStr(t, filepath.Join(wantArchive, "projects", key, id1, "scratchpad", "n.txt")) != "note\n" {
		t.Fatal("archived scratchpad")
	}
	if readStr(t, filepath.Join(wantArchive, "file-history", id1, "snap.txt")) != "snap\n" {
		t.Fatal("archived history")
	}
	mustContain(t, res.Removed, transcriptPath(from, key, id1))
	mustContain(t, res.Removed, filepath.Join(from.Dir, "projects", key, id1, "subagents", "agent-a.jsonl"))
	mustContain(t, res.Removed, filepath.Join(from.Dir, "file-history", id1, "snap.txt"))

	if readStr(t, filepath.Join(from.Dir, "projects", key, "memory", "MEMORY.md")) != "remember\n" {
		t.Fatal("memory was touched")
	}
	if readStr(t, transcriptPath(from, key, id2)) != "other session\n" {
		t.Fatal("other session was touched")
	}
	if readStr(t, filepath.Join(from.Dir, "projects", key, id2, "keep.txt")) != "stay\n" {
		t.Fatal("other sidecar was touched")
	}
	if _, err := os.Lstat(filepath.Join(wantArchive, "projects", key, "memory")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("memory was archived")
	}
	if _, err := os.Lstat(filepath.Join(wantArchive, "projects", key, id2+".jsonl")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("other session was archived")
	}
	if _, err := os.Stat(filepath.Join(from.Dir, "projects", key)); err != nil {
		t.Fatal("project directory was removed")
	}
}

func TestApplyMoveConflict(t *testing.T) {
	root := t.TempDir()
	from := makeAcct(t, root, "from")
	to := makeAcct(t, root, "to")
	archive := filepath.Join(root, "archive")
	key := "proj"
	when := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	opt := Options{ArchiveDir: archive, Now: func() time.Time { return when }}

	writeFile(t, transcriptPath(from, key, id1), "body\n", time.Time{})
	writeFile(t, filepath.Join(from.Dir, "projects", key, id1, "note.txt"), "source-note", time.Time{})
	writeFile(t, filepath.Join(to.Dir, "projects", key, id1, "note.txt"), "target-note", time.Time{})

	op, err := Plan(id1, from, to, Move, opt)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Apply(op, opt)
	if err == nil {
		t.Fatal("expected move to stop")
	}
	if !strings.Contains(err.Error(), "move stopped") {
		t.Fatalf("error %v", err)
	}
	if res.Archive != "" || len(res.Removed) != 0 {
		t.Fatalf("archived anyway: %#v", res)
	}
	mustContain(t, res.Conflicts, filepath.Join(to.Dir, "projects", key, id1, "note.txt"))
	if readStr(t, transcriptPath(from, key, id1)) != "body\n" {
		t.Fatal("source transcript removed")
	}
	if readStr(t, filepath.Join(from.Dir, "projects", key, id1, "note.txt")) != "source-note" {
		t.Fatal("source sidecar removed")
	}
	if readStr(t, filepath.Join(to.Dir, "projects", key, id1, "note.txt")) != "target-note" {
		t.Fatal("conflicting file was overwritten")
	}
	if _, err := os.Lstat(filepath.Join(archive, "20260304-050607")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("archive was created")
	}
}

func TestApplyStalePlan(t *testing.T) {
	root := t.TempDir()
	from := makeAcct(t, root, "from")
	to := makeAcct(t, root, "to")
	writeFile(t, transcriptPath(from, "proj", id1), "original\n", time.Time{})
	op, err := Plan(id1, from, to, Copy, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if op.Action != Create {
		t.Fatalf("action %s", op.Action)
	}
	dst := transcriptPath(to, "proj", id1)
	writeFile(t, dst, "diverged\n", time.Unix(1_500_000_000, 0))
	before := readStr(t, dst)
	mtime := mustMtime(t, dst)

	res, err := Apply(op, Options{})
	if err == nil {
		t.Fatal("expected refusal")
	}
	if !strings.Contains(err.Error(), reasonDiverged) {
		t.Fatalf("error %v", err)
	}
	if len(res.Written) != 0 || res.Archive != "" {
		t.Fatalf("wrote anyway: %#v", res)
	}
	if readStr(t, dst) != before {
		t.Fatalf("target changed to %q", readStr(t, dst))
	}
	if got := mustMtime(t, dst); !got.Equal(mtime) {
		t.Fatal("target mtime changed")
	}
	if readStr(t, transcriptPath(from, "proj", id1)) != "original\n" {
		t.Fatal("source changed")
	}
}

func TestPathSafety(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "acct")
	if err := os.MkdirAll(filepath.Join(dir, "projects"), 0o700); err != nil {
		t.Fatal(err)
	}
	badKeys := []string{".", "..", ".hidden", "a/b", `a\b`, "foo/../../etc"}
	for _, key := range badKeys {
		if _, err := sessionPaths(dir, key, id1); err == nil {
			t.Fatalf("key %q accepted", key)
		}
	}
	upper := strings.ToUpper("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	badIDs := []string{"..", "../" + id1, id1 + "/..", "not-a-uuid", upper}
	for _, id := range badIDs {
		if _, err := sessionPaths(dir, "proj", id); err == nil {
			t.Fatalf("id %q accepted", id)
		}
		if _, err := Locate(account.Account{Name: "a", Dir: dir}, id); !errors.Is(err, errInvalidID) {
			t.Fatalf("Locate %q: %v", id, err)
		}
	}
	escaped := filepath.Join(dir, "projects", "..", "..", "outside")
	if within(dir, escaped) {
		t.Fatalf("within accepted %s", escaped)
	}
	ok, err := sessionPaths(dir, "proj", id1)
	if err != nil {
		t.Fatal(err)
	}
	if !within(dir, ok.transcript) || !within(dir, ok.sidecar) || !within(dir, ok.history) {
		t.Fatalf("valid paths rejected: %#v", ok)
	}

	// A move whose account name would leave the archive dir writes nothing.
	from := makeAcct(t, root, "from")
	to := makeAcct(t, root, "to")
	writeFile(t, transcriptPath(from, "proj", id1), "body\n", time.Time{})
	from.Name = ".."
	archive := filepath.Join(root, "archive")
	op, err := Plan(id1, from, to, Move, Options{ArchiveDir: archive})
	if err != nil {
		t.Fatal(err)
	}
	if op.Action != Create {
		t.Fatalf("action %s reason %s", op.Action, op.Reason)
	}
	if _, err := Apply(op, Options{ArchiveDir: archive, Now: func() time.Time {
		return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	}}); err == nil {
		t.Fatal("expected archive path to be rejected")
	}
	if readStr(t, transcriptPath(from, "proj", id1)) != "body\n" {
		t.Fatal("source was removed")
	}
	if _, err := os.Lstat(transcriptPath(to, "proj", id1)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("target was written after a rejected archive path")
	}
	outside := filepath.Join(filepath.Dir(archive), "projects")
	if _, err := os.Lstat(outside); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("wrote outside the archive dir")
	}
}

func mustMtime(t *testing.T, path string) time.Time {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.ModTime()
}

// Two accounts whose projects folder is the same directory through a symlink:
// a move would archive the session out of both.
func TestSharedFoldersRefused(t *testing.T) {
	root := t.TempDir()
	from := makeAcct(t, root, "a")
	to := account.Account{Name: "b", Dir: filepath.Join(root, "accounts", "b")}
	if err := os.MkdirAll(to.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(from.Dir, "projects"), filepath.Join(to.Dir, "projects")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, transcriptPath(from, "k", id1), "line\n", time.Time{})
	for _, mode := range []Mode{Copy, Move} {
		op, err := Plan(id1, from, to, mode, Options{ArchiveDir: filepath.Join(root, "archive")})
		if err != nil {
			t.Fatal(err)
		}
		if op.Action != Refuse || op.Reason != reasonShared {
			t.Fatalf("mode %v: %s %q, want refused as shared", mode, op.Action, op.Reason)
		}
	}
	if readStr(t, transcriptPath(from, "k", id1)) != "line\n" {
		t.Fatal("source changed")
	}
}

// The target holding the session under another project folder must not get a
// second copy.
func TestOtherProjectKeyRefused(t *testing.T) {
	root := t.TempDir()
	from := makeAcct(t, root, "a")
	to := makeAcct(t, root, "b")
	writeFile(t, transcriptPath(from, "k1", id1), "line-1\nline-2\n", time.Time{})
	writeFile(t, transcriptPath(to, "k2", id1), "line-1\n", time.Time{})
	op, err := Plan(id1, from, to, Copy, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if op.Action != Refuse || !strings.Contains(op.Reason, "k2") {
		t.Fatalf("%s %q, want refused naming k2", op.Action, op.Reason)
	}
	if _, err := os.Lstat(transcriptPath(to, "k1", id1)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("second copy written")
	}
}

// A symlink inside the session folder is not copied, so a move must stop
// before archiving anything.
func TestMoveWithSymlinkInSidecarStops(t *testing.T) {
	root := t.TempDir()
	from := makeAcct(t, root, "a")
	to := makeAcct(t, root, "b")
	writeFile(t, transcriptPath(from, "k", id1), "line\n", time.Time{})
	side := filepath.Join(from.Dir, "projects", "k", id1)
	writeFile(t, filepath.Join(side, "subagents", "agent-x.jsonl"), "x\n", time.Time{})
	if err := os.Symlink("/etc/hosts", filepath.Join(side, "link")); err != nil {
		t.Fatal(err)
	}
	opt := Options{ArchiveDir: filepath.Join(root, "archive")}
	op, err := Plan(id1, from, to, Move, opt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(op, opt); err == nil || !strings.Contains(err.Error(), "use copy instead") {
		t.Fatalf("err = %v, want a refusal to move", err)
	}
	if readStr(t, transcriptPath(from, "k", id1)) != "line\n" {
		t.Fatal("source transcript was archived")
	}
	if _, err := os.Lstat(filepath.Join(side, "link")); err != nil {
		t.Fatal("source sidecar was archived")
	}
}

// A symlinked folder inside the target's session folder must not be written
// through.
func TestSymlinkedTargetFolderRefused(t *testing.T) {
	root := t.TempDir()
	from := makeAcct(t, root, "a")
	to := makeAcct(t, root, "b")
	// The session folder itself is a symlink to a folder that already has the
	// subfolder, so creating the subfolder alone would not notice.
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(filepath.Join(outside, "subagents"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, transcriptPath(from, "k", id1), "line\n", time.Time{})
	writeFile(t, filepath.Join(from.Dir, "projects", "k", id1, "subagents", "agent-x.jsonl"), "x\n", time.Time{})
	if err := os.MkdirAll(filepath.Join(to.Dir, "projects", "k"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(to.Dir, "projects", "k", id1)); err != nil {
		t.Fatal(err)
	}
	op, err := Plan(id1, from, to, Copy, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(op, Options{}); err == nil {
		t.Fatal("wrote through a symlinked folder")
	}
	if ents, _ := os.ReadDir(filepath.Join(outside, "subagents")); len(ents) != 0 {
		t.Fatalf("%d files landed outside the account", len(ents))
	}
}

func TestSideFoldersAndImports(t *testing.T) {
	root := t.TempDir()
	a := makeAcct(t, root, "default")
	b := makeAcct(t, root, "work")
	line1 := `{"type":"user","sessionId":"` + id1 + `"}` + "\n"
	line2 := `{"type":"assistant","sessionId":"` + id1 + `"}` + "\n"
	writeFile(t, transcriptPath(a, "proj", id1), line1+line2, time.Time{})
	writeFile(t, filepath.Join(a.Dir, "projects", "proj", id1, "subagents", "agent-x.jsonl"), line1, time.Time{})
	// The session moved into a worktree for a while: a side folder.
	writeFile(t, filepath.Join(a.Dir, "projects", "proj--worktrees-x", id1, "workflows", "scripts", "s-wf_1.js"), "js", time.Time{})

	src, err := Locate(a, id1)
	if err != nil {
		t.Fatal(err)
	}
	if src.ProjectKey != "proj" || len(src.SideDirs) != 1 || !strings.HasSuffix(src.SideDirs[0], filepath.Join("proj--worktrees-x", id1)) {
		t.Fatalf("locate: %#v", src)
	}
	// The target has an older copy: the first line only.
	writeFile(t, transcriptPath(b, "proj", id1), line1, time.Time{})
	op, err := Plan(id1, a, b, Copy, Options{})
	if err != nil || op.Action != FastForward {
		t.Fatalf("plan: %+v %v", op, err)
	}
	res, err := Apply(op, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !isRegular(filepath.Join(b.Dir, "projects", "proj--worktrees-x", id1, "workflows", "scripts", "s-wf_1.js")) {
		t.Fatalf("side folder not copied: %+v", res.Written)
	}
	want := map[string]Import{
		transcriptPath(b, "proj", id1):                                              {From: int64(len(line1)), To: int64(len(line1 + line2))},
		filepath.Join(b.Dir, "projects", "proj", id1, "subagents", "agent-x.jsonl"): {From: 0, To: int64(len(line1))},
	}
	if len(res.Imports) != len(want) {
		t.Fatalf("imports = %+v", res.Imports)
	}
	for _, im := range res.Imports {
		w, ok := want[im.Path]
		if !ok || im.From != w.From || im.To != w.To {
			t.Fatalf("import %+v, want %+v", im, w)
		}
	}
}

func TestForkRewritesIDAndSkipsPartialLine(t *testing.T) {
	root := t.TempDir()
	a := makeAcct(t, root, "default")
	b := makeAcct(t, root, "work")
	full := `{"type":"user","sessionId":"` + id1 + `","message":{"content":"see ` + id1 + `"}}` + "\n"
	partial := `{"type":"assistant","sessionId":"` + id1
	writeFile(t, transcriptPath(a, "proj", id1), full+partial, time.Time{})
	writeFile(t, filepath.Join(a.Dir, "projects", "proj", id1, "subagents", "agent-x.jsonl"), `{"session_id":"`+id1+`"}`+"\n", time.Time{})
	writeFile(t, filepath.Join(a.Dir, "projects", "proj", id1, "subagents", "agent-x.meta.json"), `{"agentType":"x"}`, time.Time{})
	writeFile(t, filepath.Join(a.Dir, "file-history", id1, "snap"), "s", time.Time{})

	p, err := PlanFork(id1, a, b, id2, Cut{})
	if err != nil || p.Reason != "" || p.Folders != 2 {
		t.Fatalf("plan: %+v %v", p, err)
	}
	res, err := Fork(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(transcriptPath(b, "proj", id2))
	wantLine := `{"type":"user","sessionId":"` + id2 + `","message":{"content":"see ` + id1 + `"}}` + "\n"
	if string(got) != wantLine {
		t.Fatalf("forked transcript = %q", got)
	}
	sub, _ := os.ReadFile(filepath.Join(b.Dir, "projects", "proj", id2, "subagents", "agent-x.jsonl"))
	if string(sub) != `{"session_id":"`+id2+`"}`+"\n" {
		t.Fatalf("forked subagent = %q", sub)
	}
	if !isRegular(filepath.Join(b.Dir, "projects", "proj", id2, "subagents", "agent-x.meta.json")) || !isRegular(filepath.Join(b.Dir, "file-history", id2, "snap")) {
		t.Fatalf("folders not copied: %v", res.Written)
	}
	if len(res.Imports) != 2 || res.Imports[0].From != 0 {
		t.Fatalf("imports = %+v", res.Imports)
	}
	// The source is untouched, and forking again to the same id is refused.
	if src, _ := os.ReadFile(transcriptPath(a, "proj", id1)); string(src) != full+partial {
		t.Fatalf("source changed")
	}
	if p2, _ := PlanFork(id1, a, b, id2, Cut{}); p2.Reason == "" {
		t.Fatalf("a second fork to the same id was not refused")
	}
	if id, err := NewID(); err != nil || !validID(id) {
		t.Fatalf("NewID = %q %v", id, err)
	}
}

// A fork cut at a turn keeps the transcript up to the next turn's prompt and
// leaves out the agents started after it, with their .meta.json.
func TestForkCutAtTurn(t *testing.T) {
	root := t.TempDir()
	a := makeAcct(t, root, "default")
	b := makeAcct(t, root, "work")
	turn1 := `{"type":"user","sessionId":"` + id1 + `","message":{"content":"one"}}` + "\n" +
		`{"type":"assistant","sessionId":"` + id1 + `","message":{"content":"reply one"}}` + "\n"
	turn2 := `{"type":"user","sessionId":"` + id1 + `","message":{"content":"two"}}` + "\n"
	writeFile(t, transcriptPath(a, "proj", id1), turn1+turn2, time.Time{})
	subs := filepath.Join(a.Dir, "projects", "proj", id1, "subagents")
	writeFile(t, filepath.Join(subs, "agent-early.jsonl"), `{"session_id":"`+id1+`"}`+"\n", time.Time{})
	writeFile(t, filepath.Join(subs, "agent-late.jsonl"), `{"session_id":"`+id1+`"}`+"\n", time.Time{})
	writeFile(t, filepath.Join(subs, "agent-late.meta.json"), `{"agentType":"x"}`, time.Time{})

	// An offset that is not the start of a line means the index is stale.
	if p, err := PlanFork(id1, a, b, id2, Cut{Turn: 1, Turns: 2, Upto: int64(len(turn1)) - 3}); err != nil || p.Reason == "" {
		t.Fatalf("a cut inside a line was not refused: %+v %v", p, err)
	}
	// So does one that starts a line that is not a prompt.
	firstLine := int64(strings.Index(turn1, "\n") + 1)
	if p, err := PlanFork(id1, a, b, id2, Cut{Turn: 1, Turns: 2, Upto: firstLine}); err != nil || p.Reason == "" {
		t.Fatalf("a cut at a reply line was not refused: %+v %v", p, err)
	}
	cut := Cut{Turn: 1, Turns: 2, Upto: int64(len(turn1)), Leave: []string{filepath.Join(subs, "agent-late.jsonl")}}
	p, err := PlanFork(id1, a, b, id2, cut)
	if err != nil || p.Reason != "" || p.Bytes != int64(len(turn1)) || p.Turn != 1 || p.Left != 1 {
		t.Fatalf("plan: %+v %v", p, err)
	}
	res, err := Fork(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(transcriptPath(b, "proj", id2))
	if want := strings.ReplaceAll(turn1, id1, id2); string(got) != want {
		t.Fatalf("forked transcript = %q, want %q", got, want)
	}
	newSubs := filepath.Join(b.Dir, "projects", "proj", id2, "subagents")
	if !isRegular(filepath.Join(newSubs, "agent-early.jsonl")) {
		t.Fatalf("an agent from before the cut was left out: %v", res.Written)
	}
	for _, f := range []string{"agent-late.jsonl", "agent-late.meta.json"} {
		if _, err := os.Lstat(filepath.Join(newSubs, f)); err == nil {
			t.Fatalf("%s from after the cut was copied", f)
		}
	}
	if len(res.Imports) != 2 || res.Imports[0].To != int64(len(turn1)) && res.Imports[1].To != int64(len(turn1)) {
		t.Fatalf("imports = %+v", res.Imports)
	}
}

func TestCompleteLines(t *testing.T) {
	dir := t.TempDir()
	long := strings.Repeat("x", 200<<10) // a partial line longer than one read
	for _, c := range []struct {
		content string
		want    int64
	}{
		{"", 0},
		{"partial", 0},
		{"a\nb\n", 4},
		{"a\nb\npart", 4},
		{"a\n" + long, 2},
		{long + "\n" + long, int64(len(long)) + 1},
	} {
		p := filepath.Join(dir, "t.jsonl")
		if err := os.WriteFile(p, []byte(c.content), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := completeLines(p, int64(len(c.content)))
		if err != nil || got != c.want {
			t.Fatalf("completeLines(%d bytes) = %d, %v; want %d", len(c.content), got, err, c.want)
		}
	}
}
