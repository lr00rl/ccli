package cli

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lr00rl/ccli/internal/account"
)

func TestWriteFile(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, ".pi", "agent")
	if err := os.MkdirAll(filepath.Join(home, "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	a := &app{accts: []account.Account{{Name: "pi", Tool: account.Pi, Dir: home}}}
	text := func(s string) func(io.Writer) error {
		return func(w io.Writer) error { _, err := io.WriteString(w, s); return err }
	}

	out := filepath.Join(root, "handoff.md")
	if err := os.WriteFile(out, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.writeFile(out, text("new")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(out); string(b) != "new" {
		t.Fatalf("content = %q", b)
	}
	if st, _ := os.Stat(out); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", st.Mode().Perm())
	}

	// A link is written through, and stays a link.
	target := filepath.Join(root, "real.md")
	alias := filepath.Join(root, "link.md")
	if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}
	if err := a.writeFile(alias, text("through")); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Lstat(alias); st.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the link was replaced by a file")
	}
	if b, _ := os.ReadFile(target); string(b) != "through" {
		t.Fatalf("target = %q", b)
	}
	if err := a.writeFile(root, text("x")); err == nil {
		t.Fatal("a folder was accepted as the output file")
	}

	// A failed write keeps the old file and leaves no temporary file.
	if err := a.writeFile(out, func(w io.Writer) error { io.WriteString(w, "half"); return errors.New("boom") }); err == nil {
		t.Fatal("the write error was lost")
	}
	if b, _ := os.ReadFile(out); string(b) != "new" {
		t.Fatalf("a failed write changed the file: %q", b)
	}
	ents, _ := os.ReadDir(root)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".ccli-export-") {
			t.Fatalf("temporary file left: %s", e.Name())
		}
	}

	// Inside an agent home, through the folder itself or a link to it.
	inside := filepath.Join(home, "sessions", "x.jsonl")
	link := filepath.Join(root, "agent-link")
	if err := os.Symlink(home, link); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{inside, filepath.Join(link, "sessions", "y.jsonl")} {
		if err := a.writeFile(p, text("{}")); err == nil || !strings.Contains(err.Error(), "home") {
			t.Fatalf("%s: err = %v, want a refusal", p, err)
		}
		if _, err := os.Stat(p); err == nil {
			t.Fatalf("%s was written", p)
		}
	}
}
