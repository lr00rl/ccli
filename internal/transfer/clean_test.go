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

	restored, conflicts, err := Restore(dir, a)
	if err != nil || len(conflicts) != 0 {
		t.Fatalf("restore: %v, conflicts %v", err, conflicts)
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

// A transcript the account has again is left in the archive, and a folder
// holding anything but archived sessions is refused before anything moves.
func TestRestoreConflictsAndForeignFiles(t *testing.T) {
	root := t.TempDir()
	a := makeAcct(t, root, "work")
	dir := filepath.Join(root, "archive", "20261020-153000", "work")
	writeFile(t, filepath.Join(dir, "projects", "-p", id1+".jsonl"), "archived\n", time.Time{})
	writeFile(t, transcriptPath(a, "-p", id1), "newer\n", time.Time{})
	restored, conflicts, err := Restore(dir, a)
	if err != nil || len(restored) != 0 || len(conflicts) != 1 {
		t.Fatalf("restored %v conflicts %v err %v", restored, conflicts, err)
	}
	if readStr(t, transcriptPath(a, "-p", id1)) != "newer\n" || readStr(t, filepath.Join(dir, "projects", "-p", id1+".jsonl")) != "archived\n" {
		t.Fatal("a conflict changed a file")
	}

	writeFile(t, filepath.Join(dir, "settings.json"), "{}", time.Time{})
	writeFile(t, filepath.Join(dir, "projects", "-p", id2+".jsonl"), "x\n", time.Time{})
	if _, _, err := Restore(dir, a); err == nil || !strings.Contains(err.Error(), "settings.json") {
		t.Fatalf("err = %v, want settings.json refused", err)
	}
	if _, err := os.Stat(transcriptPath(a, "-p", id2)); err == nil {
		t.Fatal("a session moved although the folder was refused")
	}
}
