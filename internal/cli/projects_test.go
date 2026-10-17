package cli

import (
	"flag"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/lr00rl/ccli/internal/account"
	"github.com/lr00rl/ccli/internal/query"
)

// physical is p with symlinks resolved; on macOS t.TempDir lives under
// /var, which is a link to /private/var.
func physical(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// forms is what scopeDirs should return for a path given as given whose
// symlink-resolved form is resolved: the path as given, then the resolved
// form if it differs.
func forms(given, resolved string) []string {
	if resolved == given {
		return []string{given}
	}
	return []string{given, resolved}
}

func TestScopeDirs(t *testing.T) {
	t.Run("relative", func(t *testing.T) {
		dir := t.TempDir()
		t.Chdir(dir)
		got, err := scopeDirs(".")
		if err != nil {
			t.Fatal(err)
		}
		if want := forms(dir, physical(t, dir)); !reflect.DeepEqual(got, want) {
			t.Errorf("scopeDirs(.) = %v, want %v", got, want)
		}
	})

	t.Run("home", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		got, err := scopeDirs("~/x")
		if err != nil {
			t.Fatal(err)
		}
		want := forms(filepath.Join(home, "x"), filepath.Join(physical(t, home), "x"))
		if !reflect.DeepEqual(got, want) {
			t.Errorf("scopeDirs(~/x) = %v, want %v", got, want)
		}
		got, err = scopeDirs("~")
		if err != nil {
			t.Fatal(err)
		}
		if want := forms(home, physical(t, home)); !reflect.DeepEqual(got, want) {
			t.Errorf("scopeDirs(~) = %v, want %v", got, want)
		}
	})

	t.Run("symlink", func(t *testing.T) {
		tmp := t.TempDir()
		realDir := filepath.Join(tmp, "real")
		link := filepath.Join(tmp, "link")
		if err := os.Mkdir(realDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(realDir, link); err != nil {
			t.Fatal(err)
		}
		realResolved := physical(t, realDir)

		got, err := scopeDirs(link)
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{link, realResolved}; !reflect.DeepEqual(got, want) {
			t.Errorf("scopeDirs(link) = %v, want %v", got, want)
		}

		// A folder deleted below the link still resolves through it.
		gone := filepath.Join(link, "gone")
		got, err = scopeDirs(gone)
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{gone, filepath.Join(realResolved, "gone")}; !reflect.DeepEqual(got, want) {
			t.Errorf("scopeDirs(link/gone) = %v, want %v", got, want)
		}

		// Several missing levels keep their order.
		deep := filepath.Join(link, "a", "b")
		got, err = scopeDirs(deep)
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{deep, filepath.Join(realResolved, "a", "b")}; !reflect.DeepEqual(got, want) {
			t.Errorf("scopeDirs(link/a/b) = %v, want %v", got, want)
		}
	})

	t.Run("empty", func(t *testing.T) {
		if got, err := scopeDirs(""); err == nil {
			t.Errorf(`scopeDirs("") = %v, want an error`, got)
		}
	})
}

func TestToolFlag(t *testing.T) {
	parse := func(args ...string) (query.Filter, error) {
		var f query.Filter
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		toolFlag(fs, &f)
		return f, fs.Parse(args)
	}
	f, err := parse("-tool", "claude,codex")
	if err != nil {
		t.Fatal(err)
	}
	if want := []account.Tool{account.Claude, account.Codex}; !reflect.DeepEqual(f.Tools, want) {
		t.Errorf("-tool claude,codex set %v, want %v", f.Tools, want)
	}
	f, err = parse("-tool", "codex", "-tool", " claude")
	if err != nil {
		t.Fatal(err)
	}
	if want := []account.Tool{account.Codex, account.Claude}; !reflect.DeepEqual(f.Tools, want) {
		t.Errorf("repeated -tool set %v, want %v", f.Tools, want)
	}
	if f, err := parse("-tool", "cursor"); err == nil {
		t.Errorf("-tool cursor accepted: %v", f.Tools)
	}
}
