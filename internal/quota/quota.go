// Package quota records Claude Code subscription limits from the status line
// payload and runs the user's status line command unchanged.
package quota

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/lr00rl/ccli/internal/account"
)

// maxParse is the most of stdin Wrap will try to interpret. A larger payload
// is given to the wrapped command whole and is not parsed.
const maxParse = 1 << 20

// Window is one rate-limit window from a status line snapshot.
type Window struct {
	UsedPercent float64   `json:"used_percent"`
	ResetsAt    time.Time `json:"resets_at,omitzero"` // zero when the source did not say
}

// Expired reports whether the window has reset since the snapshot; never when
// the reset time is unknown.
func (w Window) Expired(now time.Time) bool {
	return !w.ResetsAt.IsZero() && !now.Before(w.ResetsAt)
}

// Unix is the time of a reset given in unix seconds; zero, for unknown, when
// the source left it out.
func Unix(sec int64) time.Time {
	if sec <= 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0).UTC()
}

// Snapshot is the rate-limit state saved for one account.
type Snapshot struct {
	Account    string    `json:"account"`
	ConfigDir  string    `json:"config_dir"`
	SessionID  string    `json:"session_id,omitempty"`
	Model      string    `json:"model,omitempty"`
	CapturedAt time.Time `json:"captured_at"`
	FiveHour   *Window   `json:"five_hour,omitempty"`
	SevenDay   *Window   `json:"seven_day,omitempty"`
	// Source is where the reading came from: empty for the status line
	// wrapper, FromCodexLog for the rate limits Codex writes to its logs,
	// FromClaudeLive or FromCodexLive for a live check.
	Source string `json:"source,omitempty"`
	Plan   string `json:"plan,omitempty"` // the subscription plan, when the source names it
	// Limits and Extra come only from a live check (see Checker).
	Limits []Limit `json:"limits,omitempty"`
	Extra  *Extra  `json:"extra_usage,omitempty"`
}

// FromCodexLog marks a snapshot read from Codex's session logs.
const FromCodexLog = "codex-log"

// FromCcline marks a snapshot read from ccline's usage cache.
const FromCcline = "ccline"

type statusLine struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Model          struct {
		ID string `json:"id"`
	} `json:"model"`
	RateLimits *struct {
		FiveHour *rateWindow `json:"five_hour"`
		SevenDay *rateWindow `json:"seven_day"`
	} `json:"rate_limits"`
}

type rateWindow struct {
	UsedPercentage float64 `json:"used_percentage"`
	ResetsAt       int64   `json:"resets_at"`
}

func (r rateWindow) window() Window {
	return Window{
		UsedPercent: r.UsedPercentage,
		ResetsAt:    Unix(r.ResetsAt),
	}
}

// Parse reads the status line JSON. ok is false when it has no rate_limits
// window (nothing to record).
func Parse(input []byte, now time.Time) (s Snapshot, transcriptPath string, ok bool, err error) {
	var raw statusLine
	if err := json.Unmarshal(input, &raw); err != nil {
		return Snapshot{}, "", false, err
	}
	s = Snapshot{
		SessionID:  raw.SessionID,
		Model:      raw.Model.ID,
		CapturedAt: now,
	}
	if raw.RateLimits != nil {
		if raw.RateLimits.FiveHour != nil {
			w := raw.RateLimits.FiveHour.window()
			s.FiveHour = &w
		}
		if raw.RateLimits.SevenDay != nil {
			w := raw.RateLimits.SevenDay.window()
			s.SevenDay = &w
		}
	}
	ok = s.FiveHour != nil || s.SevenDay != nil
	return s, raw.TranscriptPath, ok, nil
}

// AccountFor maps a transcript path to the account whose Dir contains it
// (path is inside a.Dir + "/projects/"). When no account matches, it falls
// back to the CLAUDE_CONFIG_DIR value given (empty means ~/.claude, the
// account named "default"), matching by cleaned Dir. Returns false if none.
func AccountFor(transcriptPath, configDirEnv string, accts []account.Account) (account.Account, bool) {
	var best account.Account
	bestLen := -1
	for _, a := range accts {
		if !inProjects(a.Dir, transcriptPath) {
			continue
		}
		n := len(filepath.Clean(a.Dir))
		if n > bestLen {
			best = a
			bestLen = n
		}
	}
	if bestLen >= 0 {
		return best, true
	}
	want, ok := fallbackDir(configDirEnv)
	if !ok {
		return account.Account{}, false
	}
	for _, a := range accts {
		if filepath.Clean(a.Dir) == want {
			return a, true
		}
	}
	return account.Account{}, false
}

// inProjects reports whether path sits inside dir/projects. Rel, not a string
// prefix, so a sibling such as "<dir>-old/projects" is not treated as inside dir.
func inProjects(dir, path string) bool {
	root := filepath.Join(filepath.Clean(dir), "projects")
	rel, err := filepath.Rel(root, filepath.Clean(path))
	if err != nil || rel == "." || rel == "" {
		return false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return !filepath.IsAbs(rel)
}

func fallbackDir(configDirEnv string) (string, bool) {
	if configDirEnv != "" {
		return filepath.Clean(configDirEnv), true
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false
	}
	return filepath.Join(home, ".claude"), true
}

// Save writes the snapshot atomically (temp file + rename, mode 0600) to
// dataDir/quota/<account>.json, creating the folder with mode 0700. An
// existing snapshot with a later CapturedAt is kept (no overwrite with older data).
func Save(dataDir string, s Snapshot) error { return saveIn(filepath.Join(dataDir, "quota"), s) }

// SaveLive saves a live check's answer in dataDir/quota/live/<account>.json,
// apart from the passive readings, so a status line write cannot replace the
// windows and plan only a live check reports.
func SaveLive(dataDir string, s Snapshot) error {
	return saveIn(filepath.Join(dataDir, "quota", "live"), s)
}

func saveIn(dir string, s Snapshot) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, s.Account+".json")
	keep, err := newerSaved(path, s.CapturedAt)
	if err != nil || keep {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writeAtomic(path, data)
}

// newerSaved reports whether path already holds a snapshot taken after captured.
// A missing file, or a file that is not a snapshot, is not newer.
func newerSaved(path string, captured time.Time) (bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	var cur Snapshot
	if json.Unmarshal(b, &cur) != nil {
		return false, nil
	}
	return cur.CapturedAt.After(captured), nil
}

func writeAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".quota-*")
	if err != nil {
		return err
	}
	name := f.Name()
	done := false
	defer func() {
		if !done {
			os.Remove(name)
		}
	}()
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	done = true
	return nil
}

// Load returns the saved snapshots by account name. A missing folder is not
// an error. Unreadable or invalid files are skipped.
func Load(dataDir string) (map[string]Snapshot, error) {
	return loadDir(filepath.Join(dataDir, "quota"))
}

// LoadLive returns the saved live check answers by account name.
func LoadLive(dataDir string) (map[string]Snapshot, error) {
	return loadDir(filepath.Join(dataDir, "quota", "live"))
}

func loadDir(dir string) (map[string]Snapshot, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]Snapshot{}, nil
		}
		return nil, err
	}
	out := make(map[string]Snapshot, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		var s Snapshot
		if json.Unmarshal(b, &s) != nil {
			continue
		}
		key := s.Account
		if key == "" {
			key = strings.TrimSuffix(name, ".json")
		}
		out[key] = s
	}
	return out, nil
}

// Wrap is the body of `ccli statusline -- <command> [args]`. It reads stdin
// (at most 1 MiB; more is passed through but not parsed), records a snapshot
// when there is one (all errors ignored silently), then runs args[0] with
// args[1:], giving it exactly the bytes read as stdin and connecting its
// stdout and stderr to the given writers, and returns its exit code. With no
// args it prints nothing and returns 0. If the command cannot be started it
// writes one short line to stderr and returns 127.
func Wrap(args []string, stdin io.Reader, stdout, stderr io.Writer, dataDir string, accts []account.Account, configDirEnv string, now time.Time) int {
	if len(args) == 0 {
		return 0
	}
	head, replay, parse := readStatusInput(stdin)
	if parse {
		record(head, dataDir, accts, configDirEnv, now)
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin = replay
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	if err == nil {
		return 0
	}
	if cmd.ProcessState != nil {
		return cmd.ProcessState.ExitCode()
	}
	if stderr != nil {
		fmt.Fprintln(stderr, "ccli:", err)
	}
	return 127
}

func record(input []byte, dataDir string, accts []account.Account, configDirEnv string, now time.Time) {
	// Recording is a side effect of the user's status line; nothing in it may
	// take the status line down.
	defer func() { _ = recover() }()
	s, transcript, ok, err := Parse(input, now)
	if err != nil || !ok {
		return
	}
	a, found := AccountFor(transcript, configDirEnv, accts)
	if !found {
		return
	}
	s.Account = a.Name
	s.ConfigDir = a.Dir
	_ = Save(dataDir, s)
}

// readStatusInput splits stdin into the bytes Wrap may parse and the reader
// handed to the wrapped command. The command always sees the full input.
// parse is false once the input exceeds maxParse.
func readStatusInput(r io.Reader) (head []byte, replay io.Reader, parse bool) {
	if r == nil {
		return nil, bytes.NewReader(nil), true
	}
	buf := make([]byte, maxParse+1)
	n := 0
	var err error
	for n < len(buf) && err == nil {
		var nn int
		nn, err = r.Read(buf[n:])
		n += nn
		if nn == 0 && err == nil {
			break
		}
	}
	head = buf[:n]
	if n > maxParse {
		return head, io.MultiReader(bytes.NewReader(head), r), false
	}
	return head, bytes.NewReader(head), true
}

// Reading is one window's usage as last reported, from whichever source
// reported it most recently.
type Reading struct {
	UsedPercent float64   `json:"used_percent"`
	ResetsAt    time.Time `json:"resets_at,omitempty"` // zero when the source did not say
	At          time.Time `json:"reported_at"`
	Source      string    `json:"source"` // "Claude Code" or "Codex" (live), "status line", "ccline" or "Codex's logs"
}

// Expired reports whether the window has reset since the reading.
func (r Reading) Expired(now time.Time) bool { return !r.ResetsAt.IsZero() && !now.Before(r.ResetsAt) }

// CclineCache reads the usage cache that ccline (a status line program,
// https://github.com/Haleclipse/CCometixLine) keeps in an account's config
// dir: <dir>/ccline/.api_usage_cache.json. It holds the 5-hour and weekly
// percentages from Claude's usage API and the weekly reset time; ccli reads
// only those numbers. ok is false when there is no such file.
func CclineCache(dir string) (Snapshot, bool) {
	b, err := os.ReadFile(filepath.Join(dir, "ccline", ".api_usage_cache.json"))
	if err != nil {
		return Snapshot{}, false
	}
	var v struct {
		Five     *float64  `json:"five_hour_utilization"`
		Seven    *float64  `json:"seven_day_utilization"`
		ResetsAt time.Time `json:"resets_at"` // the weekly window's
		CachedAt time.Time `json:"cached_at"`
	}
	if json.Unmarshal(b, &v) != nil || v.CachedAt.IsZero() {
		return Snapshot{}, false
	}
	s := Snapshot{CapturedAt: v.CachedAt, Source: FromCcline}
	if v.Five != nil {
		s.FiveHour = &Window{UsedPercent: *v.Five}
	}
	if v.Seven != nil {
		s.SevenDay = &Window{UsedPercent: *v.Seven, ResetsAt: v.ResetsAt}
	}
	return s, s.FiveHour != nil || s.SevenDay != nil
}

// SourceName is how a snapshot's Source reads in output.
func SourceName(source string) string {
	switch source {
	case FromCodexLog:
		return "Codex's logs"
	case FromClaudeLive:
		return "Claude Code"
	case FromCodexLive:
		return "Codex"
	case FromCcline:
		return "ccline"
	}
	return "status line"
}

// Latest picks, for each window, the most recent reading among the
// snapshots given (nil ones are skipped). A reading without a reset time
// borrows the newest other reading's when that is still ahead of it.
func Latest(snaps ...*Snapshot) (five, seven *Reading) {
	pick := func(get func(*Snapshot) *Window) *Reading {
		var all []*Reading
		for _, s := range snaps {
			if s == nil {
				continue
			}
			if w := get(s); w != nil {
				all = append(all, &Reading{UsedPercent: w.UsedPercent, ResetsAt: w.ResetsAt, At: s.CapturedAt, Source: SourceName(s.Source)})
			}
		}
		var out *Reading
		for _, r := range all {
			if out == nil || r.At.After(out.At) {
				out = r
			}
		}
		if out != nil && out.ResetsAt.IsZero() {
			var from *Reading
			for _, r := range all {
				if r != out && r.ResetsAt.After(out.At) && (from == nil || r.At.After(from.At)) {
					from = r
				}
			}
			if from != nil {
				out.ResetsAt = from.ResetsAt
			}
		}
		return out
	}
	five = pick(func(s *Snapshot) *Window { return s.FiveHour })
	seven = pick(func(s *Snapshot) *Window { return s.SevenDay })
	return five, seven
}
