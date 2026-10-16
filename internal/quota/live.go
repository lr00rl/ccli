package quota

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lr00rl/ccli/internal/account"
)

// Live readings come from the account's own tool, asked at the moment:
// Claude Code answers its SDK control request get_usage (the data behind
// /usage), Codex its app server's account/rateLimits/read. ccli never reads
// credentials; the tool uses its own login, as when it runs. Starting it
// does what any start does to its own files (Claude Code refreshes caches
// in .claude.json, Codex touches its databases); no prompt is sent, no
// transcript is written, and the user's hooks and MCP servers are not loaded.
const (
	FromClaudeLive = "claude-live"
	FromCodexLive  = "codex-live"
)

// Limit is a window a source reports besides the 5-hour and weekly ones: a
// per-model weekly limit, or a window of another length.
type Limit struct {
	Name string `json:"name"` // "Fable weekly", "30-day"
	Window
}

// Extra is pay-as-you-go use past the plan's limits, when it is turned on.
type Extra struct {
	UsedPercent float64 `json:"used_percent"`
	Used        float64 `json:"used,omitempty"`  // in Currency, when the source says
	Limit       float64 `json:"limit,omitempty"` // the monthly cap, likewise
	Currency    string  `json:"currency,omitempty"`
}

// Checker asks tools for an account's limits.
type Checker struct {
	WorkDir string        // an empty folder of ccli's own to run the tools in
	Client  string        // ccli's version, given to Codex's app server
	Timeout time.Duration // per account; 0 means 30s
}

// Check asks the account's tool for its limits now. The snapshot carries
// the account, its dir and Source FromClaudeLive or FromCodexLive.
func (c Checker) Check(ctx context.Context, a account.Account, now time.Time) (Snapshot, error) {
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := os.MkdirAll(c.WorkDir, 0o700); err != nil {
		return Snapshot{}, err
	}
	var s Snapshot
	var err error
	if a.Kind() == account.Codex {
		s, err = c.codex(ctx, a)
	} else {
		s, err = c.claude(ctx, a)
	}
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			err = fmt.Errorf("no answer in %s", timeout)
		}
		return Snapshot{}, err
	}
	s.Account, s.ConfigDir, s.CapturedAt = a.Name, a.Dir, now
	return s, nil
}

const usageRequest = "ccli-usage"

func (c Checker) claude(ctx context.Context, a account.Account) (Snapshot, error) {
	// --setting-sources local in an empty folder leaves out the user's
	// settings (hooks, plugins, env); --strict-mcp-config with no config
	// starts no MCP server; no prompt is ever sent.
	p, err := start(ctx, c.WorkDir, toolEnv(a), "claude", "-p",
		"--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--setting-sources", "local", "--strict-mcp-config", "--no-session-persistence")
	if err != nil {
		return Snapshot{}, err
	}
	defer p.stop()
	if err := p.send(map[string]any{"type": "control_request", "request_id": usageRequest,
		"request": map[string]any{"subtype": "get_usage", "skip_behaviors": true}}); err != nil {
		return Snapshot{}, p.fail(err)
	}
	line, err := p.await(func(m map[string]json.RawMessage) bool {
		var r struct {
			RequestID string `json:"request_id"`
		}
		return string(m["type"]) == `"control_response"` && json.Unmarshal(m["response"], &r) == nil && r.RequestID == usageRequest
	})
	if err != nil {
		return Snapshot{}, p.fail(err)
	}
	return parseClaudeUsage(line)
}

type claudeWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    *string  `json:"resets_at"`
}

func (w *claudeWindow) window() *Window {
	if w == nil || w.Utilization == nil {
		return nil
	}
	out := &Window{UsedPercent: *w.Utilization}
	if w.ResetsAt != nil {
		if t, err := time.Parse(time.RFC3339Nano, *w.ResetsAt); err == nil {
			out.ResetsAt = t.UTC()
		}
	}
	return out
}

// parseClaudeUsage reads Claude Code's answer to get_usage.
func parseClaudeUsage(line []byte) (Snapshot, error) {
	var v struct {
		Response struct {
			Subtype  string `json:"subtype"`
			Error    string `json:"error"`
			Response struct {
				Subscription *string `json:"subscription_type"`
				Available    bool    `json:"rate_limits_available"`
				Limits       *struct {
					FiveHour *claudeWindow `json:"five_hour"`
					SevenDay *claudeWindow `json:"seven_day"`
					Opus     *claudeWindow `json:"seven_day_opus"`
					Sonnet   *claudeWindow `json:"seven_day_sonnet"`
					Models   []struct {
						Name string `json:"display_name"`
						claudeWindow
					} `json:"model_scoped"`
					Extra *struct {
						Enabled     bool     `json:"is_enabled"`
						Utilization *float64 `json:"utilization"`
						Used        *float64 `json:"used_credits"`
						Limit       *float64 `json:"monthly_limit"`
						Currency    *string  `json:"currency"`
						Places      *int     `json:"decimal_places"`
					} `json:"extra_usage"`
				} `json:"rate_limits"`
			} `json:"response"`
		} `json:"response"`
	}
	if err := json.Unmarshal(line, &v); err != nil {
		return Snapshot{}, fmt.Errorf("Claude Code's answer: %w", err)
	}
	r := v.Response
	switch {
	case r.Subtype != "success":
		if r.Error == "" {
			r.Error = "no reason given"
		}
		return Snapshot{}, fmt.Errorf("Claude Code: %s", r.Error)
	case !r.Response.Available:
		return Snapshot{}, errors.New("this login has no plan limits (an API key or a cloud provider)")
	case r.Response.Limits == nil:
		return Snapshot{}, errors.New("Claude Code could not reach the usage service")
	}
	l := r.Response.Limits
	s := Snapshot{Source: FromClaudeLive, FiveHour: l.FiveHour.window(), SevenDay: l.SevenDay.window()}
	if r.Response.Subscription != nil {
		s.Plan = *r.Response.Subscription
	}
	add := func(name string, w *claudeWindow) {
		win := w.window()
		if win == nil {
			return
		}
		for _, have := range s.Limits {
			if strings.EqualFold(have.Name, name) {
				return
			}
		}
		s.Limits = append(s.Limits, Limit{Name: name, Window: *win})
	}
	add("Opus weekly", l.Opus)
	add("Sonnet weekly", l.Sonnet)
	for _, m := range l.Models {
		add(m.Name+" weekly", &m.claudeWindow)
	}
	if e := l.Extra; e != nil && e.Enabled && e.Utilization != nil {
		s.Extra = &Extra{UsedPercent: *e.Utilization}
		if e.Places != nil && e.Used != nil && e.Limit != nil {
			scale := math.Pow10(*e.Places)
			s.Extra.Used, s.Extra.Limit = *e.Used/scale, *e.Limit/scale
		}
		if e.Currency != nil {
			s.Extra.Currency = *e.Currency
		}
	}
	if s.FiveHour == nil && s.SevenDay == nil && len(s.Limits) == 0 {
		return Snapshot{}, errors.New("Claude Code reported no limit windows")
	}
	return s, nil
}

func (c Checker) codex(ctx context.Context, a account.Account) (Snapshot, error) {
	p, err := start(ctx, c.WorkDir, toolEnv(a), "codex", "app-server")
	if err != nil {
		return Snapshot{}, err
	}
	defer p.stop()
	reply := func(id int) func(map[string]json.RawMessage) bool {
		want := fmt.Sprint(id)
		return func(m map[string]json.RawMessage) bool { return string(m["id"]) == want }
	}
	client := c.Client
	if client == "" {
		client = "dev"
	}
	if err := p.send(map[string]any{"id": 1, "method": "initialize",
		"params": map[string]any{"clientInfo": map[string]any{"name": "ccli", "version": client}}}); err != nil {
		return Snapshot{}, p.fail(err)
	}
	if line, err := p.await(reply(1)); err != nil {
		return Snapshot{}, p.fail(err)
	} else if err := rpcError(line); err != nil {
		return Snapshot{}, err
	}
	if err := p.send(map[string]any{"method": "initialized"}); err != nil {
		return Snapshot{}, p.fail(err)
	}
	if err := p.send(map[string]any{"id": 2, "method": "account/rateLimits/read"}); err != nil {
		return Snapshot{}, p.fail(err)
	}
	line, err := p.await(reply(2))
	if err != nil {
		return Snapshot{}, p.fail(err)
	}
	return parseCodexLimits(line)
}

func rpcError(line []byte) error {
	var v struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(line, &v) == nil && v.Error != nil {
		return fmt.Errorf("Codex: %s", v.Error.Message)
	}
	return nil
}

type codexWindow struct {
	UsedPercent float64 `json:"usedPercent"`
	Minutes     *int64  `json:"windowDurationMins"`
	ResetsAt    *int64  `json:"resetsAt"`
}

type codexBucket struct {
	LimitID   string       `json:"limitId"`
	LimitName *string      `json:"limitName"`
	Plan      *string      `json:"planType"`
	Primary   *codexWindow `json:"primary"`
	Secondary *codexWindow `json:"secondary"`
}

// parseCodexLimits reads the app server's answer to account/rateLimits/read.
// Windows are told apart by their length, not by primary and secondary,
// whose meaning has changed: 300 minutes is the 5-hour window and 10080
// the weekly one.
func parseCodexLimits(line []byte) (Snapshot, error) {
	if err := rpcError(line); err != nil {
		return Snapshot{}, err
	}
	var v struct {
		Result *struct {
			Main    *codexBucket           `json:"rateLimits"`
			Buckets map[string]codexBucket `json:"rateLimitsByLimitId"`
		} `json:"result"`
	}
	if err := json.Unmarshal(line, &v); err != nil {
		return Snapshot{}, fmt.Errorf("Codex's answer: %w", err)
	}
	if v.Result == nil || v.Result.Main == nil {
		return Snapshot{}, errors.New("Codex reported no rate limits")
	}
	main := *v.Result.Main
	s := Snapshot{Source: FromCodexLive}
	if main.Plan != nil {
		s.Plan = *main.Plan
	}
	put := func(prefix string, w *codexWindow) {
		if w == nil {
			return
		}
		win := Window{UsedPercent: w.UsedPercent}
		if w.ResetsAt != nil {
			win.ResetsAt = Unix(*w.ResetsAt)
		}
		var mins int64
		if w.Minutes != nil {
			mins = *w.Minutes
		}
		switch {
		case prefix == "" && mins == 300 && s.FiveHour == nil:
			s.FiveHour = &win
		case prefix == "" && mins == 10080 && s.SevenDay == nil:
			s.SevenDay = &win
		default:
			s.Limits = append(s.Limits, Limit{Name: strings.TrimSpace(prefix + " " + windowName(mins)), Window: win})
		}
	}
	put("", main.Primary)
	put("", main.Secondary)
	ids := make([]string, 0, len(v.Result.Buckets))
	for id := range v.Result.Buckets {
		if id != main.LimitID {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		b := v.Result.Buckets[id]
		name := id
		if b.LimitName != nil && *b.LimitName != "" {
			name = *b.LimitName
		}
		put(name, b.Primary)
		put(name, b.Secondary)
	}
	if s.FiveHour == nil && s.SevenDay == nil && len(s.Limits) == 0 {
		return Snapshot{}, errors.New("Codex reported no limit windows")
	}
	return s, nil
}

// windowName names a window by its length in minutes.
func windowName(mins int64) string {
	switch {
	case mins == 300:
		return "5-hour"
	case mins == 10080:
		return "weekly"
	case mins > 0 && mins%1440 == 0:
		return fmt.Sprintf("%d-day", mins/1440)
	case mins > 0 && mins%60 == 0:
		return fmt.Sprintf("%d-hour", mins/60)
	case mins > 0:
		return fmt.Sprintf("%d-minute", mins)
	}
	return "window"
}

// toolEnv is ccli's environment for the account's tool, pointed at the
// account's dir. tmux's pane variables and Claude Code's session markers are
// left out, so the run is not taken for a nested run inside a pane or a
// session; everything else, the tool's login included, is as when you run it.
func toolEnv(a account.Account) []string {
	drop := map[string]bool{
		"TMUX": true, "TMUX_PANE": true, "CLAUDECODE": true, "CLAUDE_CODE_ENTRYPOINT": true,
		"CLAUDE_CONFIG_DIR": true, "CODEX_HOME": true,
	}
	var env []string
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); !drop[k] {
			env = append(env, kv)
		}
	}
	if name, value := a.SelectEnv(); value != "" {
		env = append(env, name+"="+value)
	}
	return env
}

// proc is a tool started with JSON lines on stdin and stdout.
type proc struct {
	name   string
	cmd    *exec.Cmd
	in     io.WriteCloser
	out    *bufio.Scanner
	stderr *tailBuffer
	once   sync.Once
	done   chan struct{} // closed once Wait has returned
}

func start(ctx context.Context, dir string, env []string, name string, args ...string) (*proc, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return nil, fmt.Errorf("%s is not installed (not found on PATH)", name)
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Dir, cmd.Env = dir, env
	cmd.WaitDelay = 2 * time.Second
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	p := &proc{name: name, cmd: cmd, in: in, out: bufio.NewScanner(out), stderr: &tailBuffer{max: 400}}
	p.out.Buffer(make([]byte, 64<<10), 16<<20)
	cmd.Stderr = p.stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *proc) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = p.in.Write(append(b, '\n'))
	return err
}

// await reads lines until one matches; lines that are not JSON objects are
// skipped.
func (p *proc) await(match func(map[string]json.RawMessage) bool) ([]byte, error) {
	for p.out.Scan() {
		line := p.out.Bytes()
		var m map[string]json.RawMessage
		if json.Unmarshal(line, &m) == nil && match(m) {
			return bytes.Clone(line), nil
		}
	}
	if err := p.out.Err(); err != nil {
		return nil, err
	}
	return nil, io.ErrUnexpectedEOF
}

// fail turns a broken exchange into an error that says what the tool said.
func (p *proc) fail(err error) error {
	// exec copies stderr from its own goroutine and is done only when Wait
	// returns, so wait for the tool before reading what it said.
	p.wait(2 * time.Second)
	if msg := strings.TrimSpace(p.stderr.String()); msg != "" {
		return fmt.Errorf("%s stopped: %s", p.name, msg)
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("%s exited without answering", p.name)
	}
	return fmt.Errorf("%s: %w", p.name, err)
}

// stop ends the exchange: it closes stdin, which ends both tools, and waits.
func (p *proc) stop() { p.wait(3 * time.Second) }

// wait closes stdin and waits for the tool to exit, killing it after d. It
// is called only once reading stdout is over, as exec requires, and any
// number of times.
func (p *proc) wait(d time.Duration) {
	p.once.Do(func() {
		p.in.Close()
		p.done = make(chan struct{})
		go func() { p.cmd.Wait(); close(p.done) }()
	})
	select {
	case <-p.done:
	case <-time.After(d):
		p.cmd.Process.Kill()
		<-p.done
	}
}

// tailBuffer keeps the last max bytes written to it. exec copies the
// tool's stderr into it from its own goroutine.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (t *tailBuffer) Write(b []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, b...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = t.buf[over:]
	}
	return len(b), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
