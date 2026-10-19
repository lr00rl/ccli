// Package account finds the local Claude Code accounts. An account is one
// config dir: ~/.claude ("default") or ~/.claude-<name> ("<name>"). It reads
// only the identity label from .claude.json and the live-process registry; it
// never touches credentials.
package account

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

// Tool is the coding agent whose sessions an account holds.
type Tool string

const (
	Claude    Tool = "claude"     // a Claude Code config dir: projects/<key>/<session>.jsonl
	Codex     Tool = "codex"      // a Codex home: sessions/YYYY/MM/DD/rollout-*.jsonl
	Pi        Tool = "pi"         // a Pi agent dir: sessions/--<cwd>--/<time>_<id>.jsonl
	Cursor    Tool = "cursor"     // the Cursor CLI: ~/.cursor/projects/*/agent-transcripts
	CursorIDE Tool = "cursor-ide" // the Cursor IDE: User/globalStorage/state.vscdb
)

// Label is the short name shown next to an account that has no email.
func (t Tool) Label() string {
	switch t {
	case Codex:
		return "Codex"
	case Pi:
		return "Pi"
	case Cursor:
		return "Cursor CLI"
	case CursorIDE:
		return "Cursor IDE"
	default:
		return "Claude Code"
	}
}

// Account is one Claude Code config dir or one Codex home.
type Account struct {
	Name    string `json:"name"`
	Tool    Tool   `json:"tool"`
	Dir     string `json:"dir"`
	Email   string `json:"email,omitempty"`
	Org     string `json:"org,omitempty"`
	Billing string `json:"billing,omitempty"`
}

// Kind is the account's tool; an account made without one is a Claude Code
// config dir.
func (a Account) Kind() Tool {
	if a.Tool == "" {
		return Claude
	}
	return a.Tool
}

// SelectEnv names the environment variable that points the account's tool
// at its dir, and the value to give it. The value is empty for the tool's
// default dir (~/.claude, ~/.codex), where the variable must stay unset:
// Claude Code keys its login on whether CLAUDE_CONFIG_DIR is set at all.
func (a Account) SelectEnv() (name, value string) {
	var def string
	switch a.Kind() {
	case Codex:
		name, def = "CODEX_HOME", ".codex"
	case Pi:
		name, def = "PI_CODING_AGENT_DIR", filepath.Join(".pi", "agent")
	case Cursor, CursorIDE:
		// The CLI resumes with `agent --resume` and does not take a home
		// variable. The IDE has no resume command that reads this database.
		return "", ""
	default:
		name, def = "CLAUDE_CONFIG_DIR", ".claude"
	}
	if home, err := os.UserHomeDir(); err != nil || filepath.Clean(a.Dir) != filepath.Join(home, def) {
		value = a.Dir
	}
	return name, value
}

// Live is a running Claude Code process, from <dir>/sessions/<pid>.json.
type Live struct {
	Account   string `json:"account"`
	PID       int    `json:"pid"`
	SessionID string `json:"session_id"`
	Status    string `json:"status,omitempty"`
	Cwd       string `json:"cwd,omitempty"`
}

// NameOf maps a config dir to its account name.
func NameOf(dir string) string {
	b := filepath.Base(filepath.Clean(dir))
	switch {
	case b == ".claude":
		return "default"
	case strings.HasPrefix(b, ".claude-"):
		return strings.TrimPrefix(b, ".claude-")
	}
	return b
}

// Discover lists the accounts: Claude Code config dirs (~/.claude and
// ~/.claude-*, with a projects/ folder), Codex homes (~/.codex and
// ~/.codex-*, with a sessions/ folder), Pi agent dirs (~/.pi/agent, with a
// sessions/ folder), the Cursor CLI home (~/.cursor, with projects/ or
// chats/), and the Cursor IDE user dir (the one that holds
// globalStorage/state.vscdb). CCLI_CONFIG_DIRS, CCLI_CODEX_DIRS,
// CCLI_PI_DIRS, CCLI_CURSOR_DIRS and CCLI_CURSOR_IDE_DIRS replace each search.
func Discover() ([]Account, error) { return discover(true) }

// DiscoverDirs is Discover without reading identities, for hot paths such as
// the status line wrapper.
func DiscoverDirs() ([]Account, error) { return discover(false) }

func discover(withIdentity bool) ([]Account, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	var out []Account
	names := map[string]bool{}
	add := func(a Account) {
		// Names identify accounts on the command line, so they must be unique.
		if names[a.Name] {
			a.Name += "-" + string(a.Tool)
		}
		names[a.Name] = true
		out = append(out, a)
	}
	for _, d := range dirsOf("CCLI_CONFIG_DIRS", filepath.Join(home, ".claude"), "projects") {
		a := Account{Name: NameOf(d), Tool: Claude, Dir: d}
		if withIdentity {
			a.Email, a.Org, a.Billing = identity(d, home)
		}
		add(a)
	}
	// Codex keeps the signed-in identity only in auth.json, next to its
	// credentials, so a Codex account has no email here.
	for _, d := range dirsOf("CCLI_CODEX_DIRS", filepath.Join(home, ".codex"), "sessions") {
		add(Account{Name: strings.TrimPrefix(filepath.Base(d), "."), Tool: Codex, Dir: d})
	}
	for _, d := range piDirs(home) {
		add(Account{Name: piName(d, home), Tool: Pi, Dir: d})
	}
	for _, d := range cursorDirs(home) {
		add(Account{Name: cursorName(d, home), Tool: Cursor, Dir: d})
	}
	for _, d := range cursorIDEDirs(home) {
		add(Account{Name: cursorIDEName(d, home), Tool: CursorIDE, Dir: d})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Tool != out[j].Tool {
			return toolRank(out[i].Tool) < toolRank(out[j].Tool)
		}
		if (out[i].Name == "default") != (out[j].Name == "default") {
			return out[i].Name == "default"
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// dirsOf lists the dirs named in the environment variable env (separated by
// the OS path list separator), or else base and its siblings base-*, keeping
// those that hold the folder need.
func dirsOf(env, base, need string) []string {
	var dirs []string
	if v := os.Getenv(env); v != "" {
		dirs = filepath.SplitList(v)
	} else {
		dirs = append(dirs, base)
		more, _ := filepath.Glob(base + "-*")
		dirs = append(dirs, more...)
	}
	seen := map[string]bool{}
	var out []string
	for _, d := range dirs {
		d = filepath.Clean(d)
		if seen[d] {
			continue
		}
		seen[d] = true
		if st, err := os.Stat(filepath.Join(d, need)); err == nil && st.IsDir() {
			out = append(out, d)
		}
	}
	return out
}

// Find returns the account with this name.
func Find(accts []Account, name string) (Account, error) {
	for _, a := range accts {
		if a.Name == name {
			return a, nil
		}
	}
	names := make([]string, 0, len(accts))
	for _, a := range accts {
		names = append(names, a.Name)
	}
	return Account{}, errors.New("no account named " + strconv.Quote(name) + " (have: " + strings.Join(names, ", ") + ")")
}

// identity reads the display label of the signed-in account. Claude Code keeps
// .claude.json inside the config dir when CLAUDE_CONFIG_DIR is set, and at
// ~/.claude.json for the default dir.
func identity(dir, home string) (email, org, billing string) {
	paths := []string{filepath.Join(dir, ".claude.json")}
	if filepath.Clean(dir) == filepath.Join(home, ".claude") {
		paths = append(paths, filepath.Join(home, ".claude.json"))
	}
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		var v struct {
			OAuthAccount struct {
				EmailAddress     string `json:"emailAddress"`
				OrganizationName string `json:"organizationName"`
				BillingType      string `json:"billingType"`
			} `json:"oauthAccount"`
		}
		err = json.NewDecoder(f).Decode(&v)
		f.Close()
		if err == nil && v.OAuthAccount.EmailAddress != "" {
			return v.OAuthAccount.EmailAddress, v.OAuthAccount.OrganizationName, v.OAuthAccount.BillingType
		}
	}
	return "", "", ""
}

// LiveSessions lists running Claude Code processes across the accounts.
func LiveSessions(accts []Account) []Live {
	var out []Live
	for _, a := range accts {
		if a.Kind() != Claude {
			continue
		}
		files, _ := filepath.Glob(filepath.Join(a.Dir, "sessions", "*.json"))
		for _, f := range files {
			pid, err := strconv.Atoi(strings.TrimSuffix(filepath.Base(f), ".json"))
			if err != nil || pid <= 0 || !alive(pid) {
				continue
			}
			b, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			var v struct {
				SessionID string `json:"sessionId"`
				Status    string `json:"status"`
				Cwd       string `json:"cwd"`
			}
			if json.Unmarshal(b, &v) != nil || v.SessionID == "" {
				continue
			}
			out = append(out, Live{Account: a.Name, PID: pid, SessionID: v.SessionID, Status: v.Status, Cwd: v.Cwd})
		}
	}
	return out
}

func toolRank(t Tool) int {
	switch t {
	case Claude:
		return 0
	case Codex:
		return 1
	case Pi:
		return 2
	case Cursor:
		return 3
	case CursorIDE:
		return 4
	default:
		return 9
	}
}

// piDirs is ~/.pi/agent when it holds sessions/, plus PI_CODING_AGENT_DIR
// when that points somewhere else. CCLI_PI_DIRS replaces both.
func piDirs(home string) []string {
	base := filepath.Join(home, ".pi", "agent")
	if os.Getenv("CCLI_PI_DIRS") != "" {
		return dirsOf("CCLI_PI_DIRS", base, "sessions")
	}
	dirs := dirsOf("", base, "sessions")
	if extra := os.Getenv("PI_CODING_AGENT_DIR"); extra != "" {
		dirs = append(dirs, dirsOf("PI_CODING_AGENT_DIR", base, "sessions")...)
	}
	return dedupe(dirs)
}

func piName(dir, home string) string {
	if filepath.Clean(dir) == filepath.Join(home, ".pi", "agent") {
		return "pi"
	}
	return "pi-" + filepath.Base(filepath.Dir(dir))
}

// cursorDirs is ~/.cursor when it holds CLI transcripts or chat stores.
// CCLI_CURSOR_DIRS replaces the search. CURSOR_CONFIG_DIR is included only
// when it is a data dir (it usually holds cli-config.json, not transcripts).
func cursorDirs(home string) []string {
	base := filepath.Join(home, ".cursor")
	var dirs []string
	if v := os.Getenv("CCLI_CURSOR_DIRS"); v != "" {
		dirs = filepath.SplitList(v)
	} else {
		dirs = []string{base}
		if extra := os.Getenv("CURSOR_CONFIG_DIR"); extra != "" {
			dirs = append(dirs, extra)
		}
	}
	var out []string
	for _, d := range dedupe(dirs) {
		if cursorData(d) {
			out = append(out, d)
		}
	}
	return out
}

func cursorData(dir string) bool {
	for _, sub := range []string{"projects", "chats"} {
		if st, err := os.Stat(filepath.Join(dir, sub)); err == nil && st.IsDir() {
			return true
		}
	}
	return false
}

func cursorName(dir, home string) string {
	if filepath.Clean(dir) == filepath.Join(home, ".cursor") {
		return "cursor"
	}
	return "cursor-" + filepath.Base(dir)
}

// cursorIDEDirs is the Cursor user directory that holds state.vscdb.
// CCLI_CURSOR_IDE_DIRS replaces the platform default.
func cursorIDEDirs(home string) []string {
	base := cursorUserDir(home)
	if os.Getenv("CCLI_CURSOR_IDE_DIRS") != "" {
		var out []string
		for _, d := range filepath.SplitList(os.Getenv("CCLI_CURSOR_IDE_DIRS")) {
			d = filepath.Clean(d)
			if ideDB(d) {
				out = append(out, d)
			}
		}
		return dedupe(out)
	}
	if ideDB(base) {
		return []string{base}
	}
	return nil
}

func cursorIDEName(dir, home string) string {
	if filepath.Clean(dir) == cursorUserDir(home) {
		return "cursor-ide"
	}
	return "cursor-ide-" + filepath.Base(dir)
}

func cursorUserDir(home string) string {
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Cursor", "User")
	case "windows":
		root := os.Getenv("APPDATA")
		if root == "" {
			root = filepath.Join(home, "AppData", "Roaming")
		}
		return filepath.Join(root, "Cursor", "User")
	default:
		if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
			return filepath.Join(x, "Cursor", "User")
		}
		return filepath.Join(home, ".config", "Cursor", "User")
	}
}

func ideDB(dir string) bool {
	st, err := os.Stat(filepath.Join(dir, "globalStorage", "state.vscdb"))
	return err == nil && st.Mode().IsRegular()
}

func dedupe(dirs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, d := range dirs {
		d = filepath.Clean(d)
		if d == "." || seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	return out
}

func alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
