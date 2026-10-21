package transfer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestArchiveAndRestore(t *testing.T) {
	root := t.TempDir()
	a := makeAcct(t, root, "work")
	archive := filepath.Join(root, "archive")
	tr := transcriptPath(a, "-p", id1)
	writeFile(t, tr, `{"type":"permission-mode"}`+"\n", time.Time{})
	writeFile(t, filepath.Join(a.Dir, "projects", "-p", id1, "subagents", "agent-x.jsonl"), "{}\n", time.Time{})
	writeFile(t, filepath.Join(a.Dir, "file-history", id1, "a@v1"), "x", time.Time{})
	other := transcriptPath(a, "-p", id2)
	writeFile(t, other, "{}\n", time.Time{})

	files, err := Locate(a, id1)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 20, 15, 30, 0, 0, time.Local)
	dir, _, err := Archive(files, archive, now)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(archive, "20261020-153000", "work"); dir != want {
		t.Fatalf("archive folder %s, want %s", dir, want)
	}
	for _, p := range []string{tr, filepath.Join(a.Dir, "projects", "-p", id1), filepath.Join(a.Dir, "file-history", id1)} {
		if _, err := os.Lstat(p); err == nil {
			t.Fatalf("%s is still in the account", p)
		}
	}
	if readStr(t, other) != "{}\n" {
		t.Fatal("another session was touched")
	}

	restored, conflicts, left, err := Restore(dir, a)
	if err != nil || len(conflicts) != 0 || len(left) != 0 {
		t.Fatalf("restore: %v, conflicts %v, left %v", err, conflicts, left)
	}
	mustContain(t, restored, tr)
	if readStr(t, filepath.Join(a.Dir, "projects", "-p", id1, "subagents", "agent-x.jsonl")) != "{}\n" ||
		readStr(t, filepath.Join(a.Dir, "file-history", id1, "a@v1")) != "x" {
		t.Fatal("folders were not put back")
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("the emptied archive folder was kept")
	}
}

// A transcript the account has again stays in the archive, and anything in
// the archive that is not a session stays there too, while the sessions
// beside it move back.
func TestRestoreConflictsAndForeignFiles(t *testing.T) {
	root := t.TempDir()
	a := makeAcct(t, root, "work")
	dir := filepath.Join(root, "archive", "20261020-153000", "work")
	writeFile(t, filepath.Join(dir, "projects", "-p", id1+".jsonl"), "archived\n", time.Time{})
	writeFile(t, transcriptPath(a, "-p", id1), "newer\n", time.Time{})
	writeFile(t, filepath.Join(dir, ".DS_Store"), "x", time.Time{})
	writeFile(t, filepath.Join(dir, "projects", "-p", id2+".jsonl"), "x\n", time.Time{})
	restored, conflicts, left, err := Restore(dir, a)
	if err != nil || len(restored) != 1 || len(conflicts) != 1 || len(left) != 1 || !strings.HasSuffix(left[0], ".DS_Store") {
		t.Fatalf("restored %v conflicts %v left %v err %v", restored, conflicts, left, err)
	}
	if readStr(t, transcriptPath(a, "-p", id1)) != "newer\n" || readStr(t, filepath.Join(dir, "projects", "-p", id1+".jsonl")) != "archived\n" {
		t.Fatal("a conflict changed a file")
	}
	if readStr(t, transcriptPath(a, "-p", id2)) != "x\n" {
		t.Fatal("the session beside the foreign file did not move back")
	}
}

// A project folder in the account that is a link is not written through.
func TestRestoreRefusesALinkedProjectFolder(t *testing.T) {
	root := t.TempDir()
	a := makeAcct(t, root, "work")
	elsewhere := filepath.Join(root, "elsewhere")
	if err := os.MkdirAll(elsewhere, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(a.Dir, "projects", "-p")); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "archive", "20261020-153000", "work")
	writeFile(t, filepath.Join(dir, "projects", "-p", id1+".jsonl"), "x\n", time.Time{})
	if _, _, _, err := Restore(dir, a); err == nil {
		t.Fatal("restored through a linked project folder")
	}
	if ents, _ := os.ReadDir(elsewhere); len(ents) != 0 {
		t.Fatal("a file was written through the link")
	}
}
