package quota

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lr00rl/ccli/internal/account"
)

func TestParse(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	in := []byte(`{
		"session_id": "sess-1",
		"transcript_path": "/tmp/.claude-work/projects/-work/a.jsonl",
		"cwd": "/work",
		"model": {"id": "claude-opus-5-5", "display_name": "Opus 5.5"},
		"cost": {"total_cost_usd": 1.25},
		"rate_limits": {
			"five_hour": {"used_percentage": 42.5, "resets_at": 1791234567, "extra": true},
			"seven_day": {"used_percentage": 12, "resets_at": 1791834567},
			"ignored": 1
		}
	}`)
	s, path, ok, err := Parse(in, now)
	if err != nil || !ok {
		t.Fatalf("Parse() ok=%v err=%v", ok, err)
	}
	if path != "/tmp/.claude-work/projects/-work/a.jsonl" {
		t.Fatalf("transcript = %q", path)
	}
	if s.SessionID != "sess-1" || s.Model != "claude-opus-5-5" || !s.CapturedAt.Equal(now) {
		t.Fatalf("snapshot identity = %+v", s)
	}
	if s.FiveHour == nil || s.FiveHour.UsedPercent != 42.5 || !s.FiveHour.ResetsAt.Equal(time.Unix(1791234567, 0)) {
		t.Fatalf("five_hour = %+v", s.FiveHour)
	}
	if s.SevenDay == nil || s.SevenDay.UsedPercent != 12 || !s.SevenDay.ResetsAt.Equal(time.Unix(1791834567, 0)) {
		t.Fatalf("seven_day = %+v", s.SevenDay)
	}

	s, _, ok, err = Parse([]byte(`{"rate_limits":{"seven_day":{"used_percentage":3.5,"resets_at":100}}}`), now)
	if err != nil || !ok || s.FiveHour != nil || s.SevenDay == nil || s.SevenDay.UsedPercent != 3.5 {
		t.Fatalf("only seven_day: ok=%v err=%v five=%v seven=%+v", ok, err, s.FiveHour, s.SevenDay)
	}
	if !s.SevenDay.ResetsAt.Equal(time.Unix(100, 0)) {
		t.Fatalf("seven_day reset = %v", s.SevenDay.ResetsAt)
	}

	s, path, ok, err = Parse([]byte(`{"session_id":"x","transcript_path":"/t","model":{"id":"m"}}`), now)
	if err != nil || ok || path != "/t" || s.FiveHour != nil || s.SevenDay != nil {
		t.Fatalf("no rate_limits: ok=%v err=%v path=%q snap=%+v", ok, err, path, s)
	}
	if _, _, ok, err = Parse([]byte(`{"rate_limits":{}}`), now); err != nil || ok {
		t.Fatalf("empty rate_limits: ok=%v err=%v", ok, err)
	}

	if _, _, ok, err = Parse([]byte(`{`), now); err == nil || ok {
		t.Fatalf("malformed: ok=%v err=%v", ok, err)
	}
}

func TestWindowExpired(t *testing.T) {
	reset := time.Unix(1_700_000_000, 0).UTC()
	w := Window{UsedPercent: 40, ResetsAt: reset}
	if w.Expired(reset.Add(-time.Second)) {
		t.Fatal("expired before reset")
	}
	if !w.Expired(reset) {
		t.Fatal("still open at reset")
	}
	if !w.Expired(reset.Add(time.Second)) {
		t.Fatal("open after reset")
	}
	// A source that leaves the reset time out leaves it unknown, never 1970.
	s, _, ok, err := Parse([]byte(`{"rate_limits":{"five_hour":{"used_percentage":12}}}`), reset)
	if err != nil || !ok || !s.FiveHour.ResetsAt.IsZero() || s.FiveHour.Expired(reset) {
		t.Fatalf("no reset time: %+v, ok %v, err %v", s.FiveHour, ok, err)
	}
}

func TestAccountFor(t *testing.T) {
	tmp := t.TempDir()
	def := filepath.Join(tmp, ".claude")
	work := filepath.Join(tmp, ".claude-work")
	old := filepath.Join(tmp, ".claude-work-old")
	accts := []account.Account{
		{Name: "default", Dir: def},
		{Name: "work", Dir: work},
		{Name: "work-old", Dir: old},
	}

	got, ok := AccountFor(filepath.Join(def, "projects", "-work", "a.jsonl"), "", accts)
	if !ok || got.Name != "default" {
		t.Fatalf("default = %+v ok=%v", got, ok)
	}
	got, ok = AccountFor(filepath.Join(work, "projects", "-work", "b.jsonl"), def, accts)
	if !ok || got.Name != "work" {
		t.Fatalf("work = %+v ok=%v", got, ok)
	}
	got, ok = AccountFor(filepath.Join(old, "projects", "x"), "", accts)
	if !ok || got.Name != "work-old" {
		t.Fatalf("work-old = %+v ok=%v", got, ok)
	}

	only := []account.Account{{Name: "work", Dir: work}}
	if _, ok = AccountFor(filepath.Join(old, "projects", "x"), "", only); ok {
		t.Fatal("prefix trap: .claude-work-old matched .claude-work")
	}
	if _, ok = AccountFor(filepath.Join(work, "settings.json"), "", only); ok {
		t.Fatal("matched a path outside projects")
	}

	got, ok = AccountFor(filepath.Join(tmp, "elsewhere", "z.jsonl"), work+"/", accts)
	if !ok || got.Name != "work" {
		t.Fatalf("fallback = %+v ok=%v", got, ok)
	}

	other := []account.Account{{Name: "work", Dir: work}}
	if _, ok = AccountFor(filepath.Join(tmp, "nope", "z.jsonl"), "", other); ok {
		t.Fatal("unrelated path with empty env matched an account")
	}
}

func TestSaveLoad(t *testing.T) {
	dir := t.TempDir()
	newer := Snapshot{
		Account:    "default",
		ConfigDir:  filepath.Join(dir, ".claude"),
		SessionID:  "new",
		Model:      "claude-opus-5-5",
		CapturedAt: time.Unix(2_000, 0).UTC(),
		FiveHour:   &Window{UsedPercent: 42.5, ResetsAt: time.Unix(3_000, 0).UTC()},
		SevenDay:   &Window{UsedPercent: 12, ResetsAt: time.Unix(9_000, 0).UTC()},
	}
	older := newer
	older.SessionID = "old"
	older.CapturedAt = time.Unix(1_000, 0).UTC()
	older.FiveHour = &Window{UsedPercent: 1, ResetsAt: time.Unix(1_500, 0).UTC()}

	if err := Save(dir, older); err != nil {
		t.Fatal(err)
	}
	if err := Save(dir, newer); err != nil {
		t.Fatal(err)
	}
	if err := Save(dir, older); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, "quota", "default.json")
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("file mode %o", st.Mode().Perm())
	}
	dst, err := os.Stat(filepath.Join(dir, "quota"))
	if err != nil {
		t.Fatal(err)
	}
	if dst.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %o", dst.Mode().Perm())
	}

	work := newer
	work.Account = "work"
	work.SessionID = "t"
	work.FiveHour = nil
	if err := Save(dir, work); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "quota", "bad.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "quota", "note.txt"), []byte("ignore"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("loaded %d snapshots: %+v", len(got), got)
	}
	d := got["default"]
	if d.SessionID != "new" || d.Model != newer.Model || d.ConfigDir != newer.ConfigDir || !d.CapturedAt.Equal(newer.CapturedAt) {
		t.Fatalf("default = %+v", d)
	}
	if d.FiveHour == nil || d.FiveHour.UsedPercent != 42.5 || !d.FiveHour.ResetsAt.Equal(newer.FiveHour.ResetsAt) {
		t.Fatalf("five_hour = %+v", d.FiveHour)
	}
	if d.SevenDay == nil || d.SevenDay.UsedPercent != 12 || !d.SevenDay.ResetsAt.Equal(newer.SevenDay.ResetsAt) {
		t.Fatalf("seven_day = %+v", d.SevenDay)
	}
	if got["work"].SessionID != "t" || got["work"].FiveHour != nil {
		t.Fatalf("work = %+v", got["work"])
	}
	if _, ok := got["bad"]; ok {
		t.Fatal("loaded corrupt file")
	}

	missing := filepath.Join(t.TempDir(), "missing")
	got, err = Load(missing)
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("missing folder: len=%d err=%v nil=%v", len(got), err, got == nil)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("Load created a missing folder")
	}
}

func TestWrap(t *testing.T) {
	dir := t.TempDir()
	acctDir := filepath.Join(dir, ".claude-work")
	transcript := filepath.Join(acctDir, "projects", "-work", "s.jsonl")
	accts := []account.Account{{Name: "work", Dir: acctDir}}
	now := time.Unix(1_700_000_000, 0).UTC()
	input := []byte(`{
		"session_id": "sid",
		"transcript_path": "` + transcript + `",
		"model": {"id": "claude-opus-5-5", "display_name": "Opus 5.5"},
		"ignored": true,
		"rate_limits": {
			"five_hour": {"used_percentage": 42.5, "resets_at": 1791234567},
			"seven_day": {"used_percentage": 12, "resets_at": 1791834567}
		}
	}`)

	var out, errb bytes.Buffer
	code := Wrap([]string{"/bin/cat"}, bytes.NewReader(input), &out, &errb, dir, accts, "", now)
	if code != 0 || errb.Len() != 0 || !bytes.Equal(out.Bytes(), input) {
		t.Fatalf("cat code=%d stderr=%q stdout mismatch %v", code, errb.String(), !bytes.Equal(out.Bytes(), input))
	}
	snaps, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := snaps["work"]
	if s.Account != "work" || s.ConfigDir != acctDir || s.SessionID != "sid" || s.Model != "claude-opus-5-5" || !s.CapturedAt.Equal(now) {
		t.Fatalf("recorded = %+v", s)
	}
	if s.FiveHour == nil || s.FiveHour.UsedPercent != 42.5 || s.SevenDay == nil || s.SevenDay.UsedPercent != 12 {
		t.Fatalf("windows = %+v %+v", s.FiveHour, s.SevenDay)
	}

	out.Reset()
	errb.Reset()
	plain := []byte("not json\n")
	code = Wrap([]string{"/bin/cat"}, bytes.NewReader(plain), &out, &errb, dir, accts, "", now)
	if code != 0 || !bytes.Equal(out.Bytes(), plain) {
		t.Fatalf("plain cat code=%d stdout=%q", code, out.Bytes())
	}
	snaps, err = Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if snaps["work"].SessionID != "sid" {
		t.Fatal("non-json input replaced the snapshot")
	}

	out.Reset()
	errb.Reset()
	code = Wrap([]string{"/bin/sh", "-c", "exit 3"}, bytes.NewReader(nil), &out, &errb, dir, accts, "", now)
	if code != 3 {
		t.Fatalf("exit code %d", code)
	}

	out.Reset()
	errb.Reset()
	code = Wrap([]string{"/bin/sh", "-c", "echo OUT; echo ERR >&2"}, bytes.NewReader(nil), &out, &errb, dir, nil, "", now)
	if code != 0 || out.String() != "OUT\n" || errb.String() != "ERR\n" {
		t.Fatalf("stdio code=%d stdout=%q stderr=%q", code, out.String(), errb.String())
	}

	out.Reset()
	errb.Reset()
	code = Wrap(nil, bytes.NewReader([]byte("x")), &out, &errb, dir, accts, "", now)
	if code != 0 || out.Len() != 0 || errb.Len() != 0 {
		t.Fatalf("no args code=%d stdout=%q stderr=%q", code, out.Bytes(), errb.Bytes())
	}

	out.Reset()
	errb.Reset()
	code = Wrap([]string{"/no/such/ccli-statusline-command"}, bytes.NewReader([]byte("x")), &out, &errb, dir, accts, "", now)
	if code != 127 || out.Len() != 0 || bytes.Count(errb.Bytes(), []byte("\n")) != 1 {
		t.Fatalf("missing code=%d stdout=%q stderr=%q", code, out.Bytes(), errb.Bytes())
	}

	body := statusPayload(transcript)
	exact := append(body, bytes.Repeat([]byte(" "), maxParse-len(body))...)
	if len(exact) != maxParse {
		t.Fatalf("exact len %d", len(exact))
	}
	out.Reset()
	errb.Reset()
	code = Wrap([]string{"/bin/cat"}, bytes.NewReader(exact), &out, &errb, dir, accts, "", now.Add(time.Second))
	if code != 0 || !bytes.Equal(out.Bytes(), exact) {
		t.Fatalf("exact passthrough code=%d match=%v", code, bytes.Equal(out.Bytes(), exact))
	}
	snaps, err = Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !snaps["work"].CapturedAt.Equal(now.Add(time.Second)) || snaps["work"].SessionID != "boundary" {
		t.Fatalf("1 MiB snapshot = %+v", snaps["work"])
	}

	over := append(append([]byte{}, exact...), bytes.Repeat([]byte(" "), 32)...)
	fresh := t.TempDir()
	out.Reset()
	errb.Reset()
	code = Wrap([]string{"/bin/cat"}, bytes.NewReader(over), &out, &errb, fresh, accts, "", now)
	if code != 0 || !bytes.Equal(out.Bytes(), over) {
		t.Fatalf("over passthrough code=%d got=%d want=%d", code, out.Len(), len(over))
	}
	snaps, err = Load(fresh)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 0 {
		t.Fatalf("parsed input over 1 MiB: %+v", snaps)
	}
}

func statusPayload(transcript string) []byte {
	return []byte(`{"session_id":"boundary","transcript_path":"` + transcript + `","model":{"id":"claude-opus-5-5"},"rate_limits":{"five_hour":{"used_percentage":9,"resets_at":50}}}`)
}

func TestCclineCacheAndLatest(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "ccline"), 0o700)
	os.WriteFile(filepath.Join(dir, "ccline", ".api_usage_cache.json"),
		[]byte(`{"five_hour_utilization":3.0,"seven_day_utilization":10.0,"resets_at":"2026-10-22T11:00:00.4+00:00","cached_at":"2026-10-16T03:25:11+00:00"}`), 0o600)
	cc, ok := CclineCache(dir)
	if !ok || cc.FiveHour.UsedPercent != 3 || cc.SevenDay.UsedPercent != 10 || cc.SevenDay.ResetsAt.Day() != 22 {
		t.Fatalf("ccline = %+v %v", cc, ok)
	}
	reset5 := time.Date(2026, 10, 16, 6, 0, 0, 0, time.UTC)
	status := &Snapshot{CapturedAt: time.Date(2026, 10, 16, 3, 0, 0, 0, time.UTC),
		FiveHour: &Window{UsedPercent: 1, ResetsAt: reset5},
		SevenDay: &Window{UsedPercent: 9, ResetsAt: time.Date(2026, 10, 22, 11, 0, 0, 0, time.UTC)}}
	five, seven := Latest(status, &cc)
	// ccline is newer: its numbers win, and the 5-hour reset comes from the
	// status line since ccline does not keep it.
	if five.Source != "ccline" || five.UsedPercent != 3 || !five.ResetsAt.Equal(reset5) {
		t.Fatalf("five = %+v", five)
	}
	if seven.Source != "ccline" || seven.UsedPercent != 10 {
		t.Fatalf("seven = %+v", seven)
	}
	if f, _ := Latest(status, nil); f.Source != "status line" || f.UsedPercent != 1 {
		t.Fatalf("status only = %+v", f)
	}
	if _, ok := CclineCache(t.TempDir()); ok {
		t.Fatalf("missing cache read as present")
	}
}
