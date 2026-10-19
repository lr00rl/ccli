package quota

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/lr00rl/ccli/internal/account"
)

// fakeTool puts an executable shell script named name on PATH. It records
// what it saw in the file named by $FAKE_OUT.
func fakeTool(t *testing.T, bin, name, script string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func liveEnv(t *testing.T) (root, bin, out string) {
	t.Helper()
	root = t.TempDir()
	bin = filepath.Join(root, "bin")
	os.MkdirAll(bin, 0o755)
	out = filepath.Join(root, "seen")
	t.Setenv("HOME", root)
	// Only the fakes and the system's basic commands: never a real tool.
	t.Setenv("PATH", strings.Join([]string{bin, "/usr/bin", "/bin"}, string(os.PathListSeparator)))
	t.Setenv("FAKE_OUT", out)
	t.Setenv("TMUX_PANE", "%9")
	return root, bin, out
}

const claudeAnswer = `{"type":"control_response","response":{"subtype":"success","request_id":"ccli-usage","response":{"subscription_type":"max","rate_limits_available":true,"rate_limits":{"five_hour":{"utilization":26,"resets_at":"2026-10-16T12:00:00.319964+00:00"},"seven_day":{"utilization":23,"resets_at":"2026-10-22T11:00:00+00:00"},"seven_day_opus":null,"model_scoped":[{"display_name":"Fable","utilization":0,"resets_at":"2026-10-22T11:00:00+00:00"}],"extra_usage":{"is_enabled":false,"monthly_limit":1300,"used_credits":0,"utilization":0,"currency":"USD","decimal_places":2}}}}}`

// Claude Code is asked with one get_usage request, in the work folder, with
// CLAUDE_CONFIG_DIR set only for an account other than ~/.claude and no
// tmux pane variable; lines before the answer are skipped.
func TestCheckClaude(t *testing.T) {
	root, bin, out := liveEnv(t)
	fakeTool(t, bin, "claude", `read line
{ printf '%s\n' "$line"; printf 'dir=%s\n' "$CLAUDE_CONFIG_DIR"; printf 'pane=%s\n' "$TMUX_PANE"; printf 'args=%s\n' "$*"; pwd; } > "$FAKE_OUT"
echo '{"type":"system","subtype":"init"}'
echo 'not json'
echo '`+claudeAnswer+`'
`)
	work := filepath.Join(root, "work")
	before := time.Now()
	var log strings.Builder
	c := Checker{WorkDir: work, Trace: func(format string, args ...any) {
		fmt.Fprintf(&log, format+"\n", args...)
	}}
	for _, tc := range []struct {
		acct account.Account
		dir  string
	}{
		{account.Account{Name: "default", Dir: filepath.Join(root, ".claude")}, ""},
		{account.Account{Name: "work", Dir: filepath.Join(root, ".claude-work")}, filepath.Join(root, ".claude-work")},
	} {
		s, err := c.Check(context.Background(), tc.acct)
		if err != nil {
			t.Fatalf("%s: %v", tc.acct.Name, err)
		}
		if s.Account != tc.acct.Name || s.Source != FromClaudeLive || s.Plan != "max" || s.CapturedAt.Before(before) || s.CapturedAt.After(time.Now()) {
			t.Fatalf("%s: %+v", tc.acct.Name, s)
		}
		if s.FiveHour == nil || s.FiveHour.UsedPercent != 26 || s.FiveHour.ResetsAt.Format(time.RFC3339) != "2026-10-16T12:00:00Z" {
			t.Fatalf("5-hour %+v", s.FiveHour)
		}
		if len(s.Limits) != 1 || s.Limits[0].Name != "Fable weekly" || s.Extra != nil {
			t.Fatalf("limits %+v, extra %+v", s.Limits, s.Extra)
		}
		seen, _ := os.ReadFile(out)
		got := string(seen)
		real, _ := filepath.EvalSymlinks(work)
		for _, want := range []string{`"subtype":"get_usage"`, `"skip_behaviors":true`, "dir=" + tc.dir + "\n", "pane=\n",
			"--setting-sources local", "--strict-mcp-config", "--no-session-persistence"} {
			if !strings.Contains(got, want) {
				t.Fatalf("%s: tool saw %q, missing %q", tc.acct.Name, got, want)
			}
		}
		if !strings.Contains(got, work) && !strings.Contains(got, real) {
			t.Fatalf("%s: not run in the work folder: %q", tc.acct.Name, got)
		}
		traced := log.String()
		for _, want := range []string{"exec: ", "pid: ", "transport: JSON lines", "get_usage", "control_response", "stdin closed", "exit: ", "cwd: " + work} {
			if !strings.Contains(traced, want) && !strings.Contains(traced, "cwd: "+real) {
				if want == "cwd: "+work && strings.Contains(traced, "cwd: "+real) {
					continue
				}
				t.Fatalf("%s: trace missing %q\n%s", tc.acct.Name, want, traced)
			}
		}
	}
}

// A missing tool and a tool that never answers fail with a plain reason.
func TestCheckFailures(t *testing.T) {
	root, bin, _ := liveEnv(t)
	c := Checker{WorkDir: filepath.Join(root, "work"), Timeout: 300 * time.Millisecond}
	codex := account.Account{Name: "codex", Tool: account.Codex, Dir: filepath.Join(root, ".codex")}
	if _, err := c.Check(context.Background(), codex); err == nil || !strings.Contains(err.Error(), "codex is not installed") {
		t.Fatalf("missing tool: %v", err)
	}
	fakeTool(t, bin, "claude", "sleep 5\n")
	start := time.Now()
	_, err := c.Check(context.Background(), account.Account{Name: "default", Dir: filepath.Join(root, ".claude")})
	if err == nil || !strings.Contains(err.Error(), "no answer in 300ms") || time.Since(start) > 4*time.Second {
		t.Fatalf("silent tool: %v after %s", err, time.Since(start))
	}
	fakeTool(t, bin, "claude", "read line\necho 'please log in' >&2\n")
	if _, err := c.Check(context.Background(), account.Account{Name: "default", Dir: filepath.Join(root, ".claude")}); err == nil || !strings.Contains(err.Error(), "please log in") {
		t.Fatalf("tool that exits: %v", err)
	}
}

// Codex's app server is initialized, then asked for its rate limits, with
// CODEX_HOME set only for a home other than ~/.codex.
func TestCheckCodex(t *testing.T) {
	root, bin, out := liveEnv(t)
	fakeTool(t, bin, "codex", `read init
echo '{"id":1,"result":{"userAgent":"codex"}}'
read note
read req
{ printf '%s\n%s\n%s\n' "$init" "$note" "$req"; printf 'home=%s\n' "$CODEX_HOME"; printf 'args=%s\n' "$*"; } > "$FAKE_OUT"
echo '{"method":"remoteControl/status/changed","params":{}}'
echo '{"id":2,"result":{"rateLimits":{"limitId":"codex","planType":"free","primary":{"usedPercent":4,"windowDurationMins":43200,"resetsAt":1794739288},"secondary":null},"rateLimitsByLimitId":{"codex":{"limitId":"codex","planType":"free","primary":{"usedPercent":4,"windowDurationMins":43200,"resetsAt":1794739288}}}}}'
`)
	c := Checker{WorkDir: filepath.Join(root, "work"), Client: "1.2.3"}
	s, err := c.Check(context.Background(), account.Account{Name: "codex-work", Tool: account.Codex, Dir: filepath.Join(root, ".codex-work")})
	if err != nil {
		t.Fatal(err)
	}
	if s.Source != FromCodexLive || s.Plan != "free" || s.FiveHour != nil || s.SevenDay != nil || len(s.Limits) != 1 ||
		s.Limits[0].Name != "30-day" || s.Limits[0].UsedPercent != 4 || s.Limits[0].ResetsAt.Unix() != 1794739288 {
		t.Fatalf("%+v", s)
	}
	seen, _ := os.ReadFile(out)
	for _, want := range []string{`"method":"initialize"`, `"version":"1.2.3"`, `{"method":"initialized"}`,
		`"method":"account/rateLimits/read"`, "home=" + filepath.Join(root, ".codex-work") + "\n", "args=app-server\n"} {
		if !strings.Contains(string(seen), want) {
			t.Fatalf("tool saw %q, missing %q", seen, want)
		}
	}
}

func TestParseClaudeUsage(t *testing.T) {
	for _, tc := range []struct {
		name, line, err string
	}{
		{"error", `{"type":"control_response","response":{"subtype":"error","request_id":"ccli-usage","error":"get_usage is not supported in this context"}}`, "not supported"},
		{"api key", `{"type":"control_response","response":{"subtype":"success","request_id":"ccli-usage","response":{"rate_limits_available":false,"rate_limits":null}}}`, "no plan limits"},
		{"unreachable", `{"type":"control_response","response":{"subtype":"success","request_id":"ccli-usage","response":{"rate_limits_available":true,"rate_limits":null}}}`, "could not reach"},
	} {
		if _, err := parseClaudeUsage([]byte(tc.line)); err == nil || !strings.Contains(err.Error(), tc.err) {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
	// Extra usage, when it is on, is read in currency units; a per-model
	// window the server also names in model_scoped is listed once.
	line := `{"type":"control_response","response":{"subtype":"success","request_id":"ccli-usage","response":{"subscription_type":"max","rate_limits_available":true,"rate_limits":{"five_hour":{"utilization":91.5,"resets_at":null},"seven_day_sonnet":{"utilization":12,"resets_at":"2026-10-22T11:00:00Z"},"model_scoped":[{"display_name":"Sonnet","utilization":12,"resets_at":"2026-10-22T11:00:00Z"}],"extra_usage":{"is_enabled":true,"monthly_limit":5000,"used_credits":1234,"utilization":24.68,"currency":"USD","decimal_places":2}}}}}`
	s, err := parseClaudeUsage([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	if s.FiveHour.UsedPercent != 91.5 || !s.FiveHour.ResetsAt.IsZero() || s.SevenDay != nil {
		t.Fatalf("windows %+v %+v", s.FiveHour, s.SevenDay)
	}
	if len(s.Limits) != 1 || s.Limits[0].Name != "Sonnet weekly" {
		t.Fatalf("limits %+v", s.Limits)
	}
	if e := s.Extra; e == nil || e.Used != 12.34 || e.Limit != 50 || e.Currency != "USD" || e.UsedPercent != 24.68 {
		t.Fatalf("extra %+v", s.Extra)
	}
}

// Codex windows are placed by length, and other buckets are named.
func TestParseCodexLimits(t *testing.T) {
	line := `{"id":2,"result":{"rateLimits":{"limitId":"codex","planType":"pro","primary":{"usedPercent":61,"windowDurationMins":10080,"resetsAt":1794000000},"secondary":{"usedPercent":8,"windowDurationMins":300,"resetsAt":1793000000}},"rateLimitsByLimitId":{"codex":{"limitId":"codex"},"codex_bengalfox":{"limitId":"codex_bengalfox","limitName":"GPT-5.3-Codex-Spark","primary":{"usedPercent":3,"windowDurationMins":10080,"resetsAt":null}}}}}`
	s, err := parseCodexLimits([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	if s.Plan != "pro" || s.SevenDay.UsedPercent != 61 || s.FiveHour.UsedPercent != 8 || s.FiveHour.ResetsAt.Unix() != 1793000000 {
		t.Fatalf("%+v", s)
	}
	if len(s.Limits) != 1 || s.Limits[0].Name != "GPT-5.3-Codex-Spark weekly" || !s.Limits[0].ResetsAt.IsZero() {
		t.Fatalf("limits %+v", s.Limits)
	}
	if _, err := parseCodexLimits([]byte(`{"id":2,"error":{"code":-32000,"message":"not logged in"}}`)); err == nil || !strings.Contains(err.Error(), "not logged in") {
		t.Fatalf("error answer: %v", err)
	}
}

// A tool that started a child and never answers is killed with its child
// when the check times out: nothing it started outlives the check.
func TestCheckKillsWhatTheToolStarted(t *testing.T) {
	root, bin, out := liveEnv(t)
	fakeTool(t, bin, "codex", "sleep 30 &\necho $! > \"$FAKE_OUT\"\nsleep 30\n")
	c := Checker{WorkDir: filepath.Join(root, "work"), Timeout: 400 * time.Millisecond}
	_, err := c.Check(context.Background(), account.Account{Name: "codex", Tool: account.Codex, Dir: filepath.Join(root, ".codex")})
	if err == nil || !strings.Contains(err.Error(), "no answer") {
		t.Fatalf("check: %v", err)
	}
	b, _ := os.ReadFile(out)
	pid, perr := strconv.Atoi(strings.TrimSpace(string(b)))
	if perr != nil {
		t.Fatalf("child pid %q", b)
	}
	deadline := time.Now().Add(2 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("child %d outlived the check", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A field of the wrong type or shape costs only that field, and an error
// given as an object still says what it is.
func TestParseClaudeUsageIsLenient(t *testing.T) {
	line := `{"type":"control_response","response":{"subtype":"success","request_id":"ccli-usage","response":{"subscription_type":"pro","rate_limits_available":true,"rate_limits":{"five_hour":{"utilization":40,"resets_at":"2026-10-16T12:00:00Z"},"seven_day":{"utilization":"23%","resets_at":"2026-10-22T11:00:00Z"},"model_scoped":{"oops":true},"extra_usage":"n/a"}}}}`
	s, err := parseClaudeUsage([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	if s.FiveHour == nil || s.FiveHour.UsedPercent != 40 || s.SevenDay != nil || len(s.Limits) != 0 || s.Extra != nil || s.Plan != "pro" {
		t.Fatalf("%+v", s)
	}
	unnamed := `{"type":"control_response","response":{"subtype":"success","request_id":"ccli-usage","response":{"rate_limits_available":true,"rate_limits":{"model_scoped":[{"utilization":5,"resets_at":null},{"display_name":"Fable","utilization":"x"}]}}}}`
	if s, err := parseClaudeUsage([]byte(unnamed)); err != nil || len(s.Limits) != 1 || s.Limits[0].Name != "Model weekly" {
		t.Fatalf("unnamed model window: %v %+v", err, s.Limits)
	}
	obj := `{"type":"control_response","response":{"subtype":"error","request_id":"ccli-usage","error":{"message":"busy \u001b[31mnow"}}}`
	if _, err := parseClaudeUsage([]byte(obj)); err == nil || !strings.Contains(err.Error(), "busy") || strings.Contains(err.Error(), "\x1b") {
		t.Fatalf("object error: %v", err)
	}
}

// Shapes Codex might send that must not show wrong figures as live.
func TestParseCodexLimitsEdges(t *testing.T) {
	for _, tc := range []struct {
		name, line string
		check      func(Snapshot) bool
	}{
		{"missing percent is left out, not 0%",
			`{"id":2,"result":{"rateLimits":{"limitId":"codex","primary":{"windowDurationMins":300,"resetsAt":1},"secondary":{"usedPercent":7,"windowDurationMins":10080}}}}`,
			func(s Snapshot) bool { return s.FiveHour == nil && s.SevenDay != nil && s.SevenDay.UsedPercent == 7 }},
		{"null main id does not repeat the main windows",
			`{"id":2,"result":{"rateLimits":{"limitId":null,"primary":{"usedPercent":9,"windowDurationMins":300}},"rateLimitsByLimitId":{"codex":{"limitId":"codex","primary":{"usedPercent":9,"windowDurationMins":300}}}}}`,
			func(s Snapshot) bool { return s.FiveHour != nil && len(s.Limits) == 0 }},
		{"null main falls back to the codex bucket",
			`{"id":2,"result":{"rateLimits":null,"rateLimitsByLimitId":{"codex":{"limitId":"codex","planType":"plus","primary":{"usedPercent":12,"windowDurationMins":10080}}}}}`,
			func(s Snapshot) bool {
				return s.SevenDay != nil && s.SevenDay.UsedPercent == 12 && s.Plan == "plus" && len(s.Limits) == 0
			}},
		{"a window without a length says so",
			`{"id":2,"result":{"rateLimits":{"limitId":"codex","primary":{"usedPercent":3}}}}`,
			func(s Snapshot) bool { return len(s.Limits) == 1 && s.Limits[0].Name == "window of unknown length" }},
	} {
		s, err := parseCodexLimits([]byte(tc.line))
		if err != nil || !tc.check(s) {
			t.Errorf("%s: %v %+v", tc.name, err, s)
		}
	}
}
