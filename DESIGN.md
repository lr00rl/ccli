# ccli design

`ccli` reports on Claude Code and Codex sessions across several local
accounts, and moves Claude Code sessions between accounts. An account is one
Claude Code config dir: `~/.claude` (account `default`) or `~/.claude-<name>`
(account `<name>`), selected with `CLAUDE_CONFIG_DIR`; or one Codex home:
`~/.codex` (account `codex`) or `~/.codex-<name>` (account `codex-<name>`),
selected with `CODEX_HOME`. `CCLI_CONFIG_DIRS` and `CCLI_CODEX_DIRS` replace
either search.

## Boundaries

ccli is a reader. The only things it writes are:

- its own database, `$XDG_DATA_HOME/ccli/ccli.db` (default
  `~/.local/share/ccli/ccli.db`, override `CCLI_DB`), which also holds
  the marks `hide` sets;
- for `copy`, `move`, `fork` and `resume --sync` only: transcript files of
  the session in the target account (`projects/<key>/<id>.jsonl`,
  `projects/<any key>/<id>/`, `file-history/<id>/`; a fork writes them under
  its new id), and for `move`, an archive of the source under
  `$XDG_DATA_HOME/ccli/archive/` before the source files are removed.

In a Codex home it reads only the rollout files under `sessions/` and
`archived_sessions/`, and writes nothing. It never opens `auth.json` (which
holds the credentials and the signed-in identity) or `config.toml`, and never
opens Codex's SQLite databases: opening one read-only can still create a
`-shm` file next to it.

It never reads or writes credentials (Keychain, `.credentials.json`), never
writes `.claude.json`, `settings*.json`, `sessions/` (the live-process
registry), `teams/`, `jobs/`, `session-env/`, or anything else in a config dir.
From `.claude.json` it reads only `oauthAccount.emailAddress`,
`organizationName` and `billingType`, to label an account. From
`<dir>/ccline/.api_usage_cache.json`, when a status line program left one,
it reads only the usage percentages and times. It never sends a request to a
model or to any other service (see Quota).

## What a session consists of

Paths are relative to the account's config dir.

| Path | Content | Indexed | Copied |
| --- | --- | --- | --- |
| `projects/<key>/<id>.jsonl` | main transcript, append-only | yes | yes |
| `projects/<key>/<id>/subagents/agent-<a>.jsonl` | subagent transcript | yes | yes |
| `projects/<key>/<id>/subagents/agent-<a>.meta.json` | `agentType`, `description`, `toolUseId`, `spawnDepth`, `requestShape` | yes | yes |
| `projects/<key>/<id>/subagents/workflows/<run>/agent-<a>.jsonl` | workflow agent transcript | yes | yes |
| `projects/<key>/<id>/subagents/workflows/<run>/journal.jsonl` | workflow events (launched, started, result, failed) | yes | yes |
| `projects/<key>/<id>/workflows/<run>.json` | workflow run state: name, status, summary, start, duration, agent count | yes | yes |
| `projects/<other key>/<id>/` | folders made under another project key after the session changed its working folder | runs only | yes |
| `projects/<key>/<id>/tool-results/`, `scratchpad/` | large tool outputs, scratch files | no | yes |
| `file-history/<id>/` | `/rewind` checkpoints | no | yes |
| `projects/<key>/memory/` | project auto memory, shared by all sessions | no | no |
| `sessions/<pid>.json` | live process: `sessionId`, `status`, `cwd` | read for liveness | no |

## Transcript fields used

- Every line: `type`, `timestamp`, `cwd`, `sessionId`, `isSidechain`, `isMeta`.
- `assistant`: `message.id`, `requestId`, `message.model`, `message.usage`
  (`input_tokens`, `output_tokens`, `cache_read_input_tokens`,
  `cache_creation_input_tokens`, `cache_creation.ephemeral_5m_input_tokens`,
  `cache_creation.ephemeral_1h_input_tokens`, `server_tool_use.web_search_requests`,
  `speed`), `message.content[].type/name/id/input.skill`.
- `user`: `message.content` (string or blocks), `origin.kind`,
  `promptSource`, `promptId`, `isCompactSummary`, `toolUseResult`.
  A prompt is a user line that is not meta, not a sidechain, not a compact
  summary, carries text or an image and no `tool_result`, and is not output
  (`<local-command-stdout>`, `<bash-stdout>` and the like, or Claude Code's
  `[Request interrupted by user]` marker). Its kind decides how it counts:
  `origin.kind` `human` is typed (this covers typed text, slash commands,
  accepted suggestions and prompts sent from the desktop or a phone);
  `task-notification` is a background task report; `peer` and `coordinator`
  are other sessions. Without `origin`, `promptSource` `typed` or
  `suggestion_accepted` is typed. Older lines carry neither: text starting
  with `<task-notification>` is a task report, text starting with "Another
  Claude session sent a message:" or `<teammate-message` is a peer, the rest
  is typed. Every prompt starts a turn; only typed ones count as prompts.
- A prompt whose `promptId` already started a turn in the same file begins a
  replay: on resume Claude Code can write a stretch of the conversation
  again with new uuids but the same prompt ids and times. Nothing in a replay
  counts again, up to the next new prompt.
- `toolUseResult.agentId`, `.runId` and `.teammate_id` link an Agent or
  Workflow call to the subagent, workflow run or teammate it started.
- `system.turn_duration.durationMs`: Claude Code's own time for a turn.
- Titles: `custom-title.customTitle` (from `/rename`), then
  `ai-title.aiTitle`, then the first typed prompt.
- Team membership: `teamName`, `agentName` on lines; `agent-name` lines.
- Links: `continued-in.continuedInSessionId`, `bridgeSessionId`.
- `cost-state.totalCostUSD`: Claude Code's own estimate, kept for comparison.

One API response is written as several lines that share `message.id` and carry
identical usage (one line per content block). Usage is counted once per
`message.id` (falling back to `requestId`). A later line with different usage
for the same id replaces the earlier one. Across files, a response belongs to
the file that made it: a line in a byte range that ccli copied into the file
never owns it, and among the rest the file created first does. A forked or
copied session is not counted twice, and a session that went back and forth
between accounts leaves each response with the account that ran it. Model
`<synthetic>` is skipped.

Subagent meta files add `name` (a skill run as a subagent), `parentAgentId`
(the subagent that started this one) and, for workflow agents,
`workflowPhase`; subagent lines carry `attributionAgent`, used as the type
when there is no meta file. A workflow run's state file,
`projects/<key>/<id>/workflows/<run>.json`, gives its `workflowName`,
`status`, `summary`, `startTime`, `durationMs` and `agentCount`; it sits
under whichever project folder the session was in when the run started.

## Codex

A Codex rollout is one thread: `sessions/YYYY/MM/DD/rollout-<time>-<id>.jsonl`,
one `{timestamp, type, payload}` object per line (files written by CLI 0.147
and later also number their lines with `ordinal`). Line 0 is the thread's
`session_meta`. The rules below were checked against 2,022 rollouts written by
CLI 0.114 to 0.150.

- **Sessions and agents**: a thread whose `session_meta.source` is
  `{subagent: ...}` was started by another thread. It is indexed as a
  subagent of the user's thread it descends from (`session_id`, or following
  `parent_thread_id` in older files), nested under the agent that spawned it.
  The index reads line 0 of a file once, when it first sees it.
- **Turns**: a turn starts at `event_msg/task_started`. A person's prompt is
  `event_msg/user_message` up to CLI 0.146, an `item_completed` item of type
  `UserMessage` since, and in some exec versions only the `response_item`
  user message. The first prompt in a turn opens it; a later one was sent
  while the model worked. Text Codex injects (environment, AGENTS.md,
  permissions, notifications) is recognised by its per-part kinds
  (`content_item_kinds`, since 2026-08-27) or by its opening tag.
- **Tokens**: `token_count` carries the running total of the process that
  wrote it, and that call's own usage (`last_token_usage`). Cached input is
  part of input and reasoning part of output. A count is new usage when it
  moves one of the file's streams on by exactly its own usage; one equal to a
  stream's total repeats an earlier count or follows a compaction (whose last
  usage is a context estimate) and adds nothing; one that moves a stream on
  by more, on every field, follows counts that were not written, and the
  whole difference counts; anything else starts a stream (a resume restarts
  the counter, and two processes can write one file). The streams' totals
  carry over between incremental parses (`files.carry`). On real data the
  usage counted equals Codex's own final total in every file with one
  stream, and the sum of the streams' totals in every file with several.
- **Forks**: a thread with `forked_from_id` copied its parent's history, and
  none of the copy counts. Since CLI 0.147 the copied lines are those below
  `subagent_history_start_ordinal`, or none when `history_base` is set.
  Before that the copy is a burst of lines that all carry the time of the
  fork (session_meta lines of the ancestors included); the fork's own first
  turn is the last one started in that burst, and its counter continues from
  the parent's total.
- **Models**: from `turn_context.model`, and from `thread_settings_applied`,
  `world_state` or the instructions' provenance before the first turn
  context. Prices are OpenAI's list prices; models Codex runs through other
  providers, and ones with no public price, are counted in tokens only.
- **Limits**: each `token_count` also carries `rate_limits`. Windows are told
  apart by `window_minutes` (300 for 5 hours, 10080 for the week): what Codex
  calls primary was the 5-hour window until mid 2026 and is the weekly one
  since. Only `limit_id` `codex` is read; per-model limits and prepaid
  credits are left out. The latest reading per account is saved as that
  account's quota snapshot.
- **Not supported**: `diff`, `copy`, `move`, `fork` and `resume -a` work
  between Claude Code accounts only, and Codex has no live-process registry,
  so a running Codex session is not marked. `resume` runs
  `codex resume <id>` in the session's folder, with `CODEX_HOME` set for a
  home other than `~/.codex`.

## Relationships

ccli builds a session graph from these sources:

| Relation | Source |
| --- | --- |
| session -> subagent | the subagent folder; `meta.toolUseId` names the parent's tool call, `spawnDepth` the nesting |
| session -> workflow run -> agents | `subagents/workflows/<run>/`, its `journal.jsonl` |
| session -> continued session | `continued-in` line |
| team lead -> teammates | teammate transcripts' `teamName` / `agentName`; a team is named `session-<first 8 chars of the lead's session id>`, and `teams/<team>/config.json` (`leadSessionId`) confirms it while the team exists (Claude Code removes it at session end) |
| fork / copy | sessions sharing response ids; the ledger (`transfers`) names the copies ccli made |
| turn -> subagents and runs | the turn of the Agent or Workflow call (`spawns.turn`) whose id the subagent's meta or the call's result names; else the turn of the subagent that started it; else the turn in progress when it began |

Background jobs (`jobs/<id>/state.json`) are not read: the docs call that file
unstable and point to `claude agents --json` instead, which is left for later.

`ccli show <id>` prints the tree. The graph is stored as edges so `ccli web`
can draw it later.

## Store

SQLite through `modernc.org/sqlite` (pure Go). Aggregates are kept per file,
so one file can be re-parsed without touching others.

- `files`: path, account, session, kind (main, subagent, workflow), agent id,
  size, mtime, parsed offset, head hash, last timestamp, missing flag, the
  turn in progress and the replay flag at the parsed offset, the subagent's
  meta (type, description, skill, parent agent, workflow phase) and
  attribution, and the per-file aggregates (prompts by kind, replies, tool
  calls, active ms, first/last timestamp, cwd, titles, team, agent name,
  cost-state).
- `responses`: one row per deduplicated API response with tokens by kind,
  model, speed, timestamp, owning file, the turn it belongs to, whether its
  line was copied in, and cost at index time.
- `turns`: per main transcript and turn: the prompt's file offset, time,
  kind, text (up to 64 KiB) and prompt id, and the turn's tool calls, agents
  started, last line time, Claude Code's own duration and active time. Turn
  costs come from `responses` grouped by turn.
- `activity`: active milliseconds per file per UTC hour.
- `tools`: tool and skill use counts per file, and slash commands typed
  (`name = 'Command'`).
- `links`: continuation links (`continued-in`) per file.
- `spawns`: per file, each Agent, Task and Workflow call: its tool-use id,
  tool, turn, and what it started (agent id, run id or `teammate:<name>`).
- `processes`: each file's latest `cost-state` snapshot per process and
  model, and whether it was copied in.
- `runs`: workflow run event counts from `journal.jsonl` and what its state
  file says, with each file's mtime so unchanged files are not read again.
- `transfers`: the ledger of copies ccli made: time, mode (copy, move,
  fork), session, new session id for a fork, source and target account, and
  the byte range of each file written.
- `hidden`: sessions set aside with `hide`, and when.
- `dirty`: sessions whose reconciliation is out of date.
- `meta`: schema version, price table hash, reconciliation version.

Rows are never deleted when Claude Code deletes a transcript: the file is
marked missing and its history stays. The schema only grows: new columns are
added to existing tables in place (`columns` in `store.go`). A database with
another schema version is renamed aside, never dropped, but that would lose
the history of deleted transcripts from the live index, so the version is not
bumped for additions.

## Indexing

Every command first brings the index up to date (`--no-index` skips it).

- A file whose size grew is read from its saved offset after the first 4 KiB
  hash matches. A changed hash or a smaller size means a rewrite: the file's
  rows are dropped and it is parsed again from the start.
- Only complete lines are consumed; a half-written last line waits for the
  next run.
- Active time is the sum of gaps under 5 minutes between consecutive lines of
  the main transcript, credited to the hour of the later line. The last
  timestamp is saved so the next run continues the sum.
- Files are parsed by a worker pool; one goroutine writes to SQLite.
- Write transactions take the database lock when they begin, and an update
  applies only if the file's stored offset still equals the offset its parse
  started from, so two ccli runs never add the same bytes twice.
- A file that cannot be read keeps its offset, is counted in the run's
  errors, and is retried next run; it does not stop the run.
- Each prompt line starts a turn; its file offset is stored, so a turn is
  read back as the byte range from its prompt to the next turn's. Responses,
  tool calls and spawns carry the turn in progress, which, with the replay
  flag and the prompt ids seen so far, carries across incremental parses.
  A turn's active time sums the gaps under 5 minutes between its lines,
  leaving out the pause before its prompt.
- Byte ranges in the `transfers` ledger mark lines copied in from another
  account: their responses and process snapshots are flagged as imported.
  Recording a range for a file the index has already read past resets the
  file, so it is read again with the range. An index run that planned before
  a range was recorded stores the file as needing a full re-read instead of
  as current (each update carries the newest ledger id its parse knew).
  A reset that leaves a file holding copied responses no other file holds
  (their own file lost them before the range was recorded) marks the copy's
  source, found through the ledger, to be read again so it takes them back.
- Ownership of a response held by several files: a file holding it outside
  any imported range wins; among those, the creation time of the file (macOS
  birth time, Linux `statx`) decides, and without one the file's first line
  time. The time stored for a file only moves earlier, so a file replaced by
  a copy written back into it keeps its place.
- Workflow journals and state files are read again only when their mtime
  changed.

### Reconciliation

Each applied update marks its session dirty in the same transaction, and every
run ends by reconciling the dirty sessions and clearing the marks, so a run
that stops early leaves the work to the next one. A price table change marks
every session.

For each process in a dirty session, the latest `cost-state` snapshot is
compared per model with the responses recorded between the process's start
and end. Where the snapshot shows more tokens, one `adjustment` response
carries the missing tokens, priced from the table at base rates, so a price
override applies to them as to every recorded call. Nothing is subtracted,
Claude Code's own cost figure is not used, and an unpriced model's adjustment
carries no cost.

A copied or forked transcript repeats its original's `cost-state` lines, so
one process can appear in several sessions. It is reconciled once, by the
session of a file holding its snapshot as its own (not copied in by ccli),
the earliest created of them, against the responses of
every session holding it, using the snapshot with the latest end. Sessions
sharing a process with a dirty one are reconciled with it.

## Pricing

An embedded table of list prices per model, Anthropic's for Claude and
OpenAI's for the models Codex runs (USD per million tokens: input, output,
cache read, 5-minute and 1-hour cache writes, fast-mode input and output, and
long-context rates above a prompt size), with overrides from `~/.config/ccli/prices.json`. Model IDs are
normalized: a `[1m]` suffix and a date suffix are dropped. A server-side web
search adds $0.01. Unknown models are
counted in tokens and listed as unpriced; their cost is left out, never zero.
Cost is computed at index time; when the table hash changes, every row is
repriced. Subscriptions are not billed per token, so costs are labeled "list
price".

## Query interface

Every command calls `query.Service`; the terminal renderer only formats its
results. `ccli web` will serve the same calls over HTTP.

```go
type Service interface {
    Accounts(ctx context.Context, f Filter) ([]AccountSummary, error)
    Sessions(ctx context.Context, f Filter) (SessionPage, error)
    Session(ctx context.Context, ref string) (*SessionDetail, error)
    Overview(ctx context.Context, f Filter) (*Overview, error)
}
```

The turn views add `Turns(ref)` (the turn list with each turn's cost and the
cost of the agents it started), `TurnRange(ref, from, to, all)` (turns read
from the transcript through `conv`, with their agents and runs), `Agent(ref,
agentID)` (a subagent's transcript), `RunAgents(ref, runID)` and
`Prompts(filter)` (typed prompts across sessions). `conv` turns a byte range
of a transcript into entries: prompts, text, thinking, tool calls paired
with their results, added context and notices.

Comparing and moving sessions between accounts works on the files themselves,
so it lives in `transfer` (`Compare`, `Plan`, `Apply`, `PlanFork`, `Fork`);
the CLI joins its results with `Sessions` for titles and records what was
written in the ledger.

Planned HTTP mapping for `ccli web`:

| Endpoint | Call |
| --- | --- |
| `GET /api/accounts?days=` | `Accounts` |
| `GET /api/sessions?account=&days=&project=&model=&q=&sort=&limit=` | `Sessions` |
| `GET /api/sessions/{id}` | `Session` |
| `GET /api/sessions/{id}/turns` | `Turns` |
| `GET /api/sessions/{id}/turns/{from}-{to}` | `TurnRange` |
| `GET /api/sessions/{id}/agents/{agent}` | `Agent` |
| `GET /api/prompts?account=&days=&q=&sort=&limit=` | `Prompts` |
| `GET /api/overview?account=&days=` | `Overview` |
| `GET /api/compare?a=&b=&project=` | `transfer.Compare` |
| `GET /api/quota?account=` | `quota.Reader` |
| `POST /api/transfer` (dry run unless `apply`) | `transfer.Plan` / `transfer.Apply` |

`Filter` is parsed from flags in the CLI and from query parameters on the web.
All result types are plain structs with JSON tags; `--json` prints them as is.

## Commands

```
ccli accounts                      accounts, identity, sessions, 30-day usage, live sessions
ccli sessions [-a acct] [--days N] [--project p] [--model m] [-q text] [--sort last|cost|tokens|active] [-n N] [--hidden]
ccli show <id>                     one session: totals, cost split, tree of subagents and runs, links, copies, resume command
ccli log <id> [N | N-M | last]     turns: the list with costs, or turns in full [--tools --thinking --context --raw --all --sort cost]
ccli log <id> --agent <a>          a subagent's transcript; --run <r> a workflow run's agents
ccli view [id]                     interactive browser: sessions, turns, agents
ccli prompts [-a acct] [--days N] [--project p] [-q text] [--sort last|cost|tokens|tools|duration|length] [-n N] [--hidden]
ccli stats [-a acct] [--days N]    overview: KPIs, daily chart, projects, models, hour x weekday, session shape, skills
ccli quota [-a acct]               subscription limits per account
ccli diff <a> <b> [--project p]    sessions only in a, only in b, identical, ahead, diverged
ccli copy <id>... --to acct        copy sessions (fast-forward only)
ccli move <id>... --to acct        copy, then archive and remove the source
ccli copy|move --from a --to b [--project p] [--include missing,ahead] [--days N]   batch
ccli fork <id> --to acct [--from acct]   copy under a new session id
ccli resume <id> [-a acct [--sync]] [--print]   run claude --resume (or codex resume) with the right account and cwd
ccli hide <id>... / unhide <id>...   leave sessions out of lists, or bring them back
ccli index [--rebuild]
```

Global flags: `--json`, `--no-index`, `--color auto|always|never`.

## Hiding sessions

`hide` sets sessions aside so lists stop showing them. It writes one row per
session id to the `hidden` table and nothing else: transcripts stay where
Claude Code put them, so `claude --resume` and `ccli resume` keep working.

- A session counts as hidden while its last activity is no later than its
  mark, so a session that is used again shows again without `unhide`.
- The mark is per session id, so copies of the session in other accounts are
  hidden with it.
- `sessions`, `prompts` and the sessions list of `view` leave hidden sessions
  out (`--hidden`, or `H` in `view`, lists only them). Totals in `stats`,
  `quota` and `accounts` still count them, and `show` and `log` still open
  them by id.
- A session a Claude Code process is running is refused: its next line would
  show it again.

## Copy and move

- A session that a live process is running (any account's `sessions/<pid>.json`)
  is refused.
- If the target has no copy, the session is copied. If the target's
  transcript is a byte prefix of the source, it is extended (fast-forward).
  If the target is newer, or the two diverged, the session is refused;
  `fork` continues one side as a new session instead.
- A session is located by its transcript's project folder. Its folders
  under other project keys (made when its working folder changed) are copied,
  verified and archived with it.
- Every transcript file written (`.jsonl`) is recorded in the ledger with
  the byte range that came from the source before the file is put in place
  (a temporary file, then a rename or, for `fork`, a link that never
  replaces an existing file). If putting it in place fails, the entry is
  taken back. Files already written stay recorded when a later step fails.
- `fork` copies the complete lines of the transcript and the session folders
  under a new random id, rewriting only the `sessionId` and `session_id`
  fields. A running session can be forked; its partial last line is left
  out. The whole copy is recorded as imported, so the fork starts at $0.
- `resume -a <acct>` resumes the session in that account. If another
  account holds a newer copy (this one is a prefix of it), `--sync` first
  fast-forwards it the way `copy` does; without `--sync` nothing is written.
  A copy elsewhere that diverged from this account's own does not stop the
  resume (a note names it and the `fork` that would continue it). Diverged
  copies are never merged; when two copies are both newer than this one and
  diverged from each other, `--sync` refuses and names them.
- Accounts that share these folders through a symlink are refused, and so is
  a target that holds the session under another project folder.
- The transcript is written to a temporary file and renamed into place.
  Subagent, tool-result, scratchpad and file-history folders are copied file
  by file without overwriting a file that differs. No write passes through a
  symlinked folder in the target. Copies get their own file times, so a copy
  is always newer than its original (the index relies on that).
- `move` moves the source files into `$XDG_DATA_HOME/ccli/archive/<time>/`
  instead of deleting them, and only after every file is verified in the
  target byte for byte; a session folder holding a symlink or another file
  `copy` cannot reproduce is refused. The transcript is archived last, so an
  interrupted move can be run again.
- Usage stays with the account that ran it: after a move the session's past
  cost remains in the source account's totals.
- `copy` and `move` print the plan and change nothing unless `--apply` is
  given, for named sessions and batches alike.
- The project memory folder, the live registry and every config file stay
  untouched.

## Quota

The only documented source of subscription limits is the status line input
(code.claude.com/docs/en/statusline): `rate_limits.five_hour` and
`rate_limits.seven_day`, each with `used_percentage` and `resets_at` (epoch
seconds), for Pro and Max accounts, present after the session's first API
response. `/usage` is not documented as available under `claude -p`, so ccli
does not run it, and it never calls a usage endpoint with stored credentials
(the terms forbid third-party tools collecting or intermediating Claude.ai
credentials).

- `ccli statusline -- <command> [args]` is an opt-in wrapper for the user's
  status line. It reads the JSON from stdin, saves `rate_limits` for the
  account (found from `transcript_path`) to
  `$XDG_DATA_HOME/ccli/quota/<account>.json`, runs the wrapped command with
  the same stdin, and prints its output unchanged. ccli never edits
  `settings.json`; the user changes their `statusLine.command` to use it.
- A status line program can keep its own copy: ccline caches the 5-hour and
  weekly percentages and the weekly reset time from Claude's usage API in
  `<config dir>/ccline/.api_usage_cache.json`. ccli reads those numbers (it
  never reads the token ccline used). For each window the most recent of the
  two sources wins; a reading without a reset time borrows the other
  source's when that is still ahead.
- Codex writes its own limits to its rollouts (see Codex): the index saves
  the latest reading per Codex account as that account's snapshot, marked
  as coming from Codex's logs, with the plan it names.
- `ccli quota [-a acct]` shows, per account, each window's percentage, reset
  time and source, and the tokens and list-price cost ccli counted since the
  window started (its reset time minus 5 hours or 7 days). A window whose
  reset time has passed is shown as reset, with whether anything was used
  here since.

## Format stability

Claude Code documents the transcript format as internal and subject to change.
Fields the docs do not name (`agent-*.meta.json`, `continued-in`,
`subagents/workflows/`, `cost-state`) are read when present and ignored when
absent, so a format change degrades a view instead of breaking indexing. The
parser version is stored per file; bumping it re-reads everything.
