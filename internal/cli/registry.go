package cli

import (
	"errors"
	"flag"
	"io"
	"os"

	tools "github.com/subaud-tools/tools-common"
	"github.com/subaud-tools/tools-common/setupbundle"

	"github.com/subaud-tools/muster/internal/station"
	"github.com/subaud-tools/muster/internal/version"
)

// The four command groups, in the order the operator sees them everywhere
// (`muster help`, the man page): talk first (the thing you do most), watch
// second, identity third, plumbing last (daemon/dev internals, rarely typed
// by hand). groups below is the only place display order is decided.
const (
	GroupTalk     = "talk"
	GroupWatch    = "watch"
	GroupIdentity = "identity"
	GroupPlumbing = "plumbing"
)

// groups is the display order and heading of each group.
var groups = []tools.Group{
	{Key: GroupTalk, Heading: "Talk"},
	{Key: GroupWatch, Heading: "Watch"},
	{Key: GroupIdentity, Heading: "Identity"},
	{Key: GroupPlumbing, Heading: "Plumbing"},
}

// Registry is every muster subcommand, operator-facing and plumbing alike, as
// tools.App commands. Do not add a command anywhere else: NewApp registers
// this slice, and tools.App's dispatch, help, man and `commands --json` all
// read from it. serve/mcp/channel/debug/lambda have Run == nil: cmd/muster's
// main() routes them itself (process-level setup), and their rows exist so
// help, man and the command index still cover them. version, help, man,
// commands and update are tools.App built-ins.
//
// Built in init() rather than as a var literal on purpose: several Run
// closures below call HelpFor, which (via lookup) reads Registry itself.
// Go's package-initialization-cycle check is a static, conservative
// approximation that follows every function a var initializer references,
// transitively — it can't tell that HelpFor is only ever called long after
// init(), so a `var Registry = []Command{...Run: closureThatCallsHelpFor...}`
// literal trips a false-positive "initialization cycle for Registry" at
// compile time. Assigning inside init() sidesteps that check entirely: it's
// ordinary sequential code, not a variable initializer expression.
var Registry []tools.Command

func init() {
	bundleCommand := setupbundle.BundleCommand("muster", version.Version(), setupSource())
	bundleCommand.Group = GroupPlumbing
	Registry = []tools.Command{
		bundleCommand,
		{
			Name:     "send",
			Aliases:  []string{"send_message"},
			Synopsis: `send <target> "body" [--from <alias>] [--subject <s>] [--ref <r>] [--role] [--broadcast [--project <p>] [--standing] [--wake] [--yes]] [--intent fyi|reply-requested|action-requested]`,
			Summary:  "Send a message to an agent, role, or everyone.",
			Help: `target is an alias, a label, or a "project:label" pair, resolved the same
way for every muster surface (send, nudge, inbox, tasks). --role treats
target as a role name instead of an agent; --broadcast ignores target and
sends to every registered agent (target is then omitted: 'muster send
--broadcast "body"'). With --project, the broadcast reaches only agents
registered under that exact project (the daemon rejects unknown projects and
lists the known ones). A plain broadcast is live-only: it reaches sessions
live now and is invisible to sessions that start later. --standing (broadcast
only) also reaches sessions that start later, once, until they read it — use
it for durable standing orders, not transient holds. A broadcast lands
politely by default — it sets each recipient's mailbox badge and is picked up
on their next turn, never interrupting them all at once. A broadcast is also
hard-gated: it prints its blast radius (how many agents, which ones) and asks
to confirm before sending, so a reflexive broadcast can't storm every session;
--yes skips the prompt (required when there's no terminal to prompt on).
--wake is break-glass (broadcast only): actively interrupt every recipient
now; use sparingly, for genuinely urgent announcements. --intent tags the message for the recipient's
inbox/hook rendering: fyi (default, no action implied), reply-requested, or
action-requested.`,
			Group:    GroupTalk,
			NewFlags: newSendFlags,
			Run:      adapt(cmdSend),
		},
		{
			Name:     "nudge",
			Synopsis: `nudge <alias|label|proj:label> [--no-submit]`,
			Summary:  "Type a check-inbox nudge into an agent's tmux pane.",
			Help: `Resolves the target to its registered tmux pane and types the check-inbox
line into it, auto-submitting (pressing Enter) when the agent's model type
accepts an unattended submit. --no-submit types the line but leaves it for
the operator (or the agent) to submit by hand.`,
			Group:    GroupTalk,
			NewFlags: newNudgeFlags,
			Run:      adapt(cmdNudge),
		},
		{
			Name:     "reply",
			Synopsis: `reply <thread-id> "body" [--from <alias>] [--fyi]`,
			Summary:  "Append a reply to an existing thread.",
			Help: `The CLI half of the MCP reply tool: appends an entry to the thread and
flags every participant's mailbox, exactly as a tool-sent reply would.
--from is the replying agent's alias (default "human"). --fyi marks a
closing note: the entry lands on the thread but wakes nobody — recipients
see it on their next natural inbox check. Use it for acks and wrap-ups
that need nothing back, so a closure doesn't wake the peer into one more
closing ack. Together with 'muster inbox' and 'muster thread' this
completes the read-and-respond loop from a plain shell — the fallback
when a session has no muster MCP connection.`,
			Group:    GroupTalk,
			NewFlags: newReplyFlags,
			Run:      adapt(cmdReply),
		},
		{
			Name:        "standing",
			Subcommands: []string{"set", "retract"},
			Synopsis:    `standing <project> [--json] | standing set <project> [--key <k>] "body" [--from <alias>] | standing retract <project> [--key <k>] | standing retract --thread <id>`,
			Summary:     "Manage a project's durable standing orders (its invariants).",
			Help: `A standing order is a project's durable instruction that every session should
read on start (e.g. its invariants / golden rules). Unlike an ad-hoc
'send --broadcast --standing' it is KEYED and REPLACEABLE, so it stays a single
current statement rather than a growing pile.

  standing <project> [--json]              list the project's live orders
  standing set <project> [--key k] "body"  create or REPLACE the order under a key
  standing retract <project> [--key k]     retract it
  standing retract --thread <id>           retract one standing broadcast by id

--key defaults to 'invariants' (the project's single set of rules). set is
idempotent by (project, key) and RE-GREETS every session with the updated text —
running sessions and future ones alike — until each reads it, so a mid-flight
correction reaches everyone. retract stops it greeting new sessions and drops it
from the list; a session that already read it is unaffected. --json on the list
form is the audit/verify seam.

retract --thread is the way to retract an ad-hoc 'send --broadcast --standing'
(it has no key to name); it also works on a keyed order's thread. The id must
be a standing broadcast; get it from station or 'muster inbox'.`,
			Group:    GroupTalk,
			NewFlags: newStandingFlags,
			Run:      adapt(cmdStanding),
		},
		{
			Name:     "status",
			Synopsis: "status [--json] [--alias <alias>]",
			Summary:  "Side-effect-free per-alias inbox counts (unread, action_required).",
			Help: `Prints every registered alias's unread count and the subset whose intent is
action-requested. It is a PURE READ — it moves no read watermark and journals
no peek — so a picker or attention column can poll it on a cadence without
disturbing anyone's inbox state (unlike 'inbox', which marks read for the
owning session).

--json emits a top-level JSON array of {alias, unread, action_required}; the
default is a plain 'unread action alias' table. --alias restricts to one
alias. Departed aliases are included — their mail still waits.`,
			Group:    GroupWatch,
			NewFlags: newStatusFlags,
			Run:      adapt(cmdStatus),
		},
		{
			Name:     "agents",
			Aliases:  []string{"list_agents"},
			Synopsis: "agents",
			Summary:  "List registered agents, grouped by project, with live status.",
			Help: `Shows every registered agent's project, alias, label, model, and whether
its tmux session is still alive.

A DEVICE column appears only when the roster spans more than one machine —
that is, on a hosted bus with devices in it. Your own machine reads "this";
others show the first characters of their device id. Device is LOCATION, not
identity: nothing is addressed by it, and an alias means the same agent from
every device on the bus.`,
			Group: GroupWatch,
			Run: adapt(func(args []string, out io.Writer) error {
				if helpRequested(args) {
					return HelpFor("agents", out)
				}
				return cmdAgents(out)
			}),
		},
		{
			Name:     "inbox",
			Aliases:  []string{"get_inbox"},
			Synopsis: "inbox <alias|label|proj:label>",
			Summary:  "Show an agent's threads.",
			Help: `Prints every thread in the target agent's inbox: id, kind, from, to, status, who
spoke last, unread count, and subject. The read watermark only moves for the
alias THIS tmux session (or paneless harness session) actually owns — proven
the same way every other identity op proves it, from your tmux/harness
capture, never taken on trust from the name you typed. Reading any other
alias — an operator checking on another agent, a bare invocation outside any
session — still shows the threads, but as a peek: nothing is marked read, and
a trailing notice says so. Every peek is journaled, so a sweep across aliases
you don't own is visible in 'muster events' after the fact.`,
			Group: GroupWatch,
			Run:   adapt(cmdInbox),
		},
		{
			Name:     "tasks",
			Synopsis: "tasks <alias|label|proj:label>",
			Summary:  "Show an agent's task threads.",
			Help:     `Same as 'muster inbox' but filtered to kind=task threads only.`,
			Group:    GroupWatch,
			Run:      adapt(cmdTasks),
		},
		{
			Name:     "thread",
			Aliases:  []string{"get_thread"},
			Synopsis: "thread <id>",
			Summary:  "Show one thread's full conversation.",
			Help: `Prints the thread header (kind, participants, status, intent, subject)
then every entry oldest-first with author, timestamp, and the verbatim
body. The CLI half of the MCP get_thread tool. Side-effect-free: printing
a thread never marks it read — 'muster inbox' owns the read watermark.`,
			Group: GroupWatch,
			Run:   adapt(cmdThread),
		},
		{
			Name:     "events",
			Synopsis: "events [--agent <alias>] [--kind <kind>] [--thread <id>] [--limit <n>] [--aliases] [--full-time] [--width <cols>]",
			Summary:  "Print the bus journal (event log), oldest first.",
			Help: `The observability log behind every mailbox notify outcome (lit / cleared /
skipped / error), inbox read, send, task, and nudge event — "when was whose
mailbox actually lit." --aliases shows raw aliases instead of resolving
current labels; --full-time prints dates, not just times.`,
			Group:    GroupWatch,
			NewFlags: newEventsFlags,
			Run:      adapt(cmdEvents),
		},
		{
			Name:     "tail",
			Synopsis: "tail [--agent <alias>] [--kind <k>] [--thread <id>] [--interval <dur>] [--backlog <n>] [--aliases] [--full-time] [--width <cols>]",
			Summary:  "Tail the bus journal live.",
			Help: `Prints the last --backlog matching events, then polls for new ones every
--interval until interrupted (Ctrl-C). Side-effect-free: watching never
marks anything read.`,
			Group:    GroupWatch,
			NewFlags: newTailFlags,
			Run:      adapt(func(args []string, out io.Writer) error { return cmdTail(args, out, tailOpts{}) }),
		},
		{
			Name:     "station",
			Synopsis: "station [--interval <dur>] [--aliases] [--width <cols>] [--alias <name>]",
			Summary:  "Full-screen operator TUI for the bus.",
			Help:     `A live, navigable projects → agents → threads view of the bus, built on the same event journal 'muster tail' follows.`,
			Group:    GroupWatch,
			NewFlags: station.FlagSet,
			Run: adapt(func(args []string, out io.Writer) error {
				// station.Run launches a full-screen bubbletea program and its
				// own flag.FlagSet doesn't discard -h output, so -h/--help is
				// intercepted here first, via a throwaway parse against the same
				// flag declarations (station.FlagSet) — see that function's doc
				// comment for why it's a second declaration rather than a shared
				// call.
				if err := station.FlagSet().Parse(args); errors.Is(err, flag.ErrHelp) {
					return HelpFor("station", out)
				}
				return station.Run(args)
			}),
		},
		{
			Name:     "register",
			Aliases:  []string{"register_agent"},
			Synopsis: "register [<alias>] [--role <role>] [--model claude|codex|cursor]",
			Summary:  "Register the current tmux session as an agent.",
			Help: `Alias precedence: the explicit argument, then $MUSTER_ALIAS, then the tmux
session name. Captures the calling session's project/pane/socket identity
from tmux (internal/tmuxenv) so other commands can address it.`,
			Group:    GroupIdentity,
			NewFlags: newRegisterFlags,
			Run:      adapt(cmdRegister),
		},
		{
			Name:     "become",
			Synopsis: "become <name> [--from <alias>] [--no-inject]",
			Summary:  "Claim a durable name for this session.",
			Help: `Claim this session's real name: a new alias inherits this session's
identity and inbox watermark, and the tmux-seeded alias retires — route
traffic by a name the work deserves. --from selects which of this session's
live aliases to claim from; required when the session has more than one.
When a live Claude Code or Cursor agent is registered in this session, also
types /rename <name> into its pane so the harness session name follows the
claim. --no-inject skips that typing — for callers whose name ALREADY came
from the harness side (e.g. the statusline promoting a name that originated
from a /rename), where re-typing it would loop text into a live pane.
A departed name may be reclaimed; a live one is refused (pick another name,
or purge it first with 'muster gc --purge-agents'). Reclaiming prints an
extra note — you inherit whatever inbox/history that name already carries.`,
			Group:    GroupIdentity,
			NewFlags: newBecomeFlags,
			Run:      adapt(cmdBecome),
		},
		{
			Name:     "deregister",
			Aliases:  []string{"deregister_agent"},
			Synopsis: "deregister [<alias>]",
			Summary:  "Remove an agent's registration.",
			Help:     `Alias precedence mirrors register: the explicit argument, then $MUSTER_ALIAS, then the tmux session name. A soft delete (tombstone) — see 'muster gc'.`,
			Group:    GroupIdentity,
			Run:      adapt(cmdDeregister),
		},
		{
			Name:     "label",
			Synopsis: "label [<name>] [--clear] [--no-inject]",
			Summary:  "Name or clear the current tmux session's label.",
			Help:     `Requires a tmux session ($TMUX set). Sets (or, with --clear or a bare 'muster label', clears) this session's addressable label in one command. When a live Claude Code or Cursor agent is registered in this session, also types /rename <name> into its pane so the harness session name follows. --no-inject skips that typing — for callers whose name ALREADY came from the harness side (e.g. the statusline promoting a name that originated from a /rename), where re-typing it would loop text into a live pane.`,
			Group:    GroupIdentity,
			NewFlags: newLabelFlags,
			Run:      adapt(cmdLabel),
		},
		{
			Name:     "whereami",
			Synopsis: "whereami [--json]",
			Summary:  "Print the tmux identity of the pane this process runs under.",
			Help: `Resolves via $TMUX when present, else by walking process ancestry (works
inside env-stripped harness hooks). For muster's own hooks and operator
convenience. Prints "socket=<path> session_id=<id> session_name=<name>
pane=<id> created=<ts>" (one line); --json emits the same fields as a JSON
object. Empty stdout and a nonzero exit when no pane resolves — never a
cwd guess.`,
			Group:    GroupIdentity,
			NewFlags: newWhereamiFlags,
			Run:      adapt(cmdWhereami),
		},
		{
			Name:     "device",
			Synopsis: "device [<name>]",
			Summary:  "Show or set this machine's device name.",
			Help: `With no argument, prints this machine's device name, where it came from,
and its device id. With one argument, sets the name.

A name is no longer something you must remember to set: the first time this
machine registers anything, muster adopts one automatically from the
hostname, reduced to lowercase letters, digits and dashes, and pins it to
disk. Running this command deliberately just replaces that adopted default
with one a human would actually say — "the ci-cd session on my work laptop"
— which is also what appears in the roster's DEVICE column so an agent can
resolve the phrase to an alias.

Once a name is in force, adopted or set, it prefixes every alias this
machine mints — derived, typed, or allocated — so two machines with the same
repos and tmux conventions can never silently take over each other's roster
row. The prefix is hidden on the machine that minted it (its own agents just
read as "dotfiles/main") and shown in full from every other machine
("work-laptop-dotfiles/main"), because that is the one context where the
prefix disambiguates something.

Setting writes <MUSTER_HOME>/device-name. That is a file rather than an
export on purpose: it survives reboots with no shell configuration, and
every process reads the same answer however it was launched.
$MUSTER_DEVICE_NAME overrides it for a single shell.

Existing roster rows keep the name and alias they registered with. Plain
re-registration after naming (auto-adopted or explicit) does NOT update
them — a seeded alias is a different alias, so it creates a second identity
and leaves the mail on the first. Use 'muster become <new-alias>' to carry
identity and inbox across.`,
			Group: GroupIdentity,
			Run:   adapt(cmdDevice),
		},
		{
			Name:     "gc",
			Synopsis: "gc [--events-keep <dur>] [--purge-agents]",
			Summary:  "Reap dead agents and prune old journal events.",
			Help: `Tombstones every agent whose tmux session is no longer alive (a soft
delete: identity and read-state survive as history), then prunes journal
events older than --events-keep (default 720h = 30 days). --purge-agents
instead hard-deletes every departed or currently-dead agent row —
irreversible, off by default.`,
			Group:    GroupIdentity,
			NewFlags: newGCFlags,
			Run:      adapt(cmdGC),
		},
		{
			Name:     "serve",
			Synopsis: "serve",
			Summary:  "Run the daemon (lazy unix-socket API server).",
			Help: `The daemon speaks newline-delimited JSON over a unix socket
(internal/proto) and is the one process every other muster command and the
MCP server talk to. Logs one line to stderr on startup, then blocks until
SIGINT/SIGTERM.`,
			Group: GroupPlumbing,
		},
		{
			Name:     "mcp",
			Synopsis: "mcp",
			Summary:  "Run the MCP stdio server for coding-agent tool use.",
			Help: `Exposes the daemon's operations as MCP tools over stdio, for a coding
agent (Claude Code, Codex, Cursor, ...) to call directly. stdout is the MCP
protocol channel — all diagnostics go to stderr.`,
			Group: GroupPlumbing,
		},
		{
			Name:     "channel",
			Synopsis: "channel",
			Summary:  "Run the MCP channel carrier that pushes new mail into this session.",
			Help: `A claude/channel MCP server over stdio for the session that launched it.
It serves muster_channel_status, which reports what the channel is attached
to. Under pi it also tails the bus journal for this tmux session's aliases
and pushes a compact envelope (who, intent, thread, subject) into the session
the moment mail lands — waking an idle agent without anyone typing. Under
Claude Code it serves the tool only: 'muster watch', which the muster plugin
runs, pushes the mail.

Bodies stay in the bus: the agent answers with get_thread / get_inbox / reply
as before. The 📬 badge, the Stop-hook drain and 'muster nudge' keep working
unchanged as fallbacks.

MUSTER_CHANNEL_INTERVAL tunes the poll cadence (Go duration, default 1s,
floor 250ms); MUSTER_CHANNEL_MAX_LISTED (default 5) caps how many threads a
batch push lists before collapsing to a count. A push is an envelope line,
a "---" line, then guidance for that event. stdout is the MCP protocol — diagnostics go to stderr.`,
			Group: GroupPlumbing,
		},
		{
			Name:     "watch",
			Synopsis: "watch",
			Summary:  "Print new mail for this session as JSON lines — the Claude Code plugin's feed.",
			Help: `Runs the same carrier as 'muster channel' for this tmux session's aliases
and prints each push as one JSON line {"text": ..., "content": ..., "meta":
{...}} on stdout. The muster plugin's mod runs it for a Claude Code session
and starts a turn with each line's text. To follow the whole bus yourself,
use 'muster tail'. Diagnostics go to stderr. Runs until signalled. In a
session pi's claude-bridge runs, it prints {"stop": ...} and exits 0.`,
			Group: GroupPlumbing,
		},
		{
			Name:     "lambda",
			Synopsis: "lambda",
			Summary:  "Serve the hosted bus from an AWS Lambda behind an HTTP API.",
			Help: `Runs the AWS Lambda runtime, adapting API Gateway requests to the same
daemon operations the unix socket serves, over a DynamoDB table
($MUSTER_DDB_TABLE) instead of SQLite. Requests carry a bearer token in the
Authorization header, matched against $MUSTER_TOKEN (and, during a rotation,
$MUSTER_TOKEN_PREVIOUS); with no token configured every request is rejected.
Only the Lambda release artifact is built with lambda mode compiled in (-tags
lambda) — the binary devices run reports that and exits.`,
			Group: GroupPlumbing,
		},
		{
			Name:     "hook",
			Synopsis: "hook <SessionStart|SessionEnd|Stop|UserPromptSubmit> [model]",
			Summary:  "Session-lifecycle hook entry point for agent harness configs.",
			Help: `The single entry point an agent harness's hook config (Claude Code,
Codex, Cursor, ...) points at directly — not normally typed by hand. SessionStart
registers, SessionEnd deregisters, Stop checks the calling tmux session's
inbox and, if there's unread mail, prints decision:block JSON telling the
agent to drain it. UserPromptSubmit marks the session working, and Stop marks
it idle unless it hands the agent mail. model defaults to "claude" when
omitted. Never blocks a session: every internal error is swallowed.`,
			Group: GroupPlumbing,
			Run:   adapt(func(args []string, out io.Writer) error { return cmdHook(args, os.Stdin, out) }),
		},
		{
			Name:     "setup",
			Synopsis: "setup [--agent claude|codex|cursor] [--no-hooks] [--tmux] [--dry-run] [--force] [--print kempt]",
			Summary:  "Configure your coding agents (Claude Code, Codex, Cursor) to use muster.",
			Help: `Registers muster's MCP server with each DETECTED agent, installs the
session-lifecycle hooks, and (with --tmux) renders the tmux mailbox — the
imperative fallback for people not using kempt, applying the exact same
configuration the kempt package declares so the two paths never drift.

For Claude Code it also enables the muster plugin (muster@subaud-tools, from
the subaud-tools/plugins marketplace) in ~/.claude/settings.json, which the
next claude session installs, and removes the old muster marketplace,
muster@muster and the old muster-channel server.

Detection is by config presence: Claude Code (~/.claude or ~/.claude.json),
Codex (~/.codex), Cursor (~/.cursor). Only detected agents are touched; with
none found, setup prints how to proceed and changes nothing.

Every file edit is an idempotent deep-merge that preserves existing content,
so re-running adds no duplicates. --dry-run prints every change without
writing. --no-hooks registers the MCP server only. --agent (repeatable or
comma-separated) limits to specific agents. --tmux (off by default) also adds
the mailbox lines to ~/.tmux.conf.

When kempt is installed, setup defers to it: it prints the [packages.muster]
block to add to your kempt.toml rather than editing files, unless --force is
given. --print kempt emits that same canonical block and exits. It requires
kempt >= 0.13.0 (schema 2). --print claude-hooks, codex-hooks or cursor-hooks
emits the generated contrib hooks without writing files.`,
			Group:    GroupPlumbing,
			NewFlags: newSetupFlags,
			Run:      adapt(cmdSetup),
		},
		{
			Name:     "debug",
			Synopsis: "debug <op> [key=value ...]",
			Summary:  "Send a raw op to the daemon (dev tool).",
			Help: `Bypasses muster's own subcommands and calls the daemon directly with an
arbitrary op and string key=value args, printing the raw JSON response. For
exploring or debugging the wire protocol — not part of the stable operator
surface.`,
			Group: GroupPlumbing,
		},
	}
}

// lookup finds a Registry command by name or alias.
func lookup(name string) (tools.Command, bool) {
	for _, c := range Registry {
		if c.Name == name {
			return c, true
		}
		for _, a := range c.Aliases {
			if a == name {
				return c, true
			}
		}
	}
	return tools.Command{}, false
}
