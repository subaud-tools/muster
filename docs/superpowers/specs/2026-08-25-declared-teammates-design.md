# Declared teammates — a spawned session that is addressable, and never a peer in identity races

Status: approved in discussion (Court, 2026-08-25), pending file-level review. Date: 2026-08-25. Origin: pi harness program Phase 5 (`crew`), bus thread #308. Companion to `2026-08-06-teammate-identity-refusal-design.md` (which this extends, not replaces) and `2026-08-21-one-conversation-one-identity-design.md`.

## 1. Problem

muster knows one kind of teammate: a Claude Code fleet member, detected by sniffing (`--team-name --agent-id` in ancestor argv, or `teamName` in the transcript) and handled by refusal — its hooks are no-ops, it gets no row. That was the right answer to the 2026-08-06 theft: teammates outnumber primaries and must not race for the primary's identity.

`crew` — the pi harness's Phase 5 — needs the opposite half of the same rule. It spawns a pi session into a right-column pane of the primary's tmux session with a role prompt, and the whole point is that the spawned session is ADDRESSABLE: the primary sends it work by alias, it answers over the bus, the operator sees it on the roster and in station, and GC can reap it when it is idle. A pi teammate matches neither Claude signal, so today it is treated as a primary and races for the session's identity — the exact theft the refusal spec closed — and if it were refused instead, it could not be addressed at all.

Three concrete gaps, from bus threads 306–308:

1. **Seating.** `hookAlias` derives the alias from the tmux SESSION name, which a teammate shares with its primary; `hookMayClaimIdentity` decides pane ownership by liveness of the stored pane, which fails open in every restart/reap window. A teammate in pane B of the primary's session needs its own alias and must be structurally excluded from claiming pane A's row.
2. **Idleness.** Claude teammates record their own idleness into a dotfiles cache directory (`~/.cache/claude-teammate-idle/<socket>_<pane>`) from their Stop hook, because `SubagentStop` fires in the parent with no pane id. The reaper reads that directory plus a tmux title glyph sampled twice. None of it is on the bus, and none of it knows about pi.
3. **Reaping.** `claude-reap-teammate-panes.sh` identifies teammates via `~/.claude/teams/<team>/config.json` and `tmux-teammate-gc.sh` sniffs `pane_current_command` for Claude — the same Claude-command sniff the dotfiles liveness gate just retired for the context indicator.

## 2. Decision

**Declaration replaces detection.** The thing that spawns a teammate says so: it exports `MUSTER_TEAMMATE=<primary alias>` (and `MUSTER_ALIAS=<teammate alias>`) into the child's environment. A session whose environment carries `MUSTER_TEAMMATE` is a *declared teammate*:

- It is **registered** — its own live roster row, `primary=<primary alias>`, its own harness link, its own read-state — so it is addressable by alias exactly like any agent.
- It is **excluded from every race for its primary's identity**: register, resume-reclaim, name projection, harness stamping, and the SessionEnd tombstone sweep all scope by the row's `primary` link, never by pane liveness.
- It **reports its own idleness to the bus** (`idle` journal events, self-stamped), and GC keys on the roster, not on files or glyphs.

**Refusal stays for undeclared Claude teammates.** The 2026-08-06 rule is unchanged for a Claude fleet member that nobody declared: its hooks remain no-ops. Declaration is what flips a session from "silent extra Claude in a pane" to "addressable crew member," and it works for any harness — Claude Code can be declared too, by launching it with the two variables.

## 3. Mechanism

### 3.1 Declaration (env, read through `harnessenv`)

`harnessenv.Capture` gains `Primary string`, captured from `MUSTER_TEAMMATE` in `FromEnv` (payload has no equivalent; hooks inherit the process env, and `FromHookPayload` already falls back to `FromEnv`). `harnessenv` stays the single canonical reader — no caller touches the variable directly (CLAUDE.md: one canonical module per concern).

Precedence at `muster hook` entry: declared teammate (`Primary != ""`) → teammate path (§3.3); else undeclared Claude teammate (argv/transcript) → refusal, unchanged; else primary path, unchanged.

Alias: `hookAlias` already honors `MUSTER_ALIAS`; the spawner sets it to the role name (`h-review-3`, `capture-targets` — the same shape today's Claude teammates carry). A declared teammate with no `MUSTER_ALIAS` gets `<primary>/<pane>` so it can never collide with the primary's session-name alias.

### 3.2 Store (`store.API`, both backends)

- `Agent.Primary string` (JSON `primary`). SQLite: `ALTER TABLE agents ADD COLUMN primary_alias TEXT NOT NULL DEFAULT ''` (column named `primary_alias`; `primary` is a SQL keyword). DynamoDB: attribute `primary_alias`. Conformance test: registered with a primary, read back, and preserved across a re-register that omits `primary` — the same preservation rule `superseded_by` gained in the 2026-08-21 spec.
- `RegisterAgent` accepts `primary`. `ListAgents` returns it (JSON already carries every field).
- New query op `Teammates(primary string) ([]Agent, error)`: live rows with `primary_alias = ?`. One op, both backends, one conformance case.
- No new event columns: idleness is an event kind (§3.4).

### 3.3 Hook paths for a declared teammate

- **SessionStart:** register on the caller's own pane tuple with `primary`, `model_type` from the hook arg, harness link from the payload. NEVER `hookMayClaimIdentity` against the session-name alias — the teammate's alias is its own, so the only conflict possible is with a previous incarnation of the same teammate alias, and that is an ordinary revive. Resume-reclaim (`hookSessionStartResume`) runs only over rows whose `primary` equals the declared one (a teammate can resume itself; it cannot reclaim its primary's rows). Name projection: skipped (a teammate's name is its role; the primary's transcript title must not project onto it).
- **Stop:** the badge/inbox drain is the teammate's OWN (its alias, its watermark). Harness stamping stamps its own row only. Additionally emits the `idle` event (§3.4) — for Claude this is where `claude-teammate-idle.sh` used to write its cache file; the hook replaces it.
- **SessionEnd:** tombstones the teammate's own row only. The primary's SessionEnd sweep (`hookSessionEnd`) additionally tombstones live teammate rows whose `primary` is the departing primary and whose pane is dead — a primary leaving takes its orphaned crew with it; a still-live teammate pane is left alone (it may be finishing).
- **Ownership fails closed** on daemon errors, unchanged from §3a of the refusal spec.

### 3.4 Idleness on the bus

`log_event` gains a second accepted shape: `{kind: "idle", agent: <caller alias>}` — self-reported only; the daemon stamps `ts` and refuses a caller naming an alias it does not own (same ownership proof the owned-read rules use). The journal already answers "latest event of kind K for agent A"; `Teammates(primary)` plus the latest `idle`/`send`/`reply` events per teammate give station and GC an idle-since without a new column or a cache directory.

Emitters: `muster hook Stop <model>` for any declared teammate (Claude, pi, codex, cursor alike); the pi harness extension calls the hook from `agent_settled`, so nothing pi-specific is needed. A teammate that works again emits `send`/`reply`/`read` events, which supersede the `idle` timestamp — a resumed teammate can never look stale.

### 3.5 Reaping

`muster gc --teammates [--idle-minutes N] [--kill]`: for every live teammate row whose latest idle event is older than N minutes, whose primary is (a) departed or (b) live but not currently addressing it, and whose pane is alive: kill the pane and tombstone the row. Dry-run by default; identity comes from the roster (`primary_alias` set, `pane_id` stored) so a primary's pane can never be targeted — the same structural exclusion the shell reaper got from `teams/config.json`. Idle-glyph double-sampling is no longer needed: the teammate said it was idle.

dotfiles follow-through (branch AFTER `feat/pi-harness-terminal` merges, coordinated on the bus): `claude-reap-teammate-panes.sh` becomes a thin wrapper over `muster gc --teammates`; `tmux-teammate-gc.sh` (prefix g) keeps its "ask the lead" behavior but locates the lead via the roster (`primary` of the window's teammates) instead of pane geometry + command sniff; `claude-teammate-idle.sh` is deleted (`muster hook Stop claude` now writes the fact).

### 3.6 Station

Teammate rows render nested under their primary (one indent, `↳ role`), with the idle-since from §3.4 in the vitals line. `muster agents` gains a `PRIMARY` column.

## 4. What crew must do (the contract)

1. Spawn with `MUSTER_TEAMMATE=<primary alias>` and `MUSTER_ALIAS=<role>` in the child env (tmux `split-window -e` or a wrapper).
2. Nothing else. The child's harness extension already calls `muster hook SessionStart pi` with a payload, `hook Stop pi` on `agent_settled`, and `hook SessionEnd pi` on shutdown — the same three calls a primary makes.
3. To address the teammate: `send <role>`. The teammate answers the primary by the value of `MUSTER_TEAMMATE` — the same string it was declared with, readable from its own env.
4. To reap: `muster gc --teammates --kill`, or leave it to the operator's prefix-g gesture.

## 5. Testing

- Store conformance (both backends): `primary` persists, re-register preserves it, `Teammates()` returns only live rows for that primary.
- Hook: the 2026-08-06 regression scenario re-run with a DECLARED teammate — primary's row byte-for-byte untouched, roster gains exactly the teammate's alias with `primary` set; declared teammate resume reclaims only its own rows; teammate SessionEnd tombstones only itself; primary SessionEnd tombstones its dead-pane teammates and spares live ones.
- `log_event idle`: accepted for the caller's own alias, refused for another's; latest-idle query.
- `gc --teammates`: dry-run lists, `--kill` acts only on rows meeting all four conditions; a live primary pane is never a candidate (structural test: a primary row with `primary_alias=''` cannot be selected).
- Live trial: crew spawns a pi teammate; `muster agents` shows it under the primary; `send` reaches it; its reply reaches the primary; `gc --teammates` reaps it after idle.

## 6. Non-goals

- Changing the undeclared-Claude-teammate refusal rule.
- Cross-device teammates (hosted bus): `primary_alias` is device-scoped like every other tuple field; a follow-up if crew ever spawns remotely.
- Replacing `TaskStop` for Claude fleet teammates: Claude Code's own team roster (`teams/<team>/config.json`) is still the only complete removal there; `gc --teammates` handles declared teammates, which are the pi case.
