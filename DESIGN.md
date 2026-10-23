# ccli design

`ccli` reports on Claude Code, Codex, Pi, and Cursor sessions across several
local accounts, and moves Claude Code sessions between accounts. An account
is one Claude Code config dir: `~/.claude` (account `default`) or
`~/.claude-<name>` (account `<name>`), selected with `CLAUDE_CONFIG_DIR`; or
one Codex home: `~/.codex` (account `codex`) or `~/.codex-<name>` (account
`codex-<name>`), selected with `CODEX_HOME`; or one Pi agent dir:
`~/.pi/agent` (account `pi`), selected with `PI_CODING_AGENT_DIR`; or the
Cursor CLI data dir `~/.cursor` (account `cursor`); or the Cursor IDE user
dir (account `cursor-ide`). `CCLI_CONFIG_DIRS`, `CCLI_CODEX_DIRS`,
`CCLI_PI_DIRS`, `CCLI_CURSOR_DIRS` and `CCLI_CURSOR_IDE_DIRS` replace each
search. Cursor CLI transcripts and IDE composers stay in different accounts
even when an id appears in both.

## Boundaries

ccli is a reader. The only things it writes are:

- its own database, `$XDG_DATA_HOME/ccli/ccli.db` (default
  `~/.local/share/ccli/ccli.db`, override `CCLI_DB`), which also holds
  the marks `hide` sets and the tags `tag` gives;
- its quota readings under `$XDG_DATA_HOME/ccli/quota/` (the status line
  wrapper's and the latest Codex log reading in `<account>.json`, live check
  answers in `live/<account>.json`), and `$XDG_DATA_HOME/ccli/tool-runs/`,
  the empty folder `quota` starts the tools in;
- for `copy`, `move`, `fork` and `resume -a` only: transcript files of
  the session in the target account (`projects/<key>/<id>.jsonl`,
  `projects/<any key>/<id>/`, `file-history/<id>/`; a fork writes them under
  its new id), and for `move`, an archive of the source under
  `$XDG_DATA_HOME/ccli/archive/` before the source files are removed;
- for `clean` only: an empty Claude Code session's files, moved from its
  account into `$XDG_DATA_HOME/ccli/archive/clean/<stamp>/<account>/` with
  a `manifest.json` of the run, and moved back by `clean --undo`.

In a Codex home it reads only the rollout files under `sessions/` and
`archived_sessions/`, and writes nothing itself (a `quota` live check starts
Codex's app server, which writes to Codex's own files; see below). It never opens `auth.json` (which
holds the credentials and the signed-in identity) or `config.toml`, and never
opens Codex's SQLite databases: opening one read-only can still create a
`-shm` file next to it. Cursor's databases are the exception, opened so that
no sidecar is ever created (see below).

It never reads or writes Claude Code or Codex credentials (Keychain,
`.credentials.json`, `auth.json`), never
writes `.claude.json`, `settings*.json`, `sessions/` (the live-process
registry), `teams/`, `jobs/`, `session-env/`, or anything else in a config dir.
From `.claude.json` it reads only `oauthAccount.emailAddress`,
`organizationName` and `billingType`, to label an account. From
`<dir>/ccline/.api_usage_cache.json`, when a status line program left one,
it reads only the usage percentages and times. ccli itself never sends a
request to a model.

Cursor is the one exception to both rules. Cursor transcripts carry no
model, token or cost figures, so ccli reads the access token the Cursor IDE
stored in `state.vscdb` (`cursorAuth/accessToken`) and sends it to
`api2.cursor.sh`, and only there: redirects are refused. It asks only when a
command shows Cursor figures for an account you selected. The token is kept
in memory for that request and is never written to the index, the cache, an
error or debug output (see Cursor below).

The one exception to "reader" is `quota` without `--offline`: it starts each
account's own tool once to ask for the current limits (see Quota). Claude
Code and Codex answer with their own login; ccli reads none of their
credentials.
Starting them does what any start does to their own files: Claude Code
refreshes cache entries in `.claude.json`, and Codex writes to its databases
and plugin cache. They run in `$XDG_DATA_HOME/ccli/tool-runs/`, an empty
folder of ccli's own, and ccli saves each answer as the account's quota
snapshot.

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
`session_meta`. Codex archives a thread by moving its rollout to
`archived_sessions/`, and unarchives it by moving it back; the index moves
the file's row with it, so nothing is read or counted twice. The rules below
were checked against 2,022 rollouts written by CLI 0.114 to 0.150.

- **Sessions and agents**: a thread whose `session_meta.source` is
  `{subagent: ...}` was started by another thread. It is indexed as a
  subagent of the user's thread it descends from (`session_id`, or following
  `parent_thread_id` in older files), nested under the agent that spawned it.
  The index reads line 0 of a file once, when it first sees it.
- **Turns**: a turn starts at `event_msg/task_started`. A person's prompt is
  `event_msg/user_message` up to CLI 0.146, an `item_completed` item of type
  `UserMessage` since, and in some exec versions only the `response_item`
  user message. The first prompt in a turn opens it; a later one was sent
  while the model worked. A prompt outside any turn opens one where it
  stands, unless it repeats the prompt just taken (Codex can write one
  prompt both as an event and as a message). Text Codex injects (environment, AGENTS.md,
  permissions, notifications) is recognised by its per-part kinds
  (`content_item_kinds`, since 2026-08-27) or by its opening tag.
- **Tokens**: `token_count` carries the running total of the process that
  wrote it, and that call's own usage (`last_token_usage`). Cached input is
  part of input and reasoning part of output. A count is new usage when it
  moves one of the file's streams on by exactly its own usage; one equal to a
  stream's total repeats an earlier count or follows a compaction (whose last
  usage is a context estimate) and adds nothing; one that moves a stream on
  by more, on every field, follows counts that were not written, and the
  whole difference counts. A count whose total is its own usage starts a
  stream (a resume restarts the counter, and two processes can write one
  file), and so does any other count. Up to eight streams are kept per
  file; past that, the one idle longest is dropped. The streams' totals
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
  context. A count written before any line named a model is given the
  first one named, also when an earlier run already stored it; the counts of
  a file that names none are kept in tokens only. Prices are OpenAI's list prices; models Codex runs through other
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
  home other than `~/.codex`. `codex resume` finds only threads that are not
  archived, so for an archived one ccli prints the `codex unarchive <id>`
  command instead, and does not run it.

## Pi, Cursor CLI, Cursor IDE

These three are readers in the same way Codex is. `diff`, `copy`, `move`,
`fork` and `resume -a` stay Claude Code only. None of them has a live-process
registry. Pi is not asked for quota. Cursor's quota is the signed-in
account's billing cycle.

Pi sessions are `<agent>/sessions/--<cwd>--/<time>_<id>.jsonl` (format
version 3, the tree of `id` / `parentId`). The conversation shown is the path
from the latest leaf to the root. Every assistant reply is counted, including
replies on a branch that was left, and Pi's own USD `usage.cost` is kept
(`responses.fixed_cost`) instead of being replaced by the list-price table.
`resume` runs `pi --session <id>`, with `PI_CODING_AGENT_DIR` set when the
agent dir is not `~/.pi/agent`. A changed file is read again from the start.

Cursor CLI transcripts are
`~/.cursor/projects/<slug>/agent-transcripts/<id>/<id>.jsonl`, with subagents
in `subagents/`. The lines have no model, token or timestamp fields. The
working directory, title and clock come from `~/.cursor/chats/<hash>/<id>/meta.json`
when that sidecar exists; otherwise the project is the transcript's project
directory. A typed prompt is the text inside `<user_query>`. `resume` runs
`agent --resume <id>`. Model, token and cost figures are not in the file.
They are joined from Cursor's usage page by conversation id (the same id as
the local session, and a subagent's own id rolled up to its parent) and
cached in `cursor-usage.json` for 15 minutes. Each request to that service
takes seconds, so the event list comes in pages of 1000 (the most it
accepts), the pages after the first four at a time, and the billing cycle
is asked for alongside it. A full read covers a year and runs once a week;
in between, a read asks only from 48 hours before the newest cached event
and replaces the cached events from then on, which also picks up late
changes to recent ones. The access token Cursor stored
for the IDE login is sent only to `api2.cursor.sh`. It is not written to the
index or the cache. A failed read is saved with its time and not retried for
five minutes, and a part that failed (the cycle or the event list) keeps
what the cache had for it. A Cursor IDE composer is read again when its
header's `lastUpdatedAt`, its bubble count or its newest bubble row changes,
because Cursor writes a reply's bubbles minutes after it moves
`lastUpdatedAt`. A Cursor CLI chat is read again when its transcript,
`meta.json` or `store.db` changes.

Cursor IDE composers live in `state.vscdb` (`composerHeaders`,
`cursorDiskKV` bubbles). Cursor keeps it in WAL mode, and recent commits may
sit only in `state.vscdb-wal`. While Cursor has it open, its `-wal` and
`-shm` both exist, and ccli opens a plain read-only connection that reads the
WAL through Cursor's own `-shm`. Otherwise every commit is in the main file,
and ccli opens it `immutable=1`. Neither creates a file beside the database.
The same rule applies to Cursor CLI's `store.db`. A subagent
is filed under `subagentInfo.rootParentConversationId`. Some bubbles carry
`tokenCount`; there is no price. There is no resume command: opening an IDE
chat from the CLI can rewrite a transcript, so `show` says to open it from
the Agents window. A composer is re-read when its `lastUpdatedAt` changes.

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
- `snapshots`: every `cost-state` line of each file, per running total
  (its `startTime`) and model, with its offset and whether ccli copied it
  in. It replaced `processes`, which kept only a file's newest snapshot;
  opening an older index re-reads the files that had one, once, and leaves
  `processes` empty.
- `runs`: workflow run event counts from `journal.jsonl` and what its state
  file says, with each file's mtime so unchanged files are not read again.
- `transfers`: the ledger of copies ccli made: time, mode (copy, move,
  fork), session, new session id for a fork, source and target account, and
  the byte range of each file written.
- `hidden`: sessions set aside with `hide`, and when.
- `tags`: labels given to sessions with `tag`, and when. (A `titles` table
  from the short-lived `rename` is moved into it once, then dropped.)
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

Claude Code keeps one running total per session, named by its
`startTime`, and carries it on across resumes: in the same account, in
another account once ccli copied the session there, in a session Claude Code
continued into, and in both branches of a fork. Each `cost-state` line is the
total so far. A session's share of a total is the growth between
consecutive snapshots in its own transcript, counted at each snapshot that is
its own: a snapshot ccli copied in only moves the base for the next one. A
snapshot several files hold as their own (a transcript copied by hand, the
history a native fork or continuation repeats) counts once, in the file
created first; one that only copies hold (the original is not indexed)
counts in the earliest copy. In a file created after the total began
elsewhere, a first snapshot with nothing before it in the file only sets the
base: it holds the parent's total up to an unknown point, so counting it
could count the parent twice. What such a session spent without writing it
down before its first snapshot is left out. A total that dips does not lower
the base. `processes` rows of transcripts already gone from disk move over
as one snapshot each, since the file cannot be read again.

For each total in a dirty session, that growth is compared per model with the
session's own responses in the same time windows (from the snapshot before to
the one counted). Where the growth shows more tokens, one `adjustment`
response carries the missing tokens, priced from the table at base rates, so
a price override applies to them as to every recorded call. Nothing is
subtracted, Claude Code's own cost figure is not used, and an unpriced
model's adjustment carries no cost. Sessions sharing a total with a dirty one
are reconciled with it.

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

A session id can be in several accounts: each is a copy, its own
transcript, and the index keys sessions by account and id. Commands that
read one session (`show`, `log`, `export`, `view`) take `<account>:<id>` or
`-a`, and otherwise use the copy with the latest activity and name the
others on stderr (`query.DB.CopiesOf`). `Turns` marks each turn whose prompt
lies in a byte range the ledger says ccli copied in (`TurnRow.CopiedFrom`:
the source account, or `account:id` for a fork). Marks that belong to the
id (hide, tag) apply to every copy.

Every command reads through `query.DB`; the terminal renderer only formats
its results, and every result serializes as JSON. The main calls are
`Accounts(filter)`, `Sessions(filter)`, `Session(ref)`, `Overview(filter)`
and `Projects(filter)`. There is no shared interface: a consumer that wants
one declares what it uses where it uses it, as the TUI does with its
`Source`, and a planned `ccli web` would do the same.

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
| `GET /api/projects?dir=&account=&tool=&days=&depth=&sort=` | `Projects` |
| `GET /api/compare?a=&b=&project=` | `transfer.Compare` |
| `GET /api/quota?account=` | `quota.Reader` |
| `POST /api/transfer` (dry run unless `apply`) | `transfer.Plan` / `transfer.Apply` |

`Filter` is parsed from flags in the CLI and from query parameters on the web.
`Filter.Dirs` holds a folder in each form a recorded `cwd` may take: the
absolute path as given and the same path with symlinks resolved in its
longest existing prefix, since the agents record physical paths (`/tmp` is
`/private/tmp` on macOS) and a deleted folder still has sessions. A session
matches when its first recorded `cwd` is one of them or below it, compared by
whole path segments. `Filter.Tools` matches the agent of the session's
account. `Projects` groups the matching sessions by `cwd`, or with `Depth` by
the first levels below the matched folder; hidden sessions count, as in every
total.
All result types are plain structs with JSON tags; `--json` prints them as is.

## Commands

```
ccli accounts                      accounts, identity, sessions, 30-day usage, live sessions
ccli sessions [-a acct] [--days N] [--dir folder] [--tool claude|codex] [--project p] [--model m] [-q text] [--sort last|cost|tokens|active] [-n N] [--hidden]
ccli projects [folder] [--depth N] [--sort last|cost|sessions|path]   folders sessions started in, at or below folder (default .)
ccli show <id>                     one session: totals, cost split, tree of subagents and runs, links, copies, resume command
ccli log <id> [N | N-M | last]     turns: the list with costs, or turns in full [--tools --thinking --context --raw --all --sort cost]
ccli log <id> --agent <a>          a subagent's transcript; --run <r> a workflow run's agents
ccli view [id] [--dir folder] [--tool t]   interactive browser: sessions, turns, agents
ccli prompts [-a acct] [--days N] [--dir folder] [--tool t] [--project p] [-q text] [--sort last|cost|tokens|tools|duration|length] [-n N] [--hidden]
ccli stats [-a acct] [--days N]    overview: KPIs, daily chart, projects, models, hour x weekday, session shape, skills
ccli quota [-a acct] [--offline]   subscription limits per account, asked of the tools now
ccli diff <a> <b> [--project p]    sessions only in a, only in b, identical, ahead, diverged
ccli copy <id>... --to acct        copy sessions (fast-forward only)
ccli move <id>... --to acct        copy, then archive and remove the source
ccli copy|move --from a --to b [--project p] [--include missing,ahead] [--days N]   batch
ccli fork <id> -a acct [--turn N] [--from acct]   copy under a new session id, whole or through turn N
ccli resume <id> [-a acct [--no-sync]] [--print]   run claude --resume (or codex resume) with the right account and cwd
ccli hide <id>... / unhide <id>...   leave sessions out of lists, or bring them back
ccli clean [-a acct] [--dir folder] [--tool t] [--unanswered] [--idle 1h] [--apply]   set aside empty sessions
ccli clean --undo <stamp>          put back what one clean run archived and hid
ccli tag <id> <tag> / --clear       label a session in ccli, or remove the label
ccli index [--rebuild]
```

Every command takes `--json`, `--no-index` and `--color auto|always|never`
after its name (`ccli accounts --json`). Only `--debug` and `--verbose` may
also come before the command.

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

## Cleaning empty sessions

`clean` finds sessions that hold nothing and sets them aside once you agree.

- A session is empty when it has no prompt, no reply, no tool call, no
  tokens or cost, no subagent or workflow run, and no name from a person or
  the tool, is not running, and was last active at least `--idle` (1h) ago.
  A name means there is something ccli cannot see, such as a Cursor chat
  whose messages it cannot read. `--unanswered` adds sessions whose prompts
  never got a reply; it is opt in because those hold text a person typed.
  Sessions a person hid or tagged are left alone, and so are unused Cursor
  IDE drafts, which lists already leave out. Billed Cursor usage is read
  first and counts as content. A Codex, Pi or Cursor session with no
  recorded time is not taken as idle; for Claude Code the transcript's own
  time decides.
- A transcript with no line that names its folder takes the folder another
  session under the same project key recorded, so `--dir` finds it.
- Without `--apply`, `clean` prints the plan and asks on a terminal. With no
  terminal, or with `--json`, it changes nothing. `--no-index` is refused:
  the plan must come from what the transcripts hold now.
- A Claude Code session moves to the archive with the same path checks as
  `move`, transcript last, and its rows leave the index. A Claude Code
  session whose transcript is already gone only leaves the index. Codex, Pi
  and Cursor sessions are hidden, because ccli does not write their homes. A
  hidden session that is used again shows again by itself.
- Just before each change, the session is checked again: one that is
  running, or whose transcript moved or changed size or time since the plan,
  is left as it was, with a note. A failure or an interrupt stops the run,
  and the sessions after it are marked not done.
- The run claims its folder, named for the second it started, with an
  exclusive mkdir, so a second run in the same second is refused. Its
  `manifest.json` lists the plan before the first change and is rewritten
  after each session, marking which are done. A run that changed nothing
  removes its folder.
- `--undo <stamp>` works from the folder, so a run that stopped part way can
  be undone even without its record. It moves the transcripts back with a
  link where it can, so a path the account has again is never replaced and
  stays in the archive, as does anything that is not a session. It unhides
  the sessions the record marks as hidden, and updates the index, even when
  one account fails.

## Tagging sessions

`tag` gives a session a short label kept in the `tags` table, one row per
session id, and writes nothing else. The session's title is untouched: a tag
is a separate field (`tag` in JSON) shown in its own column.

- Lists add a TAG column only when a session on the page has a tag. The
  viewer pads the account and tag columns to the widest value on the page,
  so titles line up, and cuts a tag longer than 12 cells there; `show`
  prints it whole. In a narrow list pane the tag column shrinks so the
  title keeps 16 cells, and is left out when fewer than 4 remain.
- A tag is at most 40 characters (the viewer's editor stops there).
  Control characters are dropped and runs of whitespace become one space,
  so a pasted escape sequence cannot color or misalign a table.
- Search (`-q`, `/` in `view`) matches the tag as well as the title.
- It is per session id, so it labels the session's copies in other accounts,
  and it works the same for Codex sessions and running sessions.
- `--clear`, or an empty tag in `view`, removes it.

The first version of this feature (`rename`) replaced the title in ccli's
lists instead. A title that silently differs from the one Claude Code shows
was confusing, so it became a label next to the title. Running Claude Code's
own `/rename` (`claude -p --resume <id> "/rename ..."`) was rejected
throughout: it starts a Claude Code process (with the account's hooks and
MCP servers), appends several lines to the transcript and moves the session's
last activity, which reorders lists and brings a hidden session back. A
title set with `/rename` in Claude Code (a `custom-title` line) is read as
before. `/rename` also writes an `agent-name` line; `agentName` counts as a
teammate's name only on lines that also carry `teamName`.

A `titles` table is moved into `tags` inside one transaction that checks
for the table after taking the write lock, so two processes opening the
database at once do not both try. Moved names are cut to 40 characters, and
a session that already has a tag keeps it. Going back to a build that still
has `rename` recreates an empty `titles` table; names given there are moved
on the next open, except for sessions tagged in the meantime.

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
  `--turn N` (N from 1) ends the copy at the offset of the prompt of the
  turn after N, taken from the index and checked against the file: the
  byte before it ends a line, and the line there starts a turn by the
  parser's own rule (`transcript.StartsTurn`, so a tool result, meta line,
  compaction summary or sidechain message does not pass). Through the last
  turn, the copy ends where the index stopped reading (`files.offset`), so a
  turn written since is not taken along. It leaves out the subagent and
  workflow transcripts the index ties to a later turn (or, when the index
  could not place one, that started after the cut), with their
  `.meta.json`; every one of them must be in the session's folders. `Fork`
  repeats these checks against the files just before it writes. Files
  outside the transcript (file-history, tool results) are copied whole.
  Only the conversation is cut; nothing in the working folder changes.
- `resume -a <acct>` resumes the session in that account. If another
  account holds a newer copy (this one is a prefix of it), it first
  fast-forwards it the way `copy` does; with `--no-sync` nothing is written
  and the resume stops. A session a process is running anywhere is not
  copied; the error names the account it runs in.
  A copy elsewhere that diverged from this account's own does not stop the
  resume (a note names it and the `fork` that would continue it). Diverged
  copies are never merged; when two copies are both newer than this one and
  diverged from each other, the sync refuses and names them.
- Accounts that share these folders through a symlink are refused, and so is
  a target that holds the session under another project folder.
- A new transcript is written to a temporary file and renamed into place.
  A fast-forward appends the missing bytes to the target in place, after
  checking that the target has not changed since the plan, so it stays the
  same file with its creation time and only the new bytes are written.
  Subagent, tool-result, scratchpad and file-history folders are copied file
  by file without overwriting a file that differs, with one exception: a
  `/rewind` backup (`file-history/<id>/<hash>@vN`). Claude Code numbers those
  per process and reuses a name after a resume, replacing its own older
  backup, so a backup of the same name with other content is expected. The
  newer of the two (by modification time) takes the name; an older target
  backup moves to `archive/<time>/<target account>/` first, and a target
  backup that is as new or newer is left alone and reported. Without an
  archive folder, nothing is replaced. Copied backups keep their source's
  modification time, so a later copy compares the times Claude Code wrote
  them, not when ccli copied them. No write passes through a
  symlinked folder in the target. A new copy gets its own file times, so it
  is always newer than its original (the index relies on that when no ledger
  entry says which is which).
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

For Claude Code and Codex, ccli never calls a usage endpoint with stored
credentials (the terms forbid third-party tools collecting or intermediating
Claude.ai credentials), and it does not run `/usage`, which is not documented
as available under `claude -p`. It asks the tools instead, and keeps passive
sources as the fallback. Cursor's billing cycle is read with the IDE's
stored login, as described above.

Live check (`quota` without `--offline`), one per account, in parallel,
while the index is brought up to date:

- Claude Code: `claude -p --input-format stream-json --output-format
  stream-json --verbose --setting-sources local --strict-mcp-config
  --no-session-persistence`, sent one control request,
  `{"subtype":"get_usage","skip_behaviors":true}`. This is the request behind
  `/usage` in Claude Code's SDK protocol, which Claude Code marks
  experimental. The answer has `rate_limits.five_hour`, `seven_day`,
  `seven_day_opus`, `seven_day_sonnet` and `model_scoped[]` (each a
  percentage and an ISO reset time), `extra_usage`, and `subscription_type`.
  `--setting-sources local` in an empty folder leaves out the user's
  settings, so no hook, plugin or settings env applies; `--strict-mcp-config`
  with no config starts no MCP server; no prompt is sent and no transcript is
  written. It takes about 3 to 5 seconds, most of it Claude Code starting.
- Codex: `codex app-server`, sent `initialize`, `initialized` and
  `account/rateLimits/read`. Windows are placed by `windowDurationMins` (300
  and 10080), as in the logs; other lengths and other buckets
  (`rateLimitsByLimitId`) become named extra windows.
- Both get the user's environment minus `TMUX`, `TMUX_PANE`, `CLAUDECODE` and
  `CLAUDE_CODE_ENTRYPOINT` (so neither takes itself for a nested run), with
  `CLAUDE_CONFIG_DIR` or `CODEX_HOME` set for an account other than the
  default dir and unset for it. The tool's login is used as it is.
- Each tool runs in its own process group; a timeout kills the group, so
  nothing it started outlives the check. The answer is timed when it
  arrives and saved to `quota/live/<account>.json`, apart from the passive
  readings, so a later status line write cannot drop the windows and plan
  only a live check reports. Parts of an answer are decoded one by one: a
  field of the wrong type costs that field, and a window without a
  percentage is left out, never shown as 0%.
- A check that fails (not installed, not signed in, an API-key login, no
  answer in 30 seconds) is reported with the tool's reason, and the account
  falls back to its saved readings. `--offline` makes no check.
- For each window the newest reading among all sources wins (a live answer,
  the status line, ccline, Codex's logs); a reading without a reset time
  borrows the newest other reading's that is still ahead of it. A live
  answer lists every window the account has, so it supersedes older
  readings: a window it leaves out is not filled from them.

Passive sources, kept for `--offline` and as the fallback:

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
Output: `ccli quota [-a acct] [--offline]` shows, per account, each window's
percentage, reset time and source (live from the tool, or which saved
source and how old), the extra windows and extra use a live check reports,
and the tokens and list-price cost ccli counted since the 5-hour and weekly
windows started (the reset time minus 5 hours or 7 days). A saved window
whose reset time has passed is shown as reset, with whether anything was
used here since.

## Format stability

Claude Code documents the transcript format as internal and subject to change.
Fields the docs do not name (`agent-*.meta.json`, `continued-in`,
`subagents/workflows/`, `cost-state`) are read when present and ignored when
absent, so a format change degrades a view instead of breaking indexing. The
parser version is stored per file; bumping it re-reads everything.
