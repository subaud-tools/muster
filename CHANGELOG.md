# Changelog

Newest first. Releases before 0.23.0 are noted only where the change needed explaining.

## Unreleased

`muster setup` enables the Claude Code plugin (`muster@muster`) and no longer registers muster-channel; it removes a muster-channel entry left in `~/.claude.json`. The `muster setup --print kempt` block does the same.

## 0.27.0 — Claude Code hears muster without a launch flag

**Breaking: `muster watch` is renamed `muster tail`.** It is a rename only: the same flags (`--agent`, `--kind`, `--thread`, `--interval`, `--backlog`, `--aliases`, `--full-time`, `--width`) and the same live view of the bus journal. There is no `watch` alias for it; `watch` now names the event stream every family tool's Claude Code plugin runs.

**Claude Code hears muster without a launch flag.** muster ships a Claude Code plugin whose mod runs the new `muster watch` (the event stream) for the session, so mail starts a turn in a session started with plain `claude`. No `--dangerously-load-development-channels` and no warning dialog at launch. Under Claude Code, `muster channel` serves `muster_channel_status` only and leaves mail to `muster watch`; under pi it works as before.

## 0.26.0 — requests to an ended session are refused

A request (reply-requested, action-requested, or a task) to an agent that has ended is now refused with when it was last seen, instead of being accepted and never answered; fyi sends still wait for the alias to return.

## 0.25.1 — one daemon, restarted on upgrade

**The launchd job no longer restarts forever.** When a session's client had already started the daemon, `muster serve` under launchd found the lock held and exited cleanly, and launchd restarted it every ten seconds (103,600 runs on one Mac). A serve that a client did not start now waits for the lock and takes over as soon as the other daemon exits. A client-started serve still exits when the lock is held.

**The daemon restarts itself when its binary is upgraded.** It checks its executable every five seconds and exits when the file is replaced; launchd, or the next client, starts the new version. Previously nothing restarted it, so a daemon could run old code for weeks after `kempt update`.

## 0.25.0 — module path

Module path is now github.com/subaud-tools/muster; the source lives at github.com/subaud-tools/muster.

## 0.24.0 — working or idle, and drain first

**Every agent row now says whether its session is working or idle, and since when.** `list_agents` carries `activity` (`working`, `idle`, or empty when never reported) and `activity_at` (unix ms), and `muster agents` shows them in a new ACTIVITY column after LIVE (`working 4m`, `idle 2h`, `—`). A coordinator can see a stalled peer without asking it.

- New hook event `muster hook UserPromptSubmit <model>` marks the session working. To install it for Claude Code, run `kempt apply` with the updated `[packages.muster]` block (`muster setup --print kempt` prints it), or re-run `muster setup` if you don't use kempt.
- The Stop hook marks the session idle, unless it hands the agent mail (a block decision), which keeps it working. Every Stop now makes one extra daemon call for this.
- Reading your own inbox with `get_inbox` also marks you working; a peek at someone else's inbox changes nothing.

**Channel pushes tell the agent to drain first.** The guidance on a pushed message now starts with "First mark it read: call your get_inbox tool…", before it says to read the thread, so the 📬 badge clears as soon as the agent starts on the mail rather than after it replies.

The SQLite database moves to schema version 2 (two new columns on `agents`) on first open. muster 0.23.x refuses a version-2 database, so downgrading after this release needs the database restored from before the upgrade. Restart the daemon after updating: `launchctl kickstart -k gui/$(id -u)/tools.muster.serve`.

## 0.23.1

- Releases are built through the family release actions (tools-actions); the assets, signing and `/dl` paths are unchanged.
- Checked by the family lint set (tools-actions v0.6.0), which replaces muster's own `.golangci.yml`. Its few findings are fixed or carry a stated reason; no behaviour change.
- `just verify` and the pre-push hook run exactly what CI runs, at the version CI pins; the Lambda build and the AWS-free check are `just verify-extra`. The pre-commit hook now checks formatting instead of rewriting files.
- These notes moved from `docs/release-notes.md` to `CHANGELOG.md`.

## 0.23.0 — the family CLI and the shared database module

**muster's command line now runs on tools-common's tools.App, like every other .tools binary.** What you will notice:

- `muster help` opens with an overview (the three modes, the files, the docs) above the commands grouped as Talk, Watch, Identity and Plumbing, each group sorted by name. Bare `muster` prints the short usage to stderr and exits 2.
- `muster help <cmd>` and `muster <cmd> -h` show `Usage: muster <synopsis>`, the aliases (the MCP tool names, e.g. `get_inbox`), the one-line summary, the long help and the flags.
- Errors read `muster <cmd>: <message>`, with exit codes 0 ok, 1 runtime error, 2 usage error. With `--json`, errors are the family JSON envelope.
- `muster man` replaces `muster help --man`; `muster commands --json` is the family command index, now including the built-ins.
- Every command's own output is unchanged, and hooks still exit 0 when there is nothing to do or no daemon.

**The daemon's SQLite store opens through tools-common/sqlitedb.** Your existing database is adopted in place on first open (it becomes schema version 1; no rows change), and muster 0.22.x still opens it afterwards. The database and its -wal/-shm files are 0600 from creation, and two processes opening a new database at once both succeed. modernc.org/sqlite moves to v1.59.0.

After updating, restart the daemon so it runs the new store code: `launchctl kickstart -k gui/$(id -u)/tools.muster.serve` (kempt's service label).

## 0.22.1 — session identity from tools-common

**muster now resolves a session's identity with the family rule in tools-common/harness.** The one visible change: a Claude Code process that pi-claude-bridge runs to serve a pi turn now counts as that pi session. The bridge marks it with `AGENT_SESSION_CHILD=1` next to the `AGENT_SESSION_ID` it sets, so roster ownership and MCP caller identity attribute it to the pi session instead of registering it separately. A Claude session you start yourself from inside pi (no marker) is still its own session. Needs `@schuettc/pi-claude-bridge` 0.8.0-schuettc.6 or later, which sets the marker.

Internally, muster's own copies of the channel MCP server and the harness identity code are replaced by `tools-common/channelmcp` and `tools-common/harness` (tools-common v0.4.0). No commands, flags or tools change.

## 0.22.0 — retract a standing broadcast

**A standing broadcast can now be retracted, from Station, the CLI, or MCP.** A broadcast sent with `--standing` (`standing=true` on `send_message`) greets every session that starts later, once, until it reads it. Keyed standing orders (`muster standing set`) could always be retracted, but an ad-hoc standing broadcast had no key to name, so once sent it greeted every new session in the project with no way to stop it. Now:

- **Station** marks every live standing broadcast `◆ standing` in the thread tables and the mailbox (`/standing` filters to them). Press `R` on the selected or open one to see its target and subject, then `y` to retract it. On any other thread `R` just says it isn't a live standing broadcast.
- **CLI:** `muster standing retract --thread <id>`.
- **MCP:** `standing_retract_thread` (`from`, `thread_id`).

Retracting stops the broadcast greeting new sessions and journals a `standing` event; the thread and its replies stay in history, and a session that already read it is unaffected. Retracting an already-retracted thread reports that and changes nothing; an id that is not a standing broadcast is an error. `GetThread`, `list_threads` and `get_inbox` now report `standing_key` and `standing_retracted` on the SQLite store too, matching the hosted backend.

## 0.20.0 — MCP and Station action surface

### Station task and lifecycle actions

**Station now keeps active work visible and can act on it in place.** Its existing thread snapshot retains up to 500 older nonterminal tasks without adding another poll. Wide thread tables show task state, task readers show status, assignment, ref, and empty-body status changes, and `/` filtering matches task state. Press `t` to claim or transition a selected task with an optional note. Press `d` on a selected non-departed agent to review its identity and liveness, then tombstone it while preserving history and read state; Station cannot deregister itself. Quitting Station continues to preserve its own durable row and read watermark.

### MCP data actions

**The MCP blackboard is now complete and task work is discoverable bus-wide.** `kv_list` returns the complete small blackboard in lexicographic key order with an optional literal prefix, and idempotent `kv_delete` removes one key. `kv_set` and `kv_delete` no longer accept a caller-supplied `by`; they derive attribution from the proven current session and reject unregistered callers. `list_tasks` provides composable exact project, status, creator, and target filters, defaults to the newest 100 nonterminal tasks, caps requests at 500, and reports `truncated` instead of introducing cursors.

### MCP caller lifecycle

**MCP clients can now discover and retire their own identity without guessing an alias.** Call `current_agent` first: it returns the calling session's proven live identity and every live alias in that session's lineage. If it is not registered, call `register_agent`; otherwise use the returned alias in identity-dependent tools. `deregister_agent` takes no arguments and tombstones every live alias proven to belong to the caller, preserving history and read state for a later return. It cannot target another agent. `get_status` exposes the existing side-effect-free unread and action-required counts for every alias or one exact alias; it never marks mail read or journals a peek.

## 0.19.1 — channel wakes survive an env-stripped harness

**The muster channel now resolves its pane by process ancestry when the environment is stripped.** The channel MCP server captured its tmux identity from `$TMUX`/`$TMUX_PANE` only. Harnesses that spawn MCP servers without those variables (Claude Code) left the channel with no pane: the SessionStart hook still registered the agent (it already walks process ancestry), so mail arrived and the inbox filled, but the channel never pushed a wake — the session only saw its mail when an operator told it to check the inbox, and `muster_channel_status` reported `idle: no tmux pane`. The channel now uses the same env-else-ancestry capture the hooks use (`channelCapture`); since the channel server is a synchronous child of the harness, its ancestry reaches the pane's shell exactly as a hook's does. No change is needed on machines where the channel already worked. A genuinely paneless session (no pane in the environment or the ancestry) still idles, as before.

## 0.19.0 — `muster setup`

**`muster setup`** configures your coding agents for you. It detects the agents installed on the machine — **Claude Code, Codex, and Cursor** — and, for each, registers muster's MCP server and installs the session hooks (SessionStart/Stop/SessionEnd) via idempotent JSON/TOML deep-merge, so there is no config file to hand-edit. Flags: `--tmux` also renders the 📬 mailbox badge in `~/.tmux.conf`; `--dry-run` previews every change; `--no-hooks` registers the MCP server only; `--agent` limits to specific agents; `--force` applies even when kempt is present. Re-running is safe — every write is a merge that preserves existing content.

When **kempt** is installed, `muster setup` defers to it: it prints the declarative `[packages.muster]` block to add to your `kempt.toml` rather than editing files. **`muster setup --print kempt`** emits that same canonical package on demand — it is the single source both the imperative and declarative paths share, so they never drift.

## 0.18.0 — Business Source License

muster adopts the **Business Source License 1.1**. It is **free for any organization with fewer than 25 employees** — every feature, no key, no gating. At 25+ employees an honor-based **one-time** commercial purchase applies, priced by company size; there is no feature gating, no license key, and no phone-home, so your receipt is the proof. **Each release converts to Apache-2.0 three years after it ships.** Versions **before 0.18.0 stay MIT in perpetuity** — BUSL applies from 0.18.0 onward. Deploying the hosted backend into your own AWS account is permitted self-hosting, not a commercial hosted service.

**`muster commands [--json]`** lists the family CLI surface for discovery. Internally, `internal/humancli` was renamed to `internal/cli`.

## 0.17.0 — broadcast storm guard

**Broadcasts now reply-to-originator and pass a confirm gate.** A first broadcast returns its blast radius and sends nothing; you re-send with `confirm` to actually deliver it, and replies route back to the originator rather than fanning out.

## 0.16.0 — coordination discipline

**Standing orders replace the live broadcast backlog.** A standing order is a keyed, retractable per-project convention; new sessions no longer inherit the live broadcast backlog and instead pick up the standing orders that still apply. **Wake discipline** codifies the etiquette: polite broadcasts, fast direct sends, explicit break-glass.

**`muster update`** self-updates the binary in place, so the deploy machine and devices no longer re-run the installer. **`muster status --json`** reports side-effect-free per-alias inbox counts (the project attention column) without marking anything read. **`MUSTER_HOOK_DISABLE`** guards nested harness subprocesses from firing muster's hooks.

## 0.15.2

**Binaries are now published to muster.tools/dl**, the family download standard. GitHub releases become the durable backing store behind it.

## 0.15.0 — channel mode

**New `muster channel` MCP carrier** (Claude Code `claude/channel`, pi `pi-channels`) pushes a compact envelope — intent, sender, thread, subject, **never the body** — into an idle session, a third wake path alongside the mailbox badge and `muster nudge`. Tunable via **`MUSTER_CHANNEL_INTERVAL`** (default 1s) and **`MUSTER_CHANNEL_MAX_LISTED`** (default 5). Event-scoped guidance rides after a `---` separator; pi rename propagation flows through the native `/name`; and the carrier accepts a harness-neutral **`AGENT_SESSION_ID`**.

## 0.14.0 — one conversation, one identity

This release makes a conversation own exactly one live roster row. Identity resolves by transcript path first, and falls back to the live tmux pane tuple only when no row already carries that transcript path. A harness session id (Claude Code's own session identifier, distinct from muster's alias) is just an attribute on the row that gets re-stamped whenever it changes — a `/login` or any other harness-internal event can change it mid-conversation without starting a new conversation as far as muster is concerned.

**Upgrade order matters.** If you run the optional hosted backend, redeploy it first. Then install the new `muster` binary on each device. Then kill any already-running `muster serve` daemon so it restarts on the new version — the daemon is a lazy unix-socket process, so it does not pick up a new binary on its own; the next command that needs it will start a fresh instance. A 0.14.0 client talking to an old daemon (or an old hosted lambda you haven't redeployed yet) still works during the gap: `muster inbox` and the MCP `get_inbox` tool both decode the old response shape as well as the new one, so you won't see decode errors, but the ownership behavior described below only takes effect once the daemon itself is upgraded.

**`muster inbox <alias>` now marks read only from the owning session.** Reading an alias's inbox moves that alias's unread watermark (and clears its tmux badge) only when the call proves it comes from the session that alias actually belongs to — a live pane in that tmux session, or the same harness conversation. Any other read, including an operator running `muster inbox` on another agent's alias just to look, still returns the threads, but is a peek: nothing is marked read, and the unread count and badge are untouched. This is the behavior change most likely to surprise someone: if you were in the habit of running `muster inbox <alias>` from a different pane to clear an agent's badge on its behalf, that no longer works. The agent itself (or a genuine resume of its conversation) has to read its own mail.

**Mail addressed to a deregistered alias without `become` now drains for nobody.** If an agent's alias is deregistered directly — not retired through `become` into a new name — any mail still addressed to that old alias has no owning session left to prove ownership, so `muster inbox <that-alias>` from any pane comes back as a peek. The mail is still there and still readable, but nothing will mark it read on its own. An operator has to run `muster inbox <alias>` deliberately to look at it, understanding it stays unread until someone with real ownership proof reads it, or until it's cleaned up with `muster deregister`/`gc --purge-agents`. This is a real behavior change from prior releases, where any reader draining that alias would clear it.

**`become` can now reclaim a departed name.** If an alias was retired (departed) and nothing has taken its place, `muster become <that-alias>` (or the MCP `register_agent` tool with `become:true`) can claim it again, carrying over its identity and unread state — where before a departed name was permanently unavailable.
