#!/usr/bin/env bash
# Renders the README screenshots from a synthetic home (scripts/demo), so no
# real session data ends up in them. Needs go, jq, tmux and freeze
# (go install github.com/charmbracelet/freeze@latest); ImageMagick, if
# present, shrinks the images.
#
#   scripts/screenshots.sh            # writes docs/images/*.png
set -euo pipefail

repo="$(cd "$(dirname "$0")/.." && pwd)"
out="$repo/docs/images"
work="$(mktemp -d)"
live=""
sock="ccli-shots-$$"
trap '[ -n "$live" ] && { kill "$live"; wait "$live"; } 2>/dev/null; tmux -L "$sock" kill-server 2>/dev/null; rm -rf "$work"' EXIT

home="$work/home"
mkdir -p "$out"
go build -o "$work/ccli" "$repo"

# A process that stays alive, registered as the running session.
sleep 600 &
live=$!
go run "$repo/scripts/demo" -home "$home" -live-pid "$live" >/dev/null

env_vars=(HOME="$home" XDG_DATA_HOME="$home/.local/share" XDG_CONFIG_HOME="$home/.config" XDG_STATE_HOME="$home/.local/state")
ccli() {
	env "${env_vars[@]}" COLUMNS="${COLS:-112}" "$work/ccli" "$@"
}
ccli index >/dev/null

# Saved readings for ccli quota, which runs with --offline here: a live
# check would start the real Claude Code and Codex. The work account's comes
# from the status line wrapper; the default account's is a live check's
# answer as quota saves it, two minutes old.
now=$(date +%s)
snap() { # snap <config dir> <5h %> <5h reset> <7d %> <7d reset>
	local tp
	tp=$(find "$1/projects" -maxdepth 2 -name '*.jsonl' | head -1)
	printf '{"session_id":"demo","transcript_path":"%s","model":{"id":"claude-opus-5-5"},"rate_limits":{"five_hour":{"used_percentage":%s,"resets_at":%s},"seven_day":{"used_percentage":%s,"resets_at":%s}}}' \
		"$tp" "$2" "$3" "$4" "$5" | ccli statusline -- cat >/dev/null
}
snap "$home/.claude-work" 74 $((now + 3000)) 41.2 $((now + 190000))
iso() { date -u -r "$1" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -d "@$1" +%Y-%m-%dT%H:%M:%SZ; }
jq -n --arg dir "$home/.claude" --arg at "$(iso $((now - 120)))" --arg r5 "$(iso $((now + 7800)))" --arg r7 "$(iso $((now + 302400)))" \
	'{account: "default", config_dir: $dir, captured_at: $at, source: "claude-live", plan: "max",
	  five_hour: {used_percent: 31.5, resets_at: $r5}, seven_day: {used_percent: 58, resets_at: $r7},
	  limits: [{name: "Fable weekly", used_percent: 12, resets_at: $r7}]}' >"$home/.local/share/ccli/quota/default.json"

lead=$(ccli sessions --json -n 200 | jq -r '.sessions[] | select(.title == "Audit payment retries across services") | .id')
ahead=$(ccli diff default work --json | jq -r '[.[] | select(.state | test("ahead"))][0].session_id')

finish() { # finish <name>: render $work/<name>.ansi into docs/images/<name>.png
	local name=$1
	freeze "$work/$name.ansi" --language ansi --window --output "$out/$name.png" \
		--padding 24,32 --margin 0 --border.radius 10 --font.size 14 --line-height 1.3 \
		--background "#16161e" >/dev/null
	# freeze renders at 4x; half that is still sharp on high-density screens.
	if command -v magick >/dev/null; then
		magick "$out/$name.png" -resize 50% -strip -colors 128 "$out/$name.png"
	fi
	echo "$out/$name.png"
}

shot() { # shot <name> <ccli args...>
	local name=$1
	shift
	# The fake home lives in a temp dir; show it as a plain home path.
	# Piped output never pages.
	ccli "$@" --color always | sed "s#$home#/Users/alex#g" >"$work/$name.ansi"
	finish "$name"
}

shot stats stats
COLS=136 shot sessions sessions -n 16
shot projects projects "$home"
shot show show "${lead:0:8}"
COLS=120 shot log log "${lead:0:8}"
COLS=100 shot turn log "${lead:0:8}" 1
COLS=124 shot prompts prompts --sort cost -n 14
# For docs/tutorial.md: a turn with tool details, a subagent, a workflow run.
COLS=100 shot tools log "${lead:0:8}" 3 --tools
agent=$(ccli show "${lead:0:8}" --json | jq -r '[.. | objects | select(.description? == "Find every caller of the retry helper") | .agent_id][0]')
run=$(ccli show "${lead:0:8}" --json | jq -r '.workflow_runs[0].id')
COLS=100 shot agent log "${lead:0:8}" --agent "${agent:0:8}"
COLS=100 shot run log "${lead:0:8}" --run "${run:0:8}"
# A Codex turn, from the demo's Codex home.
cx=$(ccli sessions -a codex --json | jq -r '.sessions[] | select(.title | startswith("Add per-client")) | .id')
COLS=100 shot codex log "${cx:0:8}" 3 --tools
shot accounts accounts
shot quota quota --offline
shot diff diff default work --project web-dashboard --all
shot copy copy "${ahead:0:8}" --from work --to default

# The interactive browser, driven in a private tmux server.
tmux -L "$sock" -f /dev/null new-session -d -s v -x 150 -y 38 \
	"env ${env_vars[*]} TERM=xterm-256color $work/ccli view ${lead:0:8}; sleep 60"
sleep 2
tmux -L "$sock" send-keys -t v g
sleep 1
tmux -L "$sock" send-keys -t v j j
sleep 1.5
# tmux writes "default colour" as SGR 39 and 49, which freeze ignores; a plain
# reset draws the same. Padding at line ends is dropped.
tmux -L "$sock" capture-pane -t v -p -e | sed "s#$home#/Users/alex#g" |
	perl -pe 's/\e\[(?:39|49)m/\e[0m/g; s/ +((?:\e\[[0-9;]*m)*)$/$1/' >"$work/view.ansi"
finish view
