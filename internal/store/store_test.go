package store

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lr00rl/ccli/internal/transcript"
)

func TestOldSchemaIsSetAsideNotDeleted(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "ccli.db")
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	s.SetMeta("schema", "0")
	s.SetMeta("marker", "old history")
	s.Close()

	s, err = Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if v, _ := s.Meta("schema"); v != SchemaVersion {
		t.Fatalf("schema = %q", v)
	}
	if v, _ := s.Meta("marker"); v != "" {
		t.Fatal("new database kept old contents")
	}
	kept, _ := filepath.Glob(filepath.Join(dir, "ccli.db.schema-0.*"))
	var main string
	for _, k := range kept {
		if !strings.HasSuffix(k, "-wal") && !strings.HasSuffix(k, "-shm") {
			main = k
		}
	}
	if main == "" {
		t.Fatalf("old database not kept: %v", kept)
	}
	old, err := openDB(main)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	if v, _ := old.Meta("marker"); v != "old history" {
		t.Fatalf("old database lost its contents: %q", v)
	}
}

// Two ccli runs that planned from the same offset must not both add the same
// bytes' counters.
func TestApplyFromStaleOffsetIsSkipped(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	cost := func(string, string, transcript.Usage) (float64, bool) { return 0, false }
	u := Update{
		File:   File{Path: "/x/s1.jsonl", Account: "default", SessionID: "s1", ProjectKey: "-x", Kind: "main", Offset: 100},
		Result: transcript.Result{Lines: 3, ToolCalls: 2},
	}
	for i := 0; i < 2; i++ {
		if err := st.Apply(ctx, []Update{u}, cost); err != nil {
			t.Fatal(err)
		}
	}
	var lines, calls int
	if err := st.DB().QueryRow("SELECT lines, tool_calls FROM files").Scan(&lines, &calls); err != nil {
		t.Fatal(err)
	}
	if lines != 3 || calls != 2 {
		t.Fatalf("lines %d calls %d, want 3 and 2", lines, calls)
	}
}

// legacyTitles makes a database the way the short-lived `rename` left it:
// a titles table beside tags, one name longer than a tag may be.
func legacyTitles(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ccli.db")
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, q := range []string{
		`CREATE TABLE titles(session_id TEXT PRIMARY KEY, title TEXT NOT NULL, at INTEGER NOT NULL)`,
		`INSERT INTO titles VALUES('s1', 'payments audit', 1), ('s2', 'old name', 2), ('s3', '` + strings.Repeat("长", 50) + `', 3)`,
		`INSERT INTO tags VALUES('s2', 'kept', 3)`,
	} {
		if _, err := s.DB().Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

// Names an earlier ccli gave sessions with `rename` become tags, cut to
// MaxTag, when the database is opened, and the old table goes. A tag set
// since is kept.
func TestTitlesBecomeTags(t *testing.T) {
	s, err := Open(legacyTitles(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tags, err := s.Tags(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 3 || tags["s1"] != "payments audit" || tags["s2"] != "kept" || tags["s3"] != strings.Repeat("长", MaxTag) {
		t.Fatalf("tags %v", tags)
	}
	var n int
	if err := s.DB().QueryRow(`SELECT count(*) FROM sqlite_master WHERE name = 'titles'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("titles table left behind: %v %d", err, n)
	}
}

// Two ccli processes opening an old database at the same moment both
// succeed: the second finds the titles already moved.
func TestTitlesBecomeTagsOnceWhenOpenedTogether(t *testing.T) {
	for round := 0; round < 5; round++ {
		p := legacyTitles(t)
		var wg sync.WaitGroup
		errs := make([]error, 4)
		for i := range errs {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				s, err := Open(p)
				if err == nil {
					s.Close()
				}
				errs[i] = err
			}(i)
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("round %d, open %d: %v", round, i, err)
			}
		}
	}
}

func TestForgetRemovesOneSession(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	cost := func(string, string, transcript.Usage) (float64, bool) { return 1, true }
	resp := func(key string) transcript.Result {
		return transcript.Result{Responses: []transcript.Response{{Key: key, Model: "m", Usage: transcript.Usage{Input: 5}}}}
	}
	for _, u := range []Update{
		{File: File{Path: "/a/s1.jsonl", Account: "a", SessionID: "s1", Kind: "main"}, Result: resp("k1")},
		{File: File{Path: "/a/s1/subagents/x.jsonl", Account: "a", SessionID: "s1", Kind: "subagent"}, Result: resp("k2")},
		{File: File{Path: "/b/s1.jsonl", Account: "b", SessionID: "s1", Kind: "main"}, Result: resp("k3")},
		{File: File{Path: "/a/s2.jsonl", Account: "a", SessionID: "s2", Kind: "main"}, Result: resp("k4")},
	} {
		if err := st.Apply(ctx, []Update{u}, cost); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Hide(ctx, map[string]time.Time{"s1": time.Now()}); err != nil {
		t.Fatal(err)
	}
	n, err := st.Forget(ctx, "a", "s1")
	if err != nil || n != 2 {
		t.Fatalf("forgot %d files, err %v; want 2", n, err)
	}
	var files, responses, hidden int
	st.DB().QueryRow("SELECT COUNT(*) FROM files").Scan(&files)
	st.DB().QueryRow("SELECT COUNT(*) FROM responses").Scan(&responses)
	st.DB().QueryRow("SELECT COUNT(*) FROM hidden").Scan(&hidden)
	if files != 2 || responses != 2 || hidden != 1 {
		t.Fatalf("files %d responses %d hidden %d, want 2, 2 and 1", files, responses, hidden)
	}
}
