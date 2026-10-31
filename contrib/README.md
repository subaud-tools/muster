# contrib — optional setup pieces

muster works with nothing but the binary and an MCP registration (see the main
README). The pieces here are the two optional layers on top:

## 1. Show the mailbox in tmux (`tmux-mailbox.conf`)

When mail arrives for an agent, the daemon sets `@muster_inbox=<unread count>`
on its tmux session — but tmux won't *display* it until you tell it to. Add the
two lines in [`tmux-mailbox.conf`](tmux-mailbox.conf) to your `~/.tmux.conf`
(merge them into your own `set-titles-string` / `status-left` if you customize
those), reload tmux, and sessions with unread muster mail show `📬<count>` in
the tab title and status bar until the agent reads its inbox.

## 2. Hooks: auto-register + self-resolving inbox

The muster binary is its own hook — no script to copy. Point your agent's
session hooks at `muster hook <event> <model>`:

- **SessionStart** → registers the session on the bus.
- **Stop** (turn end) → if the session has unread muster mail, tells the agent
  to drain its inbox and reply — autonomously.
- **SessionEnd** (Claude Code only; Codex has no such event) → deregisters.

Prefer `muster setup` (or `muster setup --force` when kempt is installed)
for migration: it reconciles the known bare and `~/.local/bin/muster` Claude
hook spellings by exact event and matcher, preserving unrelated hooks. Simply
merging JSON does not remove legacy duplicates.

These hook files are generated from `internal/cli/setup_kempt.toml` and pinned
by `TestContribHooksFromCanonicalSource`. Regenerate from the repository root:

```sh
go run ./cmd/muster setup --print claude-hooks > contrib/claude-settings-hooks.json
go run ./cmd/muster setup --print codex-hooks > contrib/codex-hooks.json
go run ./cmd/muster setup --print cursor-hooks > contrib/cursor-hooks.json
```

The release setup bundle uses kempt >= **0.13.0** (`[kempt] spec = 2`) and
retains the daemon service, marketplace and shared configuration. It excludes
the owning binary download and Cursor; Cursor remains available through direct
setup. Publication uses tools-actions >= **0.13.1**. Direct setup does not
install or activate the daemon service.

Manual setup:

- **Claude Code:** merge [`claude-settings-hooks.json`](claude-settings-hooks.json)
  into `~/.claude/settings.json`.
- **Codex:** copy [`codex-hooks.json`](codex-hooks.json) to `~/.codex/hooks.json`.
  On the next `codex` launch you'll get a one-time "Hooks need review" prompt —
  choose Trust. Codex fires `SessionStart` lazily, on the session's first turn —
  a freshly opened Codex session is not on the bus until you say something to
  it, so give it any first message ("hi" is enough) before addressing mail to it.
- **Cursor Agent:** copy [`cursor-hooks.json`](cursor-hooks.json) to
  `~/.cursor/hooks.json`. Cursor's `stop` hook has a `loop_limit` of 3; muster
  also declines continuations after Cursor reports a non-zero `loop_count` or
  an `aborted`/`error` status.

For manual Codex/Cursor copies, replace `${HOME}` with your absolute home
path before installing the generated JSON. Direct setup resolves it for you;
kempt expands it during application. Claude keeps the literal
`~/.local/bin/muster` command.

**Push delivery (optional, additive):** for Claude Code, run `claude plugin marketplace add subaud-tools/plugins` and `claude plugin install muster@subaud-tools` (or let `muster setup` enable the plugin), and keep `muster mcp` registered for the bus tools.

Turn on auto-update for the `subaud-tools` marketplace so the plugin stays current: in Claude Code, run `/plugin`, open the **Marketplaces** tab, select `subaud-tools` and choose **Enable auto-update**.

New mail then starts a turn in the session instead of waiting for the Stop-hook drain. The hooks above stay exactly as configured; they are the fallback when the plugin isn't installed.

## 3. Cursor MCP setup

Add muster to `.cursor/mcp.json` for a project, or `~/.cursor/mcp.json` for all
projects:

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

Enable the configured server with `agent mcp enable muster`. For unattended
agent use, add `"Mcp(muster:*)"` to the `permissions.allow` list in
`~/.cursor/cli-config.json`.

The hook is safe to install globally: for any session that isn't a registered
agent it does nothing, and it never blocks a session from starting.

### `muster-session-hook.sh`

The same behavior as a standalone POSIX shell script, for anyone who wants to
customize the hook (change the drain instruction, add logging, gate it per
project). Functionally identical to `muster hook`; point your hook config at
the script instead if you use it.

## 3. The hosted backend (`cloudformation/muster-backend.yaml`)

An optional CloudFormation stack that puts the bus in your own AWS account so
several machines share one roster: a DynamoDB table, a Lambda function running
the same dispatch code the unix socket serves, a token-authenticated Function
URL, and a least-privilege execution role. Devices need no AWS credentials.

Do not deploy it from this file alone. The endpoint is publicly reachable and a
shared bearer token is the only thing protecting it, so read
[`docs/hosted-backend.md`](../docs/hosted-backend.md) first — it covers the
security model, the deploy and device setup, token rotation, cost, and the
limitations that only exist on this backend.
