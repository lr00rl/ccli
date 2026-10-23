# Using ccli

This guide walks through the jobs ccli is built for: finding your way around
your sessions, reading one turn by turn, finding the prompts worth rewriting,
following the agents a prompt started, keeping one session going across two
accounts, and watching your subscription limits. Every example comes from the
demo home described below, so you can run each command and see the same
sessions, ids and costs; only the times move with the day you generate it.

- [Before you start](#before-you-start)
- [Find your way around](#find-your-way-around)
- [Read a session turn by turn](#read-a-session-turn-by-turn)
- [Find the prompts worth rewriting](#find-the-prompts-worth-rewriting)
- [Follow the agents](#follow-the-agents)
- [Browse interactively](#browse-interactively)
- [Set sessions aside](#set-sessions-aside)
- [Tag sessions](#tag-sessions)
- [Work with two accounts](#work-with-two-accounts)
- [Codex sessions](#codex-sessions)
- [Watch your limits](#watch-your-limits)
- [Script it](#script-it)
- [Keep your history](#keep-your-history)
- [Troubleshooting](#troubleshooting)

## Before you start

Install with Go 1.26 or later:

```sh
go install github.com/lr00rl/ccli@latest
```

ccli finds your accounts on its own: `~/.claude` is shown as `default`, each
`~/.claude-<name>` as `<name>`, and a Codex home, `~/.codex`, as `codex`.
Every command first brings its index up
to date. The first run reads every transcript (about 10 seconds for 4 GB on
an M-series Mac); after that only new lines are read. The index lives in
`~/.local/share/ccli/ccli.db` and is the only thing ccli writes unless you
copy, move or fork a session.

To follow this guide without touching your own data, generate the demo home.
It has two accounts (`alex@example.com` as `default` and `alex@acme.example`
as `work`), a few projects and about 45 made-up sessions:

```sh
go run github.com/lr00rl/ccli/scripts/demo@latest -home /tmp/ccli-demo
export HOME=/tmp/ccli-demo XDG_DATA_HOME=/tmp/ccli-demo/.local/share
ccli accounts
```

Open a new shell when you are done, so `HOME` points back at your own.

## Find your way around

`ccli stats` is the dashboard: totals, a daily chart, projects, models,
accounts, the hours you work and the skills the model called. `ccli accounts`
lists each account with its email, its sessions and what is running in it now.

`ccli sessions` lists sessions active in the last 30 days, newest first. The
filters combine:

```sh
ccli sessions -a work --days 7 --sort cost -n 20   # last week in work, costliest first
ccli sessions --project checkout -q payment        # by folder and by title
ccli sessions --since 2026-10-01 --model sonnet
ccli sessions --dir ~/acme/checkout-service        # started in that folder or below
ccli sessions --tool claude                        # Claude Code only; codex for Codex
```

`-a` picks accounts by name, and `--tool` picks the agent, whatever the
accounts are called. `--dir` and `--tool` also narrow `prompts`, `stats` and
`view`.

To see which folders you have worked in, `ccli projects` groups sessions by
the folder each started in, at or below the current folder, over all time:

```
$ ccli projects ~ --depth 1
LAST     SESSIONS  PROMPTS  TOKENS    COST  ACCOUNTS            PROJECT
23m ago        35      424    1.7M  $71.22  codex,default,work  acme
4d ago         15      180  747.3K  $27.24  default             code
2 projects under ~, all time · 50 sessions · 2.4M tokens · $98.47 list price
ccli sessions --dir <folder> lists a project's sessions
```

Without `--depth` each folder is its own row, so a worktree under
`.claude/worktrees/` shows apart from its repository; `--depth 1` folds
everything into the folder directly below the one you asked about.
`ccli projects /` covers the whole machine.

Any command that takes a session accepts an id prefix, such as `7a93706c`.
When the same session exists in two accounts, ccli picks the copy with the
latest activity; name the account to choose: `work:7a93706c`.

`ccli show 7a93706c` prints one session: its models, how its cost splits
between the main conversation, its subagents and its workflow runs, the tree
of agents with the turn that started each, the tools and commands used, and
the command that resumes it in the right account and folder.

![ccli show](images/show.png)

## Read a session turn by turn

A turn is one prompt and everything that happened until the next prompt.
`ccli log` lists a session's turns:

```sh
ccli log 7a93706c
```

![ccli log](images/log.png)

Each row shows when the turn ran, how long it took, what it cost, how many
tool calls it made, how many agents it started and how the prompt begins. The
cost includes the subagents and workflow runs the turn started. Turns opened
by a background task report, a message from another session or the system are
hidden; `--all` shows them.

Name a turn to read it in full, or a range, or `last`:

```sh
ccli log 7a93706c 6          # turn 6
ccli log 7a93706c 3-6        # turns 3 to 6
ccli log 7a93706c last       # the latest turn
```

![ccli log for one turn](images/turn.png)

A full turn shows your prompt and the replies rendered as markdown, one line
per tool call, the prompt given to each subagent, and the agents and workflow
runs the turn started. The header says where the cost went:

```
Turn 6  7a93706c · work · Audit payment retries across services
2026-10-15 18:07 · took 1m · $0.38 ($0.15 here, $0.22 in agents) · 3 model calls · 2 tool calls
prompt 103 chars
```

Four flags add detail:

| Flag | Shows |
|------|-------|
| `--tools` | each tool call's input and output |
| `--thinking` | the model's thinking |
| `--context` | text Claude Code added on its own: skill bodies, reminders, messages from other sessions |
| `--raw` | prompts and replies as recorded, without markdown rendering |

![ccli log with --tools](images/tools.png)

Long output goes through `$PAGER` (`less -FRX` when unset); `--no-pager`
prints it straight.

## Find the prompts worth rewriting

`ccli prompts` lists the prompts you typed across every session. Each row
names the session and turn, so `ccli log <id> <turn>` opens it.

```sh
ccli prompts --days 7 --sort cost -n 20
```

![ccli prompts](images/prompts.png)

The sort orders answer different questions:

| Sort | Finds |
|------|-------|
| `cost` | turns that cost the most, agents included |
| `tools` | prompts that sent the model searching and reading a lot |
| `duration` | turns that ran longest |
| `tokens` | turns that moved the most tokens |
| `length` | the longest prompts, such as pasted logs |

`-q` searches the prompt text. Searching for a follow-up you type often shows
how often you needed it:

```
$ ccli prompts -q "simpler approach" --days 0 -n 6
WHEN         TURN         PROJECT           TOOK   COST  TOOLS  PROMPT
10-15 21:52  7a93706c:44  checkout-service    3m  $0.41      4  Try the simpler approach first.
10-15 20:23  7a93706c:29  checkout-service    1m  $0.23      3  Try the simpler approach first.
10-15 20:06  7a93706c:25  checkout-service   25s  $0.07      1  Try the simpler approach first.
10-15 19:58  7a93706c:23  checkout-service    3m  $0.41      5  Try the simpler approach first.
10-15 19:41  7a93706c:20  checkout-service    1m  $0.23      3  Try the simpler approach first.
10-15 19:05  7a93706c:16  checkout-service    1m  $0.19      3  Try the simpler approach first.

6 of 84 prompts · $1.53 shown · ccli log <id> <turn> prints one in full
```

To find every prompt you repeat, count them with `jq`:

```sh
ccli prompts --days 0 -n 0 --json | jq -r '.prompts[].prompt' | sort | uniq -c | sort -rn | head
```

A routine that works well once a week:

1. List the costliest turns: `ccli prompts --days 7 --sort cost -n 20`.
2. Open the top few with `ccli log <id> <turn>`. The header tells you whether
   the money went into the conversation itself or into the agents it started.
3. Add `--tools` to see what the model searched and read before it could
   answer. A long run of searches usually means the prompt left out where to
   look.
4. Add `--context` to see what Claude Code added to the turn, such as skill
   bodies and reminders, which are tokens too.
5. For prompts you repeat, move the instruction where it applies every time:
   your `CLAUDE.md`, a skill or a slash command.

In `ccli view`, `y` copies the selected prompt to the clipboard, ready to
rework.

## Follow the agents

`ccli show` draws the tree of subagents, including subagents started by
subagents, with the turn that started each. `ccli log` reads them:

```sh
ccli log 7a93706c --agent a24cd2c9     # one subagent's transcript
ccli log 7a93706c --run wf_df79        # a workflow run's agents and how each ended
```

![ccli log --agent: a subagent's task, what it did and its reply](images/agent.png)

A subagent's transcript starts with the task it was given, then shows its
tool calls and its reply, read the same way as a turn. The flags from the
previous sections work here too.

![ccli log --run: a workflow run with each agent's phase, status and cost](images/run.png)

A workflow run lists its agents with their phase, how each ended (`done`,
`failed` or `unfinished`) and what each cost. Pass any agent's id to
`--agent` to read it.

## Browse interactively

`ccli view` puts the same views in one browser: sessions grouped by project
(the project used last first; `/` and a project name narrows the list to it),
then a session's turns, then the agents a turn started, then an agent's
transcript. The preview on the right follows the selection; on a narrow
terminal the preview opens full screen.

```sh
ccli view               # all sessions
ccli view 7a93706c      # start at one session's turns
```

![ccli view](images/view.png)

| Key | Does |
|-----|------|
| `↑` `↓` `j` `k` | move in the list, or scroll the preview when it has focus |
| `pgup` `pgdn` `g` `G` | a page, the top, the bottom |
| `J` `K` | scroll the preview while the list has focus |
| `enter` | open a session's turns or a workflow run's agents; on a turn, focus its preview |
| `tab` `→` `l` | focus the preview |
| `esc` `←` `h` | go back a level |
| `/` | filter by what the prompt says |
| `s` | sort by cost, or back to order |
| `a` | include turns started by tasks, other sessions and the system |
| `o` | list the agents and workflow runs the selected turn started |
| `y` | copy the prompt to the clipboard |
| `t` `T` `c` `r` | toggle tool details, thinking, added context, raw text |
| `m` | in the sessions list, mark a session (to hide several at once) |
| `x` | hide the marked sessions, or the selected one, after asking |
| `H` | list only hidden sessions; `u` there brings them back |
| `#` | tag the selected session; an empty tag removes it |
| `?` | list every key |
| `q` | quit |

The mouse wheel scrolls the pane under the pointer, and a click selects a
row. When you press `enter` on a row whose preview is still loading, it opens
as soon as the preview arrives.

## Set sessions aside

One-off questions and abandoned attempts pile up and push the sessions you
still care about down the list. Hide them:

```sh
ccli hide 3b3b57cb 21c4ab7e
```

Hidden sessions leave `ccli sessions`, `ccli prompts` and the sessions list of
`ccli view`. Nothing else changes: the transcripts stay where Claude Code put
them, so `ccli resume` still works, `ccli show` and `ccli log` still open them
by id, and `stats`, `quota` and `accounts` still count what they used. Hiding
marks the session, so its copies in other accounts go too.

In `ccli view`, `m` marks sessions and `x` hides the marked ones, or the
selected one when none is marked, after a question in the footer. Filtering
first makes this quick: `/` and `tmp`, then `m` on each session to clear out.

To see what you hid, use `ccli sessions --hidden` or `H` in `view`. Bring a
session back with `ccli unhide <id>` or `u` in that list. A hidden session
that you resume and use again shows again by itself. A session that is running
cannot be hidden: hide it once it has ended.

## Clear out empty sessions

Sessions you opened and closed without asking anything, or that stopped
before the first reply, add rows that say nothing. `ccli clean` finds them:

```
$ ccli clean
LAST     ACCOUNT  ID        DO       PROJECT
11d ago  default  f33442ad  archive  ~/code/photo-sync
9d ago   work     5a9c2637  archive  ~/acme/checkout-service
6d ago   codex    9e947588  hide     ~/acme/checkout-service
Archive 2 sessions, hide 1 session? [y/N] y
✓ 2 archived, 1 hidden, 0 forgotten
Undo with: ccli clean --undo 20261020-201340
```

A session counts as empty when it has no prompt, no reply, no tool call, no
tokens or cost, no subagents and no name, is not running, and has been idle
for an hour or more. Claude Code transcripts move to ccli's archive under
`~/.local/share/ccli/archive/clean/`; Codex, Pi and Cursor sessions are
hidden, since ccli does not write into their homes. Nothing is deleted, and
the run can be put back:

```
$ ccli clean --undo 20261020-201340
Put back 2 transcripts and brought back 1 hidden session.
```

Sessions where you typed a prompt that never got an answer, often a run that
failed to start, count only with `--unanswered`, because they hold your own
text. Sessions you hid or tagged are left alone. `--dir .` limits the search
to the current project and `--tool codex` to one agent. In a script, `ccli clean` without a terminal prints the plan
and changes nothing; add `--apply` to carry it out.

## Tag sessions

A session's title is its own `/rename`, the title Claude Code generated, or
its first prompt. When several sessions look alike, give them tags: short
labels you choose, shown next to the title.

```sh
ccli tag 3b3b57cb dedupe fix
ccli tag 7a93706c payments
```

```
✓ 3b3b57cb tagged dedupe fix · Investigate duplicate invoice events
✓ 7a93706c tagged payments · Audit payment retries across services
```

The title stays as it is. Lists add a TAG column when some session on them
has a tag, `show` prints the tag above the project, and `-q` finds a session
by its tag as well as its title:

```sh
ccli sessions -q payments
```

```
LAST    ACCOUNT  ID        TAG       PROJECT           TITLE                                 MODEL        ACTIVE  TOKENS  CACHED    COST
2h ago  work     7a93706c  payments  checkout-service  Audit payment retries across servic…  opus-5-5 +2  2h 42m  210.6K   25.7M  $11.72
1 session · 210.6K tokens · 25.7M cached · $11.72 list price
```

In `ccli view`, `#` opens the selected session's tag in a line at the bottom:
type it and press `enter` to save, or `esc` to leave it. Tags line up in their
own column in the list. A tag is at most 40 characters; `ccli tag <id>
--clear`, or an empty tag after `#`, removes it.

A tag lives in ccli's index, like a hide mark. Nothing is written to the
transcript, no Claude Code process starts, and the session keeps its place in
the list because its last activity does not change. It labels the session's
copies in every account, and Codex sessions too. Claude Code's own resume
picker does not show tags; to change the title it shows, use `/rename` inside
Claude Code, and ccli picks that title up as well.

## Work with two accounts

If you keep a personal and a work login side by side, a session often starts
in one and continues in the other. ccli keeps the copies apart and keeps each
account's usage with that account.

### See where each session lives

```
$ ccli diff default work --project mobile-app
ID        LAST        PROJECT     TITLE                             STATE          SIZE default/work
487c3c16  2026-09-27  mobile-app  Add offline sync to the mobile …  only in work             - / 49K
746cbfa4  7h ago      mobile-app  Why does the nightly export tak…  work is ahead          65K / 97K
b9c85738  13d ago     mobile-app  Add offline sync to the mobile …  only in work             - / 18K
0 only in default · 2 only in work · 0 default ahead · 1 work ahead · 0 diverged
To bring work's sessions into default: ccli copy --from work --to default  (prints the plan; add --apply)
```

"work is ahead" means default's transcript is an exact prefix of work's: the
session went on in work after default last saw it.

### Which copy you are looking at

`746cbfa4` is in both accounts under the same id. `show`, `log`, `export`
and `view` read one copy: the one you name with `-a` (or as
`default:746cbfa4`), or else the one used last, and they say which on
stderr:

```
$ ccli log 746cbfa4
ccli: 746cbfa4 is also in work; this is the copy in default, the one used last (-a work picks another)
Why does the nightly export take forty minutes now?
746cbfa4 · default · 25 prompts typed · $0
```

In a copy's turn list, the turns that came in with a copy show `copied` in
the COST column, and a line under the table explains them:

```
turns 18 to 25 came with a copy from work; their usage counts in work (ccli log 746cbfa4 -a work)
```

The README's "Sessions, copies and accounts" table lists what every command
works on and what `-a` means for it.

### Continue a session in the other account, and come back

Say `746cbfa4` went on in work, and now you want to continue it in default.
Exit Claude Code in work first, then:

```
$ ccli resume 746cbfa4 -a default
ccli: brought 746cbfa4 up to date in default from work (fast-forward)
```

`resume -a` extends default's copy with what work added, then runs
`claude --resume` in the session's folder under default. It only appends to
a copy that the newer one extends exactly, so nothing in default's copy is
rewritten. When you go back to work later, do the same the other way:

```sh
ccli resume 746cbfa4 -a work
```

If default has no copy yet, the same command creates one. Add `--no-sync` to
resume an account's copy only when it is already up to date:

```
$ ccli resume 746cbfa4 -a default --no-sync
ccli: default has an older copy of 746cbfa4 than work. Leave out --no-sync to bring it up to date first, or resume it in work
```

Each copy appears in `ccli show` under "Copies between accounts":

```
Copies between accounts
  2026-10-16 00:28  copy work → default  41K in 2 files
```

Usage stays where it was made. The turns you ran in work count toward work
and the turns you ran in default count toward default, however often the
session goes back and forth. ccli records every byte range it copies into an
account, and a response that arrived with a copy never counts toward the
account it was copied into. A copy that has not done any new work yet shows
$0, and its MODEL column stays empty until it makes its first call. In
`ccli sessions` its account reads `default ← work`, naming where it came from.

Add `--print` to see the command without running it (with `-a`, the copy is
still brought up to date, so the command opens the newest one), and `--fork`
to resume into a new session id (Claude Code's `--fork-session`).

### Pitfalls when switching accounts

These are the mistakes that are easy to make, and what ccli says about them.

**Planning a copy without applying it.** `copy`, `move` and `fork` print a
table of what they would do and stop. The last line says
`Dry run, nothing was copied`. Until you run the command again with
`--apply`, the session is not in the target account, and
`claude --resume <id>` there answers "No conversation found". You can skip
`copy` entirely: `ccli resume <id> -a <account>` copies the session in as
part of resuming it.

**Switching while Claude Code still runs the session.** ccli does not copy a
session that a process is running, because the transcript is still growing.
It names the account:

```
ccli: 746cbfa4 is running in work; exit Claude Code there, then run this again to bring default's copy up to date
```

**Using the resume line Claude Code prints on exit.** When Claude Code exits
it prints `Resume this session with: claude --resume <id>`. The line has no
`CLAUDE_CONFIG_DIR`, so in a plain shell it opens the default account's copy
of the session, even if you were working in another account. If that copy is
older and you continue it, the two copies diverge, and ccli can no longer
bring one up to date from the other. Resume through ccli instead:
`ccli resume <id>` picks the copy with the latest activity, and
`ccli resume <id> -a <account>` brings that account's copy up to date first.

**Reading $0 as a missing session.** A fresh copy shows $0 and no model.
That is usage staying in the account that spent it, not lost data; `ccli
show` lists the turns, and the copy's own cost grows as you work in it.

**A `/rewind` backup that differs in both accounts.** Claude Code backs up
each file it edits under a numbered name, and after a resume it starts the
numbers again, replacing its own older backups. So a copy can meet a
backup of the same name with other content. The newer one takes the name
and the older one moves to ccli's archive; the copy says which:

```
$ ccli copy 746cbfa4 --from work --to default --apply
✓ 746cbfa4 Why does the nightly export take forty minutes no… fast-forward work → default, 4 files written, 1 older /rewind backup replaced
    replaced 1 /rewind backup with the newer one of the same name, as Claude Code does; the older is in ~/.local/share/ccli/archive/20261022-232541/default
1 of 1 sessions copied.
```

If the target's backup is the newer one, it stays and the copy lists it as
left alone. The conversation is complete either way. What can differ is
`/rewind`'s restore of code at an older checkpoint, which may bring back a
newer version of that file; Claude Code does the same after any resume, so
keep the code history in git.

### When both copies went on

If you continued the session in both accounts without syncing, the copies
have diverged and ccli will not merge them. They are two conversations with
a shared beginning, and Claude Code loads one line of messages, so a merged
file could keep only one of them active. `resume -a` resumes that account's
own copy and tells you about the other one. To continue the other copy here
as well, fork it into a new session:

```
$ ccli fork d244741f --from work -a default
Fix flaky cart total test around daylight saving
fork d244741f from work into default as a new session 77ad76f7
  writes a 74K transcript; the original is not touched
  usage already in the transcript stays with work; only new work counts toward default
Dry run, nothing was forked. Add --apply to do it (a new id is drawn each time).
```

`-a` names the account the fork goes into and continues in, and it is
required; it may be the session's own account. `--to` still works and means
the same.

### Fork from an earlier turn

Sometimes a session went somewhere you did not want, and you would rather
pick it up from an earlier point. `fork --turn` keeps the conversation
through one turn and leaves the rest out, in a new session beside the
original. Find the turn in `ccli log`:

```
$ ccli log 746cbfa4
Why does the nightly export take forty minutes now?
746cbfa4 · work · 25 prompts typed · $5.24

 TURN  WHEN         TOOK   COST  TOOLS  AGENTS  PROMPT
    1  10-21 15:01    2m  $0.21      5          Why does the nightly export take forty minutes now?
    2  15:05          1m  $0.14      3          Try the simpler approach first.
    3  15:30          1m  $0.17      3          Check how the other services call this endpoint.
    4  15:35          2m  $0.27      4          Check how the other services call this endpoint.
    5  15:41          1m  $0.16      3          That works. Clean up the naming and commit it.
    6  15:44          2m  $0.17      3          Look at the failing test and find the cause before …
```

`ccli log` hides the turns that tasks and other sessions started; `--all`
lists them, and their numbers work with `--turn` too, from 1 up (a turn 0
there is what came before the first prompt, and a fork cannot end in it).
Then plan the fork:

```
$ ccli fork 746cbfa4 --turn 5 -a default
Why does the nightly export take forty minutes now?
fork 746cbfa4 from work into default as a new session 681ae78a
  keeps the conversation through turn 5 of 25, which began: That works. Clean up the naming and commit it.
  leaves out turns 6 to 25 and 1 agent transcript started in them
  writes a 21K transcript and 1 session folder; the original is not touched
  usage already in the transcript stays with work; only new work counts toward default
  only the conversation goes back to turn 5: files on disk stay as they are now, and the fork knows nothing of the changes made after that turn
Dry run, nothing was forked. Add --apply to do it (a new id is drawn each time).
```

Add `--apply` to write it, then `ccli resume <new id>` to continue. The fork
keeps the transcript up to the prompt of the next turn, and leaves out the
transcripts of agents started after the turn you chose. ccli checks that
cut against the transcript twice, when it plans and again just before it
writes; if the transcript no longer matches its index there, it stops and
asks you to run the command again. Forking through the last turn keeps
everything ccli has read; if the session wrote more since, the plan says
how much it leaves out. Without `--turn`, a fork keeps the whole
conversation.

Read the last line of the plan carefully. Only the conversation goes back.
The files the session changed after that turn stay changed, and the fork's
Claude does not know about those changes, so it may describe the code as it
was. To roll the code back as well, use git, or `/rewind` inside Claude Code,
which can restore the files it tracked. The original session is never
touched, so you can always go back to it.

### Copy and move many sessions

`copy` and `move` take session ids or, with `--from`, a batch:

```sh
ccli copy 746cbfa4 --to default                   # one session
ccli copy --from work --to default --project mobile-app
ccli move --from work --to default --days 30 --apply
```

Both print a plan and write nothing until you add `--apply`. A batch takes
sessions missing from the target and sessions the source is ahead on
(`--include missing,ahead`). ccli refuses a session that a Claude Code process
is running in any account, a target that is newer, and copies that have
diverged. `move` checks every copied file byte for byte, then archives the
source under `~/.local/share/ccli/archive/` instead of deleting it.

![ccli copy dry run](images/copy.png)

## Codex sessions

If you also use Codex, its homes are accounts like any other: your
sessions, prompts and turns appear next to Claude Code's, and every command
above works on them. The demo home has one, `codex`:

```sh
ccli sessions -a codex
ccli log 01a4547a 3 --tools
```

![ccli log for a Codex turn](images/codex.png)

A Codex thread that spawned agents lists them under the turn that started
each, the same as Claude Code's subagents, and `ccli log <id> --agent <a>`
reads one. Costs are OpenAI's list prices; models Codex ran through another
provider, or with no public price, are counted in tokens and marked `+`.
`ccli resume` runs `codex resume` in the session's folder. `codex resume`
does not find a thread Codex has archived, so for one of those ccli prints
the `codex unarchive <id>` command to run first, and leaves running it to
you. Copying, moving and forking work between Claude Code accounts only.

## Watch your limits

`ccli quota` shows, for each account, how much of the 5-hour and the weekly
window is used, when each resets, any per-model weekly limit or extra use the
plan has, and what this machine used inside each window.

![ccli quota](images/quota.png)

The figures are current: `quota` asks each account's own tool at that moment.
Claude Code answers with the data its `/usage` command shows, so use on
claude.ai, the desktop app and other machines is included; Codex's app server
answers with the account's rate limits, including windows of other lengths. ccli
starts each tool once, all accounts in parallel, in an empty folder of its own.
It sends no prompt, loads none of your hooks, plugins or MCP servers, and
writes no transcript. It never reads your credentials: the tool signs in as it
always does. The check takes a few seconds, and ccli says so on the terminal
while it waits.

When a check fails, the account says why and shows its saved figures instead:

```
codex  Codex
  live check failed: Codex: codex account authentication required to read rate limits
  no limits recorded yet
```

Every check is saved, so `ccli quota --offline` shows the last figures at once
without starting anything. Two passive sources keep those saved figures fresh
between checks. Claude Code passes the limits to your status line command
after each reply; put `ccli statusline --` in front of that command in each
account's `settings.json`:

```json
"statusLine": { "type": "command", "command": "ccli statusline -- ~/.claude/ccline/ccline" }
```

The wrapper records the numbers, runs your command with the same input and
prints its output unchanged; it adds about 20 ms. If you have no status line
yet, `ccli statusline -- true` records the numbers and shows nothing. If you
use [ccline](https://github.com/Haleclipse/CCometixLine), ccli also reads the
percentages from its cache. For Codex, the index picks up the limits Codex
writes to its session logs with each reply. Each reading says where it came
from and how old it is.

## Script it

Every command takes `--json` and prints the same data the table shows. A few
examples:

```sh
# The costliest prompts of the week, agents included
ccli prompts --days 7 --sort cost -n 10 --json |
  jq -r '.prompts[] | "\(.cost + .agent_cost | . * 100 | round / 100)  \(.session_id[0:8]) \(.seq)  \(.headline)"'

# Cost per day
ccli stats --days 14 --json | jq -r '.daily[] | "\(.date) \(.cost | . * 100 | round / 100)"'

# Cost per account
ccli stats --days 30 --json | jq -r '.by_account[] | "\(.name) \(.cost | round)"'

# Sessions running right now
ccli sessions -n 0 --json | jq -r '.sessions[] | select(.live) | "\(.account):\(.id[0:8]) \(.title)"'
```

In JSON, a prompt's `cost` is its turn's own calls and `agent_cost` is the
agents it started; together they are the cost the table shows.

## Keep your history

Claude Code deletes transcripts that have not changed for `cleanupPeriodDays`
(30 by default). ccli keeps its totals and your prompts after that, but
reading an old turn, resuming or copying a session needs its transcript. To
keep transcripts longer, set it in each account's `settings.json`:

```json
"cleanupPeriodDays": 180
```

`ccli index --rebuild` reads every transcript again from the start; history
for transcripts already deleted stays as it was.

To remove ccli, delete the binary, `~/.local/share/ccli` (the index and any
archived sessions) and `~/.config/ccli`, and take `ccli statusline --` out of
your status line command.

## Troubleshooting

**"no session matches"**: the prefix matches nothing in the index. Check
`ccli sessions --days 0 -q <prefix>`; the index only knows transcripts it has
read.

**The wrong copy opens**: a session in two accounts resolves to the copy with
the latest activity. Name the account, as in `work:7a93706c`.

**"ccli view needs a terminal"**: `view` cannot run with its output piped.
`ccli log` prints the same turns.

**The pager gets in the way**: pass `--no-pager`, or set `PAGER` to the pager
you prefer.

**Colors are wrong**: `--color never` turns them off and `--color always`
keeps them in a pipe. Markdown uses a dark style unless `CCLI_THEME=light` is
set or `COLORFGBG` says the background is light.

**An account is missing**: ccli looks for `~/.claude` and `~/.claude-*` with a
`projects` folder. To use other folders, list them in `CCLI_CONFIG_DIRS`,
separated by `:`; that list replaces the default search.

**A model shows no cost**: models missing from the price table are counted
in tokens and left out of cost. Add them to `~/.config/ccli/prices.json`, in
USD per million tokens:

```json
{ "claude-example-1": { "input": 3, "output": 15, "cache_read": 0.3 } }
```

**A command is slow**: `--no-index` skips the index update, for when you
know nothing changed. `CCLI_DB` points ccli at another index file.
