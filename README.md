# muster

**A local coordination bus for coding agents.**

`muster` lets coding-agent sessions in separate terminals send messages and hand
tasks to each other. Any agent that can register an MCP server can join the bus —
Claude Code, OpenAI Codex, Cursor Agent, and pi are supported. Everything runs
over a local unix socket with state in a local SQLite file; muster itself never
calls a model.

- **Messages and tasks between sessions.** One agent posts a "review this branch"
  task; a standing session in another terminal claims it, works it, and replies.
- **One static Go binary**, four modes: a lazy-started daemon, a stdio MCP server
  each agent registers, a channel carrier that pushes wakes into idle sessions,
  and a CLI for you.
- **tmux-native wake** — mail sets a mailbox flag (`📬<count>`) on the recipient's
  tmux session; `muster nudge` is the only thing that types into a pane.

Landing page: **muster.tools**

## Status

**v0.18.0** — muster is now source-available under the **Business Source License
1.1** (free for any organization under 25 people; each release converts to
Apache-2.0 three years after it ships; versions before v0.18.0 stay MIT). The
coordination core is mature: **channel mode** (`muster channel`) pushes wakes
into idle sessions over MCP; **standing orders** are a keyed, retractable
per-project convention that new sessions read once instead of inheriting a live
broadcast backlog; a **broadcast storm guard** gates every broadcast behind a
blast-radius confirmation and routes replies to the originator; and `muster
station` is the full-screen operator TUI. Messages and tasks carry an intent
(`fyi`, `reply-requested`, `action-requested`); a departed agent is tombstoned,
not deleted, so its history stays visible and re-registering its alias revives
it; and target resolution is canonical — an unknown `send`/`task_create` target
(CLI or MCP) fails loudly instead of addressing a thread to nobody. See
[releases](https://github.com/subaud-tools/muster/releases) for the full changelog.

## Setup

```bash
# 1. install the binary (macOS or Linux; on Windows use WSL2)
curl -fsSL https://muster.tools/install.sh | sh
#    (or build from source with Go 1.26+: go install github.com/subaud-tools/muster/cmd/muster@latest)

# 2. register the MCP server with each agent
claude mcp add muster -s user -- muster mcp     # Claude Code: the bus tools
claude plugin marketplace add subaud-tools/muster   # Claude Code: mail starts a turn
claude plugin install muster@muster
#    (or run `muster setup`, which does all of the Claude Code steps)
codex mcp add muster -- muster mcp              # Codex
# Cursor Agent: add the mcp.json block shown below, then:
agent mcp enable muster
# (any other MCP client: point it at `muster mcp` over stdio)

# 3. in each session, have the agent call current_agent first;
#    call register_agent only when registered is false
#    (or add that instruction to your project's CLAUDE.md / AGENTS.md)
```

That's a working bus. Two optional layers, both in [`contrib/`](contrib/):

- **See the mailbox** — two lines of tmux config render `📬<count>` on tabs with
  unread mail ([`contrib/tmux-mailbox.conf`](contrib/tmux-mailbox.conf)).
- **Automate the lifecycle** — session hooks (`muster hook <event> <model>`)
  auto-register agents on start and have them drain their own inbox at turn
  end ([config for all three harnesses in `contrib/`](contrib/README.md)).

## MCP mode

`muster mcp` runs muster as an MCP server over stdio, exposing the bus as tools
any MCP client (Claude Code, Codex, Cursor Agent) can call. Register it once per
tool:

```bash
# Claude Code
claude mcp add muster -s user -- muster mcp
# Codex
codex mcp add muster -- muster mcp
# Cursor Agent: add this to .cursor/mcp.json (or ~/.cursor/mcp.json)
```

```json
{
  "mcpServers": {
    "muster": {
      "command": "muster",
      "args": ["mcp"]
    }
  }
}
```

## Channel mode

`muster channel` is an MCP **channel** carrier (Claude Code's `claude/channel` convention; pi via its `pi-channels` extension). Registered beside `muster mcp`, it tails the bus journal for the session's aliases and pushes a compact envelope — intent, sender, thread, subject, never the body — into the session the moment mail lands. An idle agent wakes and answers with `get_thread`/`reply`; nobody types. The 📬 badge, the Stop-hook drain and `muster nudge` are unchanged fallbacks. `MUSTER_CHANNEL_INTERVAL` (default `1s`) tunes the poll cadence; `muster_channel_status` (the carrier's one tool) reports what it is attached to.

Each push is an envelope line, then a line containing only `---`, then guidance for that push — the rule for an event travels with the event (what an `fyi` forbids, what an `action-requested` obliges) instead of living in a handshake blob thousands of tokens back. The handshake keeps only the durable core. A batch names the strictest intent present and lists each thread up to `MUSTER_CHANNEL_MAX_LISTED` (default 5); above that it collapses to a count and routes to `get_inbox`. Clients that split on the separator (pi-channels) can dedupe identical guidance across a batch; clients that do not (Claude Code) render it as-is.

Claude Code hears mail through the muster plugin: install it with `claude plugin marketplace add subaud-tools/muster` and `claude plugin install muster@muster`, or let `muster setup` enable it.

The plugin's mod runs `muster watch` for the session and starts a turn with each push, in a session started with plain `claude`; keep `muster mcp` registered for the bus tools.

Without the plugin, the badge and Stop-hook drain still deliver the mail, but an idle session doesn't wake for it.

```bash
agent mcp enable muster
```

For unattended Cursor Agent use, allow `Mcp(muster:*)` in the
`permissions.allow` list in `~/.cursor/cli-config.json`.

The tools, by what they do:

| Group | Tools | Notes |
|---|---|---|
| Identity | `current_agent`, `register_agent`, `deregister_agent`, `list_agents`, `get_status` | discover your proven identity first; join or leave the bus; see who's on it; inspect unread counts without marking mail read |
| Conversation | `send_message`, `reply`, `get_inbox`, `get_thread` | a **message** is a plain thread — no state, just an exchange |
| Work | `list_tasks`, `task_create`, `task_claim`, `task_transition` | discover tasks with exact filters, then create or advance them through `open → claimed → needs_info \| blocked → completed \| declined \| cancelled`. Claiming is atomic |
| Shared state | `kv_set`, `kv_get`, `kv_list`, `kv_delete` | a small key/value scratchpad; mutations are attributed to the proven caller rather than a client-supplied alias |

The MCP server talks to the local daemon (auto-started on first use). Start an
identity-dependent workflow with `current_agent`. When it reports
`registered: false`, call `register_agent`; otherwise use the returned
`agent.alias` rather than guessing your name. `deregister_agent` takes no target
and tombstones every live alias proven to belong to the calling session while
preserving its history and read state. `get_status` returns side-effect-free unread
and action-required counts for all aliases, or filters by one exact alias.

`kv_list` returns the complete blackboard in key order and accepts an optional
literal prefix; `kv_delete` is idempotent. `list_tasks` composes exact project,
status, creator, and target filters. It defaults to the 100 most recently
updated nonterminal tasks, accepts limits through 500, and sets `truncated`
when more matching work exists.

> Note: stdout is the MCP channel in this mode; muster writes all diagnostics to
> stderr.

## CLI

Agents coordinate through the MCP tools above — the CLI is for **you** (and for
hooks): commands you run from any shell to watch the bus and step in when you
want to (they auto-start the daemon):

```bash
muster agents                              # who's registered
muster status --json                       # side-effect-free per-alias unread counts (marks nothing read)
muster inbox <alias>                       # an agent's threads — addressed to it or started by it
muster tasks <alias>                       # just the tasks for an agent
muster events                              # the bus event log: every mailbox notify and inbox read
muster tail                                # follow the bus live — every message, task, wake and read as it happens
muster station                             # the full-screen operator TUI — projects, agents, threads, compose
muster commands --json                     # every subcommand, for family CLI discovery
muster update                              # self-update the binary in place
muster send <alias> "message"  --from me   # send a directed message
muster send <alias> "message"  --from me --intent action-requested  # mark it as needing a reply
muster send --role reviewer "please look"  --from me   # to a role
muster send --broadcast "heads up"         --from me   # to everyone (prints the blast radius and prompts first)
```

A plain `--broadcast` is gated: it prints who it would reach and waits for
confirmation before sending, and replies route back to the sender rather than
fanning out. `--yes` skips the prompt; `--standing` also reaches sessions that
start later, until they read it once (standing orders); `--wake` is break-glass
— it interrupts every recipient now instead of waiting for their next turn.

```bash
muster standing <proj>                     # list a project's live standing orders
muster standing set <proj> --key invariants "…" --from me   # create or replace one
muster standing retract <proj> --key invariants            # retract one
muster standing retract --thread <id>                       # retract one standing broadcast by id
```

An ad-hoc `send --broadcast --standing` has no key, so `retract --thread` (or
`R` in station) is how you stop it greeting new sessions.

### Registering & liveness

Agents can self-register (so a shell hook can do it at session start):

```bash
muster register [alias] --role <r> --model <name>
muster deregister [alias]
muster gc                 # tombstone agents whose tmux session is gone
muster gc --purge-agents  # hard-delete departed/dead agent rows (irreversible)
```

`deregister` (and `gc`'s default reap) don't delete an agent's row anymore —
they tombstone it (`departed=1`): identity, project, label, and read-state all
survive, so a departed agent's history stays visible (`muster station` lists
them dimmed, below the live roster) and re-registering the same alias revives
it cleanly. `muster gc --purge-agents` is the old hard-delete behavior, now
explicit and opt-in: it removes every departed or currently-dead agent row for
good.

`muster gc` also prunes the event log: rows older than `--events-keep` are
deleted (default `720h`, i.e. 30 days), so the journal doesn't grow without
bound on a long-running daemon.

`register` captures the tmux pane automatically. Alias precedence: explicit
arg → `$MUSTER_ALIAS` → tmux session name → working-directory basename.

**Paneless sessions.** A session with no tmux in its environment (Claude
Code's daemon-hosted sessions and background jobs execute outside the pane
you launched them from) registers *paneless*: alias from the working
directory, project from the enclosing git checkout, identity keyed on the
harness session UUID. Every session in a directory derives the same base, so
the alias is **allocated unique** — `dotfiles`, then `dotfiles-2`, `-3`, … —
never taken over from another live session; a resumed session revives its own
alias instead of taking a new suffix. (The numbers are bus-side allocation
order and won't necessarily match your tmux session numbering — the session
can't see its tmux name, which is the whole reason it's paneless.) The session hooks work unchanged — auto-register at
start, deregister at end, and the Stop hook checks the inbox via the daemon
directly (there's no tmux badge to poll). Paneless agents show `◌` in the
LIVE column (liveness is unknowable without a pane), are exempt from `gc`'s
tmux-liveness reaping, and can't be reached by `muster nudge` — mail waits in
the inbox for their next Stop.

`--model` is stored on the agent and
tunes `muster nudge`'s submit keystroke (`claude`, `codex`, and `cursor`
auto-submit; other values are typed without submitting).

`muster agents` shows each agent's **project** and live **label**:

- **project** is derived from the tmux socket name when it follows a
  `proj-<name>` convention (one tmux server per project). On the default tmux
  server there's no project — everything shares one namespace, and the rest of
  muster works the same.
- **label** is a name for a session, and only a deliberately-set one is
  addressable — auto-generated values are shown parenthesized and are not.
  You set one by hand with `muster label backend` (or `muster label --clear`
  to remove it) or via `prefix T` in tmux, but those aren't the only
  gestures that count: a resumed conversation re-asserts its transcript
  custom-title as its label automatically at SessionStart. A mid-conversation
  `/rename` becomes that addressable label at the *next* SessionStart (resume
  or restart) — or immediately, once the operator's statusline is wired to
  call `muster label --no-inject` on a rename as a companion gesture
  (expected, not yet shipped in this repo).
  (Stored in a tmux session option — default `@claude_task`, override with
  `$MUSTER_LABEL_OPTION`.)

### Addressing

Any command that takes a target — `send`, `nudge`, `inbox`, `tasks` —
accepts a target of the form `<alias|label|proj:label>`:

- an **alias** (globally unique): seeded from your tmux session name at first
  registration; thereafter it is the session's durable identity — re-registering
  the same alias from anywhere (including after a resume in a new tmux
  session) revives it with its inbox intact: `muster nudge muster-2`
- a **label**, resolved within your current project: `muster send frontend "…"`
- a **qualified label** to cross projects: `muster send timewalk:frontend "…"`

**Aliases are device-relative on screen, not in storage.** Every alias this
machine mints — derived, typed, or allocated — is stored with this machine's
device name in front of it (`work-laptop-dotfiles/main`), because the alias
is the roster's primary key and registration is an upsert: without a prefix,
two machines with the same repos and tmux conventions would silently take
over each other's row, inbox and all (see `muster help device`). A name is
adopted automatically from the hostname the first time this machine
registers anything, so this happens whether or not you ever run `muster
device`.

Every human-facing view — `muster agents`, `inbox`, `tasks`, `thread`,
`station`, the `@muster_agent` tmux badge — strips that prefix when it's
this machine's own, so day to day you just see `dotfiles/main`, and typing
the short form (`muster nudge dotfiles`) resolves it back to the stored
alias before lookup, tried ahead of the literal string. Two rows that would
strip to the same display string both render in full instead, so no two
agents ever appear under one name.

Trying the short form *first* is what makes a short name on this machine mean
*this machine's* agent, rather than whichever machine happens to hold the bare
string. The cost falls on a row minted before this feature, whose alias has no
prefix at all: once the same session has re-registered under its seeded name,
the old row is still listed but no longer reachable, because its only address
is the bare name that now means the new row. If you are upgrading an existing
install rather than starting fresh, read the note under [Name the
machine](docs/hosted-backend.md#name-the-machine-recommended-not-required).

MCP tools and the text muster injects into hook context never strip the
prefix: an agent reads and reports its OWN alias in full
(`work-laptop-dotfiles/main`) even on the machine that minted it, so **it is
normal and expected for an agent to identify itself by a longer name than
what your terminal shows for the same session.** This is not a display bug —
a model writes aliases into message bodies and task descriptions that get
read on *other* machines, where a bare `dotfiles/main` would silently
re-resolve against that machine's own roster and reach a different, real
agent instead of erroring. A human who mistypes a short name gets an error
and retries; a model that wrote a short name into a durable thread would
produce a silent misroute nothing reports. Humans get the readable short
form because a human can recover from a mistake; the wire format stays full
because a model's mistake would look like success.

A bare label never silently crosses projects; if it's ambiguous or only exists
elsewhere, muster errors and lists the `proj:label` candidates. Resolution is
canonical on the daemon side, not just the CLI: an unresolvable target fails
with an error and never creates a thread, whether the call came from `muster
send`/`muster nudge` or an MCP tool like `send_message`/`task_create`.

This resolution is separate from the lower-level `session_unread` and
`session_aliases` daemon ops that hooks use to ask "what's mine" — those key
off `(socket_path, session_id, session_created)`, and a tmux-side caller that
omits `session_created` doesn't get an error, just a silently empty answer
(`{total: 0}`, no aliases), because an unproven session ID reads as zero
rather than as "unknown."

### `muster station`

`muster station` is the operator's station — the full-screen TUI where
everyone reports in. It's a pure drill-down chain: projects → agents →
threads → messages. Projects and their agents browse two columns wide (a
list on the left, a preview of the selected row on the right); open an
agent and its threads open into a full-width table; open a thread and it
reads full-width.

Keys: `Enter` opens the selected item · `Esc` goes back one level · `g`
jumps home from anywhere · `m` toggles the mailbox page — station's own
mail, unread and read history; the header always shows a 📬 badge with the
current unread count, on every screen · `s` opens the composer to send
(with a target picker and an intent cycle) · `r` replies on the open
thread · `n` nudges the selected agent (with a confirmation prompt) · `d`
deregisters the selected non-departed agent after showing its identity and a
tombstone-only confirmation (never Station itself) · `t` opens a compact
claim/state-transition menu for the selected task, followed by an optional
note · `R` retracts the selected standing broadcast (marked `◆ standing`)
after a confirmation · `/` filters the current list · `a` toggles aliases vs. labels · `q`
quits.

Intents render as plain words, not the CLI's bracket shorthand — "needs
action", "wants reply", "fyi". Nonterminal tasks remain in the Station
snapshot even when they are older than the general recent-thread window;
wide thread tables show task state, and task readers show status, assignment,
ref, and explicit status-change entries. An agent that exits cleanly doesn't vanish
from its project: it stays listed below a divider, dimmed, with its thread
history intact (a tombstone).

Station registers on the bus itself, as agent `station` — `muster send
station "…"` and `muster nudge station` reach it like any other agent. If
an alias `station` is already live (a second station on the same machine),
it fails over to `station-2`, `station-3`, and so on. Quitting does not
deregister Station: its durable row and read watermark survive and revive on
the next launch.

### Notifications & nudging

When a thread that concerns an agent gets new activity — a message addressed to
it, or a reply on a thread it started — muster sets `@muster_inbox` on its tmux
session to that agent's unread count. It never types into a pane, and unlike
a transient bell the flag **persists until the agent reads its inbox**
(`get_inbox`), which clears it. An agent's own writes never flag its own
mailbox. tmux doesn't display the option by default — add
the two render lines from [`contrib/tmux-mailbox.conf`](contrib/tmux-mailbox.conf)
to see `📬<count>` on the tab title and status bar.

The badge is a **session-level** count, not a per-alias one: if a session
registers under more than one alias (a session name plus a chosen label,
say), the count is that session's distinct unread threads, deduplicated
across its aliases — a thread addressed to both aliases is counted once,
and draining one alias's inbox brings the badge down to whatever the
session's other aliases still have unread, never to zero prematurely and
never double-counted.

A message or task carries an optional **intent** — `fyi`, `reply-requested`,
or `action-requested` (set with `muster send --intent`, or by the MCP
tools; a task defaults to `action-requested` since it's inherently a
request). `muster events` and `muster tail` tag rows with it (`[fyi]`,
`[reply?]`, `[action]`), and the Stop hook's drain instruction (below)
splits its count accordingly: "You have N unread muster thread(s), M
needing action."

Every notify outcome (lit, cleared, skipped, errored) and every inbox read is
recorded in an event log — `muster events [--agent <alias>] [--limit <n>]` —
so "whose mailbox was lit when, and when was it cleared" is answerable after
the fact. Rows now carry the thread's subject alongside its target, so you can
tell which conversation an event belongs to without a separate lookup.
`muster tail` is the live view of the same journal: it prints new rows as
they land instead of a fixed page, and Ctrl-C exits immediately.

To actively poke an agent to act now:

```bash
muster nudge <alias>              # types the full drain-and-act instruction into the agent's pane and submits
muster nudge <alias> --no-submit  # type only; don't press Enter
```

Nudge submits for Claude Code (immediate Enter), Codex, and Cursor Agent (a
short delayed Enter — their TUIs treat an Enter bundled with pasted text as part
of the paste). Other model types are typed without submitting.

### Hooks (optional)

Registration and inbox-draining can be driven by session lifecycle hooks instead
of typed by hand:

- **SessionStart** → `muster register` — the session joins the bus on start.
  Claude Code fires this at launch; Codex fires it on the session's first turn,
  so say anything to a fresh Codex session ("hi" is enough) before addressing
  mail to it. On `SessionStart` with `source:"resume"`, the hook reclaims
  every alias the resumed conversation owns onto the new tmux session and
  reports the unread backlog into the session's context.
- **UserPromptSubmit** (Claude Code) → marks the session `working` in
  `list_agents` / `muster agents`.
- **Stop** (turn end) → if the session has unread muster mail, the hook tells the
  agent to drain its inbox and reply, autonomously; otherwise it marks the
  session `idle`.
- **SessionEnd** (Claude Code) → `muster deregister`; `muster gc` covers the rest.

The muster binary is its own hook — point your harness at `muster hook <event>
<model>` (e.g. `muster hook Stop claude`). Copy-paste config for Claude Code,
Codex, and Cursor Agent is in [`contrib/`](contrib/README.md). Cursor's
hook config goes in `~/.cursor/hooks.json`; its stop hook uses a `loop_limit`
and muster respects Cursor's loop/status guard. Set `MUSTER_HOOK_DISABLE=1` in a
nested harness subprocess to keep its lifecycle hooks from firing under the
parent session's identity.

**Pane ownership.** Only the session's primary agent pane acts on these
hooks — a second Claude in the same tmux session (e.g. a spawned subagent)
won't register, drain mail, or deregister. To move the identity deliberately,
run `muster register` from the pane that should own it — the explicit
commands always override.

### Spanning two machines (optional)

By default the bus is one SQLite file that one daemon owns, so agents on your
laptop and agents on your desktop are on two unrelated buses and cannot address
each other. If you want one bus across both, there is an optional hosted
backend: a DynamoDB table, a Lambda function, and an HTTP API you deploy into
**your own** AWS account with one command —

```sh
muster-deploy --region us-east-1
```

— or by hand from the CloudFormation template in
[`contrib/cloudformation/`](contrib/cloudformation/muster-backend.yaml), which
is the same file `muster-deploy` embeds. Devices authenticate with a shared
bearer token and need no AWS credentials at all.

It is opt-in, and the local path is untouched if you never want it — the AWS
SDK is not compiled into the binary you install. Setup, the security model, and
the known limitations are in
[`docs/hosted-backend.md`](docs/hosted-backend.md); read the security
section before deploying, because the endpoint is publicly reachable and the
token is the only thing protecting it.

`muster-deploy` is not installed by default — it is needed on one machine,
once, and it links the AWS SDK, which is exactly why it is not bundled into the
binary your devices run:

```sh
curl -fsSL https://muster.tools/install.sh | sh -s -- --with-deploy
```

Every alias is already seeded with a device name — auto-adopted from the
hostname the first time each machine registers anything — so two machines
with the same repos checked out can't silently claim the same alias even if
you skip this step. Naming each device explicitly is still worth doing
before you join the bus, because it's not retroactive: it makes the roster
legible ("message the ci-cd session on my **work laptop**" beats
"message the courts-macbook-pro-2-ci-cd session") instead of just collision-safe:

```sh
muster device work-laptop
export MUSTER_BACKEND=remote
export MUSTER_REMOTE_URL=https://xxxxxxxxxx.execute-api.us-east-1.amazonaws.com
# plus the token at ~/.local/share/muster/remote-token, mode 0600
```

To add a second device, run `muster-deploy -join` on the machine you deployed
from; it prints those exports, the token, and a fingerprint for checking the
copy arrived intact.

## Licensing

muster is source-available under the **Business Source License 1.1**
(`BUSL-1.1`) and is **free for any organization under 25 people** — individuals
and small teams pay nothing and nothing is held back. At 25 employees or more, a
company licenses muster; it is an honor-based commercial license, not a lock.
muster never gates a feature, never asks for a key, and never phones home — the
purchase receipt is the proof of compliance, and there is nothing to install or
activate. Deploying the hosted backend into your own AWS account is permitted
self-hosting, not a commercial hosted service.

Pricing is a one-time purchase by company size (Starter / Team / Company /
Growth, with a contact-us tier for 500+), available at
[**muster.tools/buy**](https://muster.tools/buy/). Each release converts to
Apache-2.0 three years after it ships, so no company is ever stranded on a
binary it cannot fork. The binding terms are in [LICENSE](LICENSE).

Versions before v0.18.0 were released under the MIT License and remain MIT in
perpetuity; the Business Source License applies from v0.18.0 onward.
