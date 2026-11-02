# CLAUDE.md — muster

Conventions for working in this repo (humans and agents).

## What muster is

A local multi-agent coordination bus: independent coding-agent sessions (Claude
Code + OpenAI Codex, each in its own tmux tab) hand tasks/messages to each other —
no copy/paste, subscription-only (the bus never calls a model; it routes between
agents already running on their own plans). One static Go binary, multi-mode
(`serve` daemon · `mcp` stdio server · human CLI · `lambda` handler for the
optional hosted backend).

**License.** muster is BUSL-1.1 as of v0.18.0 — free for organizations under 25
employees, and each release converts to Apache-2.0 three years after it ships.
Versions before v0.18.0 stay MIT in perpetuity.

## Build / test / run

- **`just verify`** — the gate, the same in every .tools repo: `just gate` runs
  the family Go gate (tools-actions go-ci at the version `ci.yml` pins: gofmt,
  vet, golangci-lint with the family config, `go test -race`, all four release
  targets), then `just verify-extra` runs muster's own checks (the `-tags lambda`
  build and `aws-free`). The pre-push hook and CI run exactly these, so local and
  CI can't drift. There is no repo `.golangci.yml`: a real exception is an inline
  `//nolint:<linter> // <why>`.
- **`just verify-dynamo`** — the second gate, deliberately NOT part of `verify`
  because it needs Docker. Runs `internal/dynamostore` and the DynamoDB half of
  the cross-backend conformance suite against DynamoDB Local. Without an endpoint
  those tests *skip*, so `verify` compiles and vets them but proves nothing about
  DynamoDB semantics — run this after touching `internal/dynamostore`. The
  `dynamo` job in CI runs the same two packages against a service container.
- **cgo-free** — the binary builds under `CGO_ENABLED=0` (pure-Go SQLite via
  `modernc.org/sqlite`). Don't add cgo dependencies.
- **macOS tests** use `internal/mustertest.ShortHome()` for unix-socket paths (the
  `sun_path` ~104-char limit; `t.TempDir()` is too long and breaks the socket).
- Build + run: `go build -o ~/.local/bin/muster ./cmd/muster`, then
  `muster serve | mcp | agents | send | inbox | tasks | nudge | register | status | update | commands | standing | channel | station | …`.

## Branch model

`feat/* → dev → main`. CI (`just verify`) is required on `dev` and `main`; `main` is
the release line. Never develop on `main` — do feature work in a git worktree off
`dev`, merged via PR.

**Releases are automated.** The `VERSION` file is the knob: bump it on `dev`, and
when the promotion PR merges to `main`, the release workflow tags `v<VERSION>`,
creates the GitHub release with generated notes, and attaches cross-compiled
binaries (darwin/linux × arm64/amd64) with checksums. A merge to `main` that
doesn't bump `VERSION` releases nothing. The darwin binaries are signed and
notarized by the release job itself, which is why that job runs on a macOS
runner, and it fails rather than publishing anything unsigned — so a green
release needs no manual signing step. `contrib/release-sign.sh v<VERSION>`
remains only for repairing an already-published release by hand.

Pick the next version from the TAGS, not from `dev`'s `VERSION` file. The
release commit bumps `VERSION` on `main` only, so until someone back-merges,
`dev` still shows the version before the last release and a feature branch cut
from it will propose a number that is already tagged — v0.14.0 was chosen this
way after v0.13.0 had shipped. CI's `version-guard` catches the collision, but
only once the PR is open.

## Architecture (the mental model)

- **daemon = API.** A lazy unix-socket daemon speaking newline-delimited JSON
  (`internal/proto`). The MCP server (`internal/mcpserver`) and the human CLI
  (`internal/cli`) are **peer clients** of the daemon — neither goes through the
  other. Any daemon op is reachable from a plain CLI subcommand.
- **tmux = substrate.** Liveness, wake, and identity lean on tmux — but only through
  `internal/tmuxenv` (the one canonical capture path) or the injected
  `wake.Notifier`. Keep `internal/daemon` and `internal/store` tmux-agnostic.
- **Wake is split.** `internal/wake` *notifies* (sets the `@muster_inbox` tmux
  option; never types into a pane). `internal/nudge` is the **only** send-keys path.
  The daemon never types. `internal/channel` is the third wake path: it pushes an
  envelope into the session over MCP (`muster channel`) — like `wake`, it never types.

## The naming contract

The tmux option pair `@claude_task` / `@claude_task_manual` is the neutral
meeting point between muster and the operator's dotfiles (its design spec is
no longer tracked; the last copy is in commit `726d7ed`).
Intentional gestures (prefix T, `muster label`, the SessionStart projection
of a transcript custom-title) set both; automatic syncs write only the label
and defer to the flag; readers trust the pair. The conversation's transcript
custom-title is the one durable name — tmux and the bus are projections.
Attribution requires proof: `session_created = 0` never matches a live
session (tmuxenv.IsSessionAlive), while DepartStaleSiblings still spares
those rows from reaping — attribution and tombstoning are distinct decisions.

Naming and identity are separate questions. The naming contract above governs what a conversation is *called*; underneath it, a conversation's identity resolves by transcript path first, falling back to the live tmux pane tuple only when no row carries that transcript path yet.

The harness session ID (Claude Code's own session identifier) is just an attribute on the row, re-stamped whenever it changes — it is never part of the identity key, because a `/login` (or any harness-internal event) can change it mid-conversation without starting a new conversation.

register_agent resolves against that identity before ever inserting a row: a conversation whose transcript path already has a live row adopts that row instead of minting a sibling, so one conversation keeps exactly one live row even across a harness session ID change.

## Hard rules

- **stdout is sacred in `mcp` mode** — it is the MCP channel. All diagnostics go to
  **stderr**. A stray `fmt.Println` on an mcp-mode path corrupts the protocol.
- **The AWS SDK never reaches a device binary.** Four packages may import
  `github.com/aws/aws-sdk-go-v2/...` or `aws-lambda-go`, and they divide into
  two islands neither of which `cmd/muster` can reach. `internal/dynamostore`
  and `internal/lambdamode` are the hosted BUS, entered only through
  `cmd/muster/lambda_on.go` behind `//go:build lambda`. `internal/deploy` and
  `cmd/muster-deploy` are the hosted backend's INSTALLER — a separate binary,
  because deploying to an AWS account is inherently done by someone who
  already has credentials for it, while devices must not need any.
  `just aws-free` (run by `cross`, so by `verify`) asserts
  `go list -deps ./cmd/muster` contains no AWS package and fails the gate if
  it ever does. Drop an AWS import into any untagged package on the device
  path — `internal/remote`, `internal/daemon`, `internal/store` — and it
  links into the binary every device installs. Devices reach the hosted bus over
  plain HTTPS with a bearer token (`internal/remote`) and need no AWS
  credentials, profile, region, or SDK; that is the entire reason the Lambda
  tier exists rather than devices talking to DynamoDB directly. `just verify`
  builds both configurations, and `.github/workflows/release.yml`
  builds the device binaries without the tag and the Lambda zip with it — so
  check the build tag before adding an AWS import, and never widen that edge.
- **One canonical module per concern** — identity capture lives in `internal/tmuxenv`,
  not copied around. Extend the owner; don't fork it.
- **Knobs, not constants** — operator-tunable defaults over hardcoded numbers.

## Package map

`cmd/muster` entrypoint · `internal/proto` wire protocol · `internal/client` daemon
client · `internal/daemon` the daemon · `internal/store` the `store.API` interface
+ its SQLite implementation · `internal/mcpserver` MCP tools · `internal/cli`
operator CLI · `internal/wake` notify · `internal/nudge` send-keys ·
`internal/nudgeguard` proves a live-local target before send-keys ·
`tools-common/channelmcp` (external) stdlib claude/channel MCP server · `internal/channel` the
channel carrier (journal tail → push) · `internal/render` shared journal rendering ·
`internal/resolve` canonical target resolution · `internal/display` terminal-output
sanitizer · `internal/station` operator TUI · `internal/version` build stamp ·
`internal/tmuxenv` tmux capture/liveness/label · `tools-common/harness` (external) paneless
harness-session capture and the family identity rule (tmuxenv's counterpart) · `internal/paths` socket+db paths ·
`internal/clock` injectable time · `internal/mustertest` shared test helpers.

Hosted backend (all optional; a device links only `remote` and `device`):
`internal/dynamostore` the **second `store.API` implementation**, on DynamoDB —
lambda-only · `internal/lambdamode` API Gateway → `daemon.Dispatch` adapter —
lambda-only · `internal/remote` the device's HTTPS+bearer-token transport to the
hosted bus · `internal/device` this machine's stable device identity ·
`internal/storetest` the conformance suite both `store.API` implementations must
pass, so the two backends cannot drift.

`store.API` having two implementations is the thing to hold onto: behaviour SQLite
gets for free from one pinned connection (ordering, serialization) costs conditional
writes and transactions on DynamoDB, and the divergences that survived are documented
in the package comments of `internal/dynamostore/store.go` and `events.go`. Add an
op to `store.API` and you owe both backends plus a conformance test.

## Using the bus itself

If you're an agent working here and want to coordinate with sessions in other
terminals, the `.claude/skills/muster-coordination` skill is the etiquette
(register, inbox, send/reply, addressing, the wake model).

If you're helping an operator put the bus on more than one machine, the
`.claude/skills/muster-hosted-backend` skill covers deploying with
`muster-deploy`, adding a device, custom domains, and diagnosing a hosted bus
— including the rule that matters most there: the bearer token must never
reach your context, so never run `muster-deploy -join` yourself.

## Worktrees

Every distinct line of work — a feature, a fix, an agent's task — runs in its own
git worktree on its own branch. **Never branch-switch this clone**; it may hold
uncommitted work.

Follow the global `AGENTS.md` shared worktree placement rule and the canonical workspace root identified by the workspace's `CLAUDE.md`. Create branches from `origin/dev`.

- **Never a sibling** (`../<repo>-<topic>`, `<repo>-worktrees/`). Siblings escape the
  repo, clutter the parent directory, and strand themselves when the repo moves.
- **Never `/tmp`.** On macOS it symlinks to `/private/tmp`; worktrees under it break
  vite-node/vitest module resolution *before a single test runs*.
- Follow the global `AGENTS.md` shared worktree cleanup rules.
- `.worktrees/` and `.claude/worktrees/` are gitignored.
