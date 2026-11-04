package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/subaud-tools/muster/internal/tmuxenv"
	"github.com/subaud-tools/tools-common/harness"
)

// cmdHook implements "muster hook <SessionStart|SessionEnd|Stop|UserPromptSubmit> [model]" —
// the single entry point an agent harness's hook config points at directly
// (in place of a copied contrib/muster-session-hook.sh). model defaults to
// "claude" when omitted.
//
// The stdin payload is read once here and handed to every branch: harnesses
// that host sessions outside tmux (daemon-hosted sessions — see tools-common/harness)
// leave tmuxenv.CaptureEnv empty, and the payload's session_id/cwd are then
// the ONLY identity a hook has, for SessionStart's register just as much as
// for Stop's inbox check.
//
// A hook must never block a session, so cmdHook always returns nil: every
// internal error is swallowed, and on any input other than a recognized
// event it is simply a no-op.
func cmdHook(args []string, stdin io.Reader, out io.Writer) error {
	if helpRequested(args) {
		return HelpFor("hook", out)
	}
	if len(args) < 1 {
		return fmt.Errorf("usage: muster hook <SessionStart|SessionEnd|Stop|UserPromptSubmit> [model]")
	}
	// A harness that spawns a nested Claude Code (or other hooked harness)
	// as a subprocess sets this in the child's env: the child inherits
	// $TMUX and the pane's process ancestry, so without the guard its
	// hooks reclaim and then tombstone the hosting pane's bus row on
	// every request (caught live 2026-08-27: pi-claude-bridge children
	// left a pi session permanently departed). Env scrubbing alone cannot
	// stop this — hookCapture falls back to an ancestry walk.
	if os.Getenv("MUSTER_HOOK_DISABLE") != "" {
		return nil
	}
	model := "claude"
	if len(args) > 1 && args[1] != "" {
		model = args[1]
	}
	payload, _ := io.ReadAll(io.LimitReader(stdin, 1<<20)) // a hook payload is small; the cap only guards a pathological writer
	// A fleet TEAMMATE's hooks are no-ops (teammate-identity-refusal spec
	// §3): a teammate is a full Claude session in a pane of some
	// primary's tmux session, so without this gate it races the primary
	// for the session's bus identity at every register, resume-reclaim,
	// projection, tombstone sweep, and stamp — and the pane-ownership
	// guard only protects a primary while its pane claim is provable
	// (caught live 2026-08-06: a teammate stamped its pane + harness id
	// onto two primaries' rows). Explicit registration (MCP
	// register_agent, `muster register`) is deliberately untouched — a
	// teammate can still be GIVEN an identity on purpose; what is barred
	// is automatic capture.
	//
	// TWO signals, in this order (spec §3a, after the v0.10.1 acceptance
	// failure). ARGV is authoritative: a teammate's claude process is
	// launched with `--agent-id <x> --team-name <y>` and the hook is its
	// descendant, so the marker exists from process birth and covers
	// SessionStart — the most damaging event, and the one the transcript
	// CANNOT cover, because the harness writes the transcript file only
	// after the SessionStart hooks have run (IsTeammate then fail-opens
	// on a missing file, which is exactly how the shipped gate was walked
	// through live). Both flags are required on ONE ancestor: either
	// alone can show up in a non-teammate's command line by coincidence
	// (a primary prompted with `claude -p "…--team-name…"`, a wrapper's
	// `sh -c`), while nothing but a teammate is LAUNCHED with the pair —
	// and a false positive here silently disables a primary's whole
	// identity machinery. The TRANSCRIPT scan stays as the belt: it
	// covers spawn shapes where the ancestry is unreadable — an async or
	// reparented hook, or a harness that launches teammates some other
	// way — on every later event once the file exists. Codex/Cursor
	// payloads carry no transcript_path and their processes carry
	// neither flag, so neither signal ever matches them.
	if tmuxenv.AncestorArgvContainsAll("--team-name", "--agent-id") ||
		harness.IsTeammate(harness.FromHookPayload(payload).TranscriptPath) {
		return nil
	}
	switch args[0] {
	case "SessionStart":
		c := hookCapture()
		h := harness.FromHookPayload(payload)
		var start struct {
			Source string `json:"source"`
		}
		_ = json.Unmarshal(payload, &start)
		if c.SocketPath != "" && c.PaneID != "" {
			// The resume path used to return early; it now only suppresses the
			// default session-name register (handled), so BOTH a reclaimed
			// conversation and a fresh one fall through to the projection —
			// resuming a named conversation anywhere re-asserts its name with
			// zero operator gestures (spec §4).
			handled := false
			if start.Source == "resume" {
				handled = hookSessionStartResume(c, h, model, out)
			}
			mayClaim := hookMayClaimIdentity(c)
			if !handled && mayClaim {
				hookRegisterPane(c, h, model)
			}
			// The projection carries the SAME pane-ownership gate as the
			// registration (the v0.7.1 rule: a sibling pane never speaks for
			// the session's identity). store.SetSessionLabel rewrites every
			// row on the tuple, so an ungated projection would let a subagent
			// pane's transcript durably rename the primary — and because
			// labels are addresses, sends to the stolen name would silently
			// misroute. A reclaimed conversation (handled) has proven its
			// identity by reclaiming, so it projects regardless.
			if handled || mayClaim {
				hookProjectName(c, harness.CustomTitle(h.TranscriptPath), out)
			}
		} else {
			hookSessionStartPaneless(h, model)
		}
	case "SessionEnd":
		// hookCapture, not tmuxenv.CaptureEnv: SessionEnd hooks run
		// env-stripped in production exactly like every other hook (see
		// hookCapture's doc comment), so CaptureEnv alone always came back
		// empty and every dying session took the paneless branch — the
		// ancestry-walk fallback is what lets a pane-anchored dying session
		// resolve ITS tuple, which is what scopes hookSessionEnd's tombstone
		// sweep to that tuple instead of every alias the harness session
		// ever registered (finding F2).
		hookSessionEnd(hookCapture(), harness.FromHookPayload(payload))
	case "Stop":
		hookStop(payload, out)
	case "UserPromptSubmit":
		// Prints nothing: Claude Code adds a UserPromptSubmit hook's stdout
		// to the prompt's context.
		hookActivity(harness.FromHookPayload(payload), "working")
	}
	return nil
}

// hookProjectName projects the conversation's user-set name (the transcript
// custom-title — see harness.CustomTitle) onto every naming surface at
// SessionStart: the tmux option pair (socket-aware — hooks run env-stripped,
// so an ambient set-option would land nowhere) and the stored bus label
// (manual, incarnation-scoped via set_label). This is the spec's
// conversation-as-identity payoff: resume nfl-3 anywhere and "send nfl-3"
// routes with zero gestures. Best-effort throughout — a failed write degrades
// to pre-projection behavior, never a wrong name. Empty title = no-op: never
// demote, never clear. A same-project manual holder elsewhere is warned
// about, never overwritten: the set_label write is tuple-scoped by
// construction, so stealing is impossible and the resolver's ambiguity error
// stays the enforcement (spec §4).
//
// No /rename injection (the one thing `muster label` also does): the name
// CAME from the harness side, so typing it back into the pane would loop it.
func hookProjectName(c tmuxenv.Capture, title string, out io.Writer) {
	if title == "" || c.SocketPath == "" || c.SessionID == "" {
		return
	}
	opt := tmuxenv.LabelOption()
	if err := tmuxenv.SetSessionOptionOn(c.SocketPath, c.SessionID, opt, title); err != nil {
		return // tmux unreachable: leave every surface as-is
	}
	_ = tmuxenv.SetSessionOptionOn(c.SocketPath, c.SessionID, opt+"_manual", "1")
	_ = tmuxenv.RefreshClientOn(c.SocketPath) // best-effort: repaint title bars
	if _, err := callData("set_label", map[string]any{
		"socket_path": c.SocketPath, "session_id": c.SessionID,
		"session_created": c.SessionCreated,
		"label":           title, "label_manual": true,
	}); err != nil {
		_, _ = fmt.Fprintf(out, "muster: session name %q set in tmux; bus sync failed (%v) — refreshes on next register\n", title, err)
		return
	}
	_, _ = fmt.Fprintf(out, "muster: session name %q → tmux label + bus (manual)\n", title)
	warnLabelCollision(title, c, out)
}

// warnLabelCollision surfaces (never resolves) a same-project manual-label
// holder on a different session: the human decides who renames.
func warnLabelCollision(title string, c tmuxenv.Capture, out io.Writer) {
	raw, err := callData("list_agents", nil)
	if err != nil {
		return
	}
	var agents []agentRow
	if json.Unmarshal(raw, &agents) != nil {
		return
	}
	var myProject string
	for _, a := range agents {
		if a.SocketPath == c.SocketPath && a.SessionID == c.SessionID && a.SessionCreated == c.SessionCreated && !a.Departed {
			myProject = a.Project
			break
		}
	}
	for _, a := range agents {
		if a.Departed || !a.LabelManual || a.Label != title || a.Project != myProject {
			continue
		}
		if a.SocketPath == c.SocketPath && a.SessionID == c.SessionID {
			continue // my own tuple
		}
		_, _ = fmt.Fprintf(out, "muster: note — label %q is also held by %s; sends to the bare label will error as ambiguous until one of you renames\n", title, a.Alias)
	}
}

// hookCapture resolves the tmux identity a hook acts on: the environment
// when the harness passed it through, else the process-ancestry walk —
// hooks are spawned env-stripped, but a synchronous hook is still a
// descendant of its pane's shell (see tmuxenv.CaptureFromAncestry). The
// zero Capture (both paths empty) means genuinely paneless.
func hookCapture() tmuxenv.Capture {
	if c := tmuxenv.CaptureEnv(); c.SocketPath != "" && c.PaneID != "" {
		return c
	}
	return tmuxenv.CaptureFromAncestry()
}

// hookRegisterPane registers the session-name alias for a pane-anchored
// SessionStart. It cannot delegate to cmdRegister: that reads the tmux
// identity from the ENVIRONMENT, which a stripped hook doesn't have — the
// capture c (env or ancestry walk) is the truth here.
func hookRegisterPane(c tmuxenv.Capture, h harness.Capture, model string) {
	alias := hookAlias(c)
	if alias == "" {
		return
	}
	_, _ = callData("register_agent", map[string]any{
		"alias": alias, "role": "", "model_type": model,
		"session_name": c.SessionName, "session_id": c.SessionID,
		"session_created":    c.SessionCreated,
		"harness_session_id": h.SessionID, "transcript_path": h.TranscriptPath,
		"socket_path": c.SocketPath, "pane_id": c.PaneID,
		"project": c.Project, "label": c.Label, "label_manual": c.LabelManual,
	})
}

// hookSessionStartResume reclaims a resumed conversation's aliases onto the
// tmux session it woke up in (durable-alias spec change 4). The harness
// session UUID is the lookup key — resume keeps it (only fork mints a new
// one) — and conversationRows returns every row this conversation ever
// registered. Each row is re-registered onto the CURRENT tuple (the revive
// path: read-state survives, the daemon reports outcome+unread), EXCEPT a
// row still live in a different, provably-alive tmux session — that is a
// real collision (the old side's SessionEnd reason:"resume" normally
// tombstones first), reported rather than clobbered — OR a departed row
// whose SupersededBy is set (store.Become stamps this on the seed in the
// same transaction that clones its identity onto the claimed alias): that
// row's identity was explicitly claimed away and must never resurrect on
// resume, no matter whether the row that claimed it is itself still live.
// Returns true when at least one alias was reclaimed: the caller then skips
// the default session-name register — the conversation's identity IS the
// reclaimed one, and minting a second alias would split it. Output goes to
// stdout, which the harness injects into the session's context: the agent
// wakes up knowing who it is and what's waiting.
//
// The printed unread count is session truth, not per-alias truth (finding
// F2, live rig): register_agent's ack.Unread is store.UnreadCount(alias) —
// text-matched to the single alias just reclaimed, blind to mail addressed
// to a superseded seed whose identity moved onto this same tuple (F1). Once
// at least one alias reclaims, session_unread's lineage-aware total (see
// store.SessionUnread) is queried ONCE for the current tuple and substituted
// into every printed line — a session's pending mail is one number, not a
// sum of per-alias guesses, so all reclaimed aliases share it. A
// session_unread failure degrades to the per-alias acks already collected,
// unchanged, rather than block the hook.
func hookSessionStartResume(c tmuxenv.Capture, h harness.Capture, model string, out io.Writer) bool {
	if h.SessionID == "" {
		return false
	}
	type reclaimedLine struct {
		alias, outcome string
		unread         int
	}
	var lines []reclaimedLine
	for _, ag := range conversationRows(h) {
		if ag.Departed && ag.SupersededBy != "" {
			continue // become-retired seed: SupersededBy already carries this identity forward, never resurrect it
		}
		sameTuple := ag.SocketPath == c.SocketPath && ag.SessionID == c.SessionID
		if !ag.Departed && !sameTuple && ag.SocketPath != "" &&
			tmuxenv.IsSessionAlive(ag.SocketPath, ag.SessionID, ag.SessionCreated) {
			_, _ = fmt.Fprintf(out, "muster: alias '%s' is still live in another tmux session — not reclaimed\n", ag.Alias)
			continue
		}
		ack := reclaimRow(ag, c, h, model)
		lines = append(lines, reclaimedLine{ag.Alias, ack.Outcome, ack.Unread})
	}
	if len(lines) == 0 {
		return false
	}
	if total, _, ok := sessionUnreadForHook(c.SocketPath, c.SessionID, c.SessionCreated); ok {
		for i := range lines {
			lines[i].unread = total
		}
	}
	for _, ln := range lines {
		_, _ = fmt.Fprint(out, reconnectLine(ln.alias, ln.outcome, ln.unread))
	}
	return true
}

// reconnectLine formats one resume line of hookSessionStartResume's output.
// Pulled out to a named function so it is directly testable: this text is
// injected into an agent's context (the harness pipes hook stdout back into
// the conversation), which makes it a MODEL surface — unlike the CLI/station
// display helpers, it must carry the FULL stored alias, never the
// device-stripped short form a human-facing surface would show. See
// mcpserver.TestModelSurfacesKeepTheFullAlias for why: a short alias here
// would re-resolve against whatever device the model's own machine is, not
// the one that minted it, and silently reach a different, real agent.
func reconnectLine(alias, outcome string, unread int) string {
	return fmt.Sprintf("muster: reconnected as '%s' (%s) — %d unread thread(s); call get_inbox with alias '%s'\n",
		alias, outcome, unread, alias)
}

// hookSessionStartPaneless auto-registers a session that has no tmux pane in
// its environment (harness daemon-hosted sessions) on the paneless tuple
// ("", harness session UUID). $MUSTER_ALIAS is explicit operator intent and
// registers exactly (plain upsert). Otherwise the alias is ALLOCATED from
// the payload cwd's basename via allocPanelessAlias — every session in a
// directory derives the same base, so uniqueness (dotfiles, dotfiles-2, …)
// must be allocated, never taken over: the takeover this replaces let a
// second session in the same directory silently steal the first one's
// identity and inbox. A session that already owns aliases on its tuple
// (resume) refreshes the first instead of allocating a new one.
func hookSessionStartPaneless(h harness.Capture, model string) {
	regFn := func(alias string, ifAbsent bool) error {
		_, err := callData("register_agent", registerPanelessArgs(alias, "", model, h, ifAbsent))
		return err
	}
	if alias := os.Getenv("MUSTER_ALIAS"); alias != "" {
		_ = regFn(seedAlias(alias), false)
		return
	}
	if h.SessionID == "" && h.Alias() == "" {
		return // no resolvable identity: never dial the daemon from an identity-less hook
	}
	if owned := conversationRows(h); len(owned) > 0 {
		// This session already has an identity — the pane-side launch
		// handshake pre-registered a tmux-anchored row, or a prior life of
		// this session (resume) left one. A live row needs nothing from
		// this hook; if every owned row is a tombstone, revive the first
		// UNSUPERSEDED one with its stored identity intact (finding F1: a
		// become-retired seed must never be revived under its old alias —
		// firstUnsuperseded skips it). If every owned row turns out to be
		// superseded, there is no identity left to revive; fall through to
		// allocation below exactly as if owned had been empty.
		for _, ag := range owned {
			if !ag.Departed {
				return
			}
		}
		if ag, ok := firstUnsuperseded(owned); ok {
			reviveRow(ag, h, model)
			return
		}
	}
	// Same rule as cmdRegister's paneless fallback: the base is a raw cwd
	// basename, identical on any two machines with the same directory
	// checked out, so it is seeded before allocation like every other mint
	// site — this is the SessionStart hook's own paneless registration path,
	// the way daemon-hosted (no tmux pane) sessions actually register in
	// production.
	_, _ = allocPanelessAlias(seedAlias(h.Alias()), h.SessionID, regFn)
}

// hookAlias resolves the identity a hook event acts on, mirroring
// cmdRegister/cmdDeregister's no-arg precedence: $MUSTER_ALIAS, else the
// captured tmux session name.
//
// Both branches seed. Every alias this machine mints is seeded — there is no
// carve-out for an explicit choice, because once the prefix is hidden locally
// an operator cannot tell a device-scoped name from an unqualified one, and a
// silent exception here would be invisible at the moment it mattered.
func hookAlias(c tmuxenv.Capture) string {
	if v := os.Getenv("MUSTER_ALIAS"); v != "" {
		return seedAlias(v)
	}
	return seedAlias(c.SessionName)
}

// hookGetAgent fetches an alias's full roster row via the daemon's get_agent
// op, decoded exactly like cmdNudge's pane resolution. The three results are
// deliberately distinct (spec §3a): found=true with the row; found=false with
// err=nil when the daemon ANSWERED "no such alias"; and err!=nil when the
// roster could not be consulted at all (dial failure mid-daemon-restart,
// daemon error, undecodable reply). "Absent" and "unknown" are different
// facts — collapsing them is what let a foreign pane claim a live identity
// during the v0.10.1 acceptance run — so every caller decides for itself
// which way an unanswerable roster falls.
func hookGetAgent(alias string) (agentFull, bool, error) {
	raw, err := callData("get_agent", map[string]any{"alias": alias})
	if err != nil {
		return agentFull{}, false, err
	}
	var res struct {
		Found bool      `json:"found"`
		Agent agentFull `json:"agent"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return agentFull{}, false, err
	}
	if !res.Found {
		return agentFull{}, false, nil
	}
	return res.Agent, true, nil
}

// hookMayClaimIdentity is the SessionStart gate (spec: first live claimant
// wins the session's primary-agent pane). Degrades to true — today's
// register — whenever tmux identity can't answer, or the roster answers that
// nothing live holds the name.
//
// A roster that cannot answer at all is the one case that falls the other way
// (spec §3a): an unreachable daemon is not evidence of a free identity, and
// this is exactly how the v0.10.1 live acceptance failed — the installer's
// daemon restart had the daemon down for the instant a foreign pane's
// SessionStart ran its check, the dial error read as "no row", and the pane
// claimed the primary's identity. Declining to write costs nothing: a hook
// never blocks a session either way, primaries re-register at every
// SessionStart, and the very next hook event re-runs this check once the
// daemon is back.
func hookMayClaimIdentity(c tmuxenv.Capture) bool {
	if c.SocketPath == "" || c.PaneID == "" {
		return true
	}
	ag, found, err := hookGetAgent(hookAlias(c))
	if err != nil {
		return false
	}
	if !found {
		return true
	}
	if ag.Departed {
		return true // a tombstone never owns the identity
	}
	if ag.SocketPath != c.SocketPath || ag.SessionID != c.SessionID {
		return true // cross-session takeover: a renamed/recreated session reclaims its name
	}
	if ag.PaneID == "" || ag.PaneID == c.PaneID {
		return true
	}
	return !tmuxenv.IsPaneAlive(c.SocketPath, ag.PaneID)
}

// hookSessionEnd deregisters EVERY alias the dying pane owns, not just the
// one hookAlias resolves. A session that registered a custom alias (via the
// register_agent tool or `muster register <alias>`) on top of its
// session-name alias used to leave the custom one behind: @muster_agent
// lives on the tmux session, which outlives the Claude session, so the next
// agent in that pane silently inherited the leftover identity AND its inbox
// — the Stop hook then instructed it to act on another agent's mail (the
// lake-broker incident, thread 85). Ownership is judged per alias with the
// same predicate hookOwnsIdentity applies to one: same (socket_path,
// session_id) tuple, not departed, and the row's pane is unset or this pane
// — so a dying sibling (subagent) pane still cannot tombstone the primary's
// registrations. Outside tmux the paneless tuple ("", harness session UUID)
// is enumerated the same way — every alias this harness session registered is
// tombstoned; with no harness identity either, the single-identity gate is
// all there is, exactly as before.
func hookSessionEnd(c tmuxenv.Capture, h harness.Capture) {
	if c.SocketPath == "" || c.SessionID == "" {
		if h.SessionID != "" {
			hookSessionEndPaneless(h)
			return
		}
		if hookOwnsIdentity(c) {
			_ = cmdDeregister(nil, io.Discard)
		}
		return
	}
	raw, err := callData("list_agents", nil)
	if err != nil {
		return // hooks never block a session on a dead daemon
	}
	var rows []agentRow
	if json.Unmarshal(raw, &rows) != nil {
		return
	}
	for _, ag := range rows {
		if ag.Departed || ag.SocketPath != c.SocketPath || ag.SessionID != c.SessionID {
			continue
		}
		if ag.PaneID != "" && ag.PaneID != c.PaneID {
			continue // a sibling pane's identity: not ours to tombstone
		}
		// ag.Alias is already the exact stored alias for this row — route
		// through deregisterAlias, not cmdDeregister, so it is never
		// re-expanded local-first into an unrelated live agent's alias (see
		// deregisterAlias's doc comment).
		_ = deregisterAlias(ag.Alias, io.Discard)
	}
}

// hookSessionEndPaneless tombstones every row the dying harness session owns
// — handshake-registered tmux rows and paneless rows alike (conversationRows'
// two match shapes). Rows of other sessions, or with no harness link at all
// (pre-handshake registrations), are never this session's to tombstone.
//
// Belt-and-suspenders (finding F2): conversationRows keys purely on the
// harness session UUID, with no tuple discrimination. In the resume
// coexistence window — SessionEnd(reason:"resume") fires for the OLD side
// while the NEW side has already registered its row under the SAME UUID on
// a NEW tuple — that lookup returns both, and an unqualified sweep would
// tombstone the new side's live registration out from under it. A row still
// provably alive on its own tmux tuple (mirroring the reclaim collision
// check in hookSessionStartResume) is therefore skipped: it belongs to
// someone else's still-running session, not this dying one.
func hookSessionEndPaneless(h harness.Capture) {
	for _, ag := range conversationRows(h) {
		if ag.Departed {
			continue
		}
		if ag.SocketPath != "" && tmuxenv.IsSessionAlive(ag.SocketPath, ag.SessionID, ag.SessionCreated) {
			continue
		}
		// ag.Alias is already the exact stored alias for this row — see the
		// tmux sweep above for why this must not go through cmdDeregister.
		_ = deregisterAlias(ag.Alias, io.Discard)
	}
}

// hookOwnsIdentity is the SessionEnd gate for the no-tmux fallback (and the
// single-identity ownership predicate hookSessionEnd generalizes): deregister
// only what this pane owns — a dying sibling (subagent) must not tombstone
// the primary.
func hookOwnsIdentity(c tmuxenv.Capture) bool {
	if hookAlias(c) == "" {
		// No resolvable identity (global hooks, non-tmux sessions): nothing to
		// deregister, and never dial the daemon from an identity-less hook.
		return false
	}
	// An unanswerable roster falls the same way as an absent row here, and
	// that is already fail-CLOSED: with no proof of ownership this hook
	// deregisters nothing. Same posture as hookMayClaimIdentity — decline to
	// write — reached from the opposite direction.
	ag, found, err := hookGetAgent(hookAlias(c))
	if err != nil || !found {
		return false
	}
	if ag.Departed {
		return false // a tombstone is already gone: nothing live to deregister
	}
	if ag.SocketPath != c.SocketPath || ag.SessionID != c.SessionID {
		return false
	}
	return ag.PaneID == "" || ag.PaneID == c.PaneID
}

// stopInput decodes the Stop-hook stdin payload. Invalid or empty JSON leaves
// it at its zero value, matching the contrib script's tolerant
// `jq -r '.stop_hook_active // false'`. Cursor uses loop_count and status for
// the same loop guard.
type stopInput struct {
	StopHookActive bool   `json:"stop_hook_active"`
	LoopCount      int    `json:"loop_count"`
	Status         string `json:"status"`
}

// stopReason is the JSON payload muster prints on stdout for a Stop hook
// finding unread mail: {"decision":"block","reason":"..."}. Claude Code and
// Codex both treat decision:block as "run reason as the next prompt"; Cursor
// accepts the same shape (with stopInput's loop guard preventing a loop).
type stopReason struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}

// hookStop ports contrib/muster-session-hook.sh's Stop branch to Go, with
// identical semantics: a self-resolving inbox check. If this tmux session has
// unread muster mail (the @muster_inbox option the daemon sets), it prints one
// line of decision:block JSON telling the agent to drain its inbox
// autonomously; otherwise it prints nothing. Best-effort throughout — stdin
// read/decode failures, a missing tmux, or a missing/non-numeric/zero count
// all fall through to "print nothing".
//
// Identity resolution mirrors hookCapture's env-else-walk (finding F1): a
// harness that passed $TMUX through gets the exact ambient-query behavior
// this hook has always had (hookStopTmuxEnv); a harness that stripped it
// (the production case — every hook is spawned env-stripped) falls to the
// process-ancestry walk (hookStopWalked), and only when BOTH come back empty
// does the session count as genuinely paneless (hookStopPaneless). Before
// this fix, hookStop gated on the literal $TMUX env var alone, so it ALWAYS
// took the paneless branch in production and stampHarnessLinks (the resume
// spec's repair path, below) never ran.
//
// Every Stop that does not hand the agent mail marks the session idle
// (hookActivity), the loop-guard and aborted/error returns included: the
// session really is stopping. A Stop that prints a block decision keeps the
// agent going, so it leaves the session working. The idle mark is a
// separate daemon call from the drain, so the cheap @muster_inbox gate still
// decides whether Stop drains, but every Stop now dials the daemon.
func hookStop(payload []byte, out io.Writer) {
	h := harness.FromHookPayload(payload)
	if !hookStopBlocks(payload, h, out) {
		hookActivity(h, "idle")
	}
}

// hookStopBlocks is hookStop's drain decision; it reports whether it printed
// a block decision.
func hookStopBlocks(payload []byte, h harness.Capture, out io.Writer) bool {
	var in stopInput
	_ = json.Unmarshal(payload, &in) // invalid/empty JSON -> zero value (false)
	if in.StopHookActive || in.LoopCount > 0 {
		return false // loop guard: we already triggered a continuation this cycle
	}
	if in.Status == "aborted" || in.Status == "error" {
		return false
	}
	if os.Getenv("TMUX") != "" {
		return hookStopTmuxEnv(h, out)
	}
	if c := tmuxenv.CaptureFromAncestry(); c.SocketPath != "" && c.PaneID != "" {
		return hookStopWalked(c, h, out)
	}
	return hookStopPaneless(h, out)
}

// hookActivity reports this session working or idle for every alias it
// owns, resolved the way hookStop resolves its session: the ambient tmux
// session, else the ancestry walk's, else the harness conversation's live
// rows. Best-effort: any failure is swallowed, and nothing is printed.
func hookActivity(h harness.Capture, state string) {
	var aliases []string
	if os.Getenv("TMUX") != "" {
		aliases = sessionAliasesForHook(tmuxenv.SocketFromEnv(), tmuxenv.CurrentSessionID(), tmuxenv.CurrentSessionCreated())
	} else if c := tmuxenv.CaptureFromAncestry(); c.SocketPath != "" && c.PaneID != "" {
		aliases = sessionAliasesForHook(c.SocketPath, c.SessionID, c.SessionCreated)
	} else {
		for _, ag := range conversationRows(h) {
			if !ag.Departed {
				aliases = append(aliases, ag.Alias)
			}
		}
	}
	if len(aliases) == 0 {
		return
	}
	_, _ = callData("set_activity", map[string]any{"aliases": aliases, "state": state})
}

// hookStopTmuxEnv is hookStop's tmux path when the harness passed $TMUX
// through — unchanged from before finding F1: the (socket_path, session_id)
// tuple and the @muster_inbox cheap gate both read from the AMBIENT tmux
// session (no -S/-t), relying on $TMUX/$TMUX_PANE in the process
// environment. The incarnation the daemon queries by comes from the same
// ambient read (tmuxenv.CurrentSessionCreated) — the walked path below gets
// it from its Capture instead.
func hookStopTmuxEnv(h harness.Capture, out io.Writer) bool {
	optCount, err := strconv.Atoi(tmuxenv.CurrentSessionOption("@muster_inbox"))
	if err != nil || optCount <= 0 {
		return false // cheap gate: no daemon calls unless the tmux option says there's mail
	}
	socketPath := tmuxenv.SocketFromEnv()
	sessionID := tmuxenv.CurrentSessionID()
	label := tmuxenv.CurrentSessionOption(tmuxenv.LabelOption())
	return hookStopDrain(socketPath, sessionID, tmuxenv.CurrentSessionCreated(), os.Getenv("TMUX_PANE"), optCount, label, h, out)
}

// hookStopWalked is finding F1's new branch: the harness stripped $TMUX from
// the hook's environment, but the process-ancestry walk (the same fallback
// hookCapture uses) still located the pane. tmuxenv.CurrentSessionOption,
// SocketFromEnv, and CurrentSessionID are all env-dependent and would read
// nothing here — every tmux read instead goes through the socket-aware query
// seam (tmuxenv.SessionOption / SessionLabel) against the WALKED capture's
// own tuple, never the (absent) ambient environment.
func hookStopWalked(c tmuxenv.Capture, h harness.Capture, out io.Writer) bool {
	optCount, err := strconv.Atoi(tmuxenv.SessionOption(c.SocketPath, c.PaneID, "@muster_inbox"))
	if err != nil || optCount <= 0 {
		return false // cheap gate: no daemon calls unless the tmux option says there's mail
	}
	label, _ := tmuxenv.SessionLabel(c.SocketPath, c.PaneID)
	return hookStopDrain(c.SocketPath, c.SessionID, c.SessionCreated, c.PaneID, optCount, label, h, out)
}

// hookStopDrain is the daemon-facing core shared by hookStopTmuxEnv and
// hookStopWalked: given a resolved (socket_path, session_id, pane) tuple and
// the cheap gate's option count, it asks the daemon for the session's full
// alias list (session_aliases) and its true unread/action counts
// (session_unread) — a session with a split identity (a tmux-name alias plus
// a chosen one) must drain both, not just the alias the hook happened to
// read the option from. Either call's failure (or an empty alias list) falls
// back to today's single session-name behavior so the hook never goes silent
// because of a daemon hiccup.
func hookStopDrain(socketPath, sessionID string, sessionCreated int64, myPane string, optCount int, label string, h harness.Capture, out io.Writer) bool {
	total, action, ok := sessionUnreadForHook(socketPath, sessionID, sessionCreated)
	if !ok {
		total, action = optCount, 0 // fall back to the tmux option value on op failure
	}
	if total <= 0 {
		return false
	}
	aliases := sessionAliasesForHook(socketPath, sessionID, sessionCreated)

	// Repair missing harness links (durable-alias spec: the stamp's Stop
	// half). An alias registered via the MCP tool in an env carrying no
	// harness UUID has no link for a future resume to find; every Stop
	// payload carries the UUID, so stamp it here. This runs only when the
	// mail gate above already opened — the cheap @muster_inbox check stays
	// the sole decider of whether Stop dials the daemon at all, so a
	// mail-less session costs nothing (documented residual: a link-less
	// alias that never receives mail is never auto-stamped and rides the
	// re-register-by-transcript contract instead).
	stampHarnessLinks(aliases, h, socketPath, sessionID, myPane)

	if !hookStopOwnsAnyAlias(aliases, myPane) {
		return false // roster names a live owner and it isn't me: don't drain a sibling's mail
	}

	reason := hookReason(total, action, aliases, label)
	b, err := json.Marshal(stopReason{Decision: "block", Reason: reason})
	if err != nil {
		return false
	}
	_, _ = fmt.Fprintln(out, string(b)) // best-effort: a hook's stdout write failing has nowhere to report to
	return true
}

// hookStopPaneless is the Stop-hook inbox check for sessions with no tmux in
// their environment (harness daemon-hosted sessions). Such a session has no
// @muster_inbox tmux option to serve as the cheap firing gate, so this dials
// the daemon directly on every Stop — two ops over a local unix socket, the
// price of daemon-hosted mail. Identity resolves by harness session UUID
// (conversationRows): handshake-registered tmux rows and paneless rows
// alike. An unregistered session prints nothing, and unlike the tmux path
// there is no session-name fallback to address — a harness identity either
// resolved from the roster or doesn't exist.
func hookStopPaneless(h harness.Capture, out io.Writer) bool {
	if h.SessionID == "" {
		return false
	}
	var aliases []string
	var label string
	var tup *agentRow
	owned := conversationRows(h)
	for i, ag := range owned {
		if ag.Departed {
			continue
		}
		aliases = append(aliases, ag.Alias)
		if label == "" && ag.LabelManual {
			label = ag.Label
		}
		if tup == nil && ag.SocketPath != "" && ag.SessionID != "" {
			tup = &owned[i]
		}
	}
	if len(aliases) == 0 {
		return false
	}
	// Unread comes from ONE tuple: the handshake row's tmux tuple when the
	// session has one (its mail lives there), else the paneless tuple. A
	// split identity spanning both would need two queries whose overlapping
	// threads can't be deduplicated client-side — the aliases list still
	// names every identity, so nothing goes undrained.
	sock, sid, created := "", h.SessionID, int64(0)
	if tup != nil {
		sock, sid, created = tup.SocketPath, tup.SessionID, tup.SessionCreated
	}
	total, action, ok := sessionUnreadForHook(sock, sid, created)
	if !ok || total <= 0 {
		return false
	}
	b, err := json.Marshal(stopReason{Decision: "block", Reason: hookReason(total, action, aliases, label)})
	if err != nil {
		return false
	}
	_, _ = fmt.Fprintln(out, string(b)) // best-effort: a hook's stdout write failing has nowhere to report to
	return true
}

// stampHarnessLinks attaches h.SessionID to every alias of MY tuple that
// lacks a harness link. Best-effort per alias: a failed get or stamp is
// skipped — hooks never block a session.
//
// myPane scopes the stamp to panes THIS hook actually is (finding F3): two
// agent panes can share one tmux session, and without the pane check pane
// A's Stop would stamp A's harness UUID onto sibling pane B's link-less
// alias — mirroring the pane predicate hookOwnsIdentity already applies to
// deregistration. A row with no stored pane_id carries no ownership
// information (same convention as hookOwnsIdentity/hookStopOwnsAnyAlias) and
// is never skipped on that basis alone.
func stampHarnessLinks(aliases []string, h harness.Capture, socketPath, sessionID, myPane string) {
	if h.SessionID == "" {
		return
	}
	for _, alias := range aliases {
		// Unanswerable roster ⇒ skip this alias: the stamp is a WRITE, and
		// stamping a row whose ownership fields could not be read is the
		// same class of mistake as claiming an identity blind.
		ag, ok, err := hookGetAgent(alias)
		if err != nil || !ok || ag.Departed ||
			ag.SocketPath != socketPath || ag.SessionID != sessionID {
			continue
		}
		if ag.HarnessSessionID == h.SessionID && ag.TranscriptPath == h.TranscriptPath {
			continue // already stamped with both current links: nothing to do
		}
		if ag.PaneID != "" && ag.PaneID != myPane {
			continue // a sibling pane's alias: not mine to stamp
		}
		// Finding 6: a row with an EMPTY pane_id carries no pane-level proof
		// of ownership (the same convention hookOwnsIdentity/
		// hookStopOwnsAnyAlias use), so the pane check above cannot rule out
		// a sibling pane sharing this tuple. Task 8 needs the stamp to be
		// able to UPDATE an existing link — a resumed conversation legitimately
		// gets a new harness_session_id while its transcript_path stays put —
		// but that repair must be provably THIS conversation, not just any
		// pane in the session. Restore the repair-only posture for exactly
		// the ambiguous (empty pane_id) case: stamp a fresh link freely, but
		// only overwrite an EXISTING one when the caller's transcript proves
		// it. A pane-scoped row (PaneID == myPane) already has real proof and
		// skips this check entirely.
		if ag.PaneID == "" && ag.HarnessSessionID != "" && ag.TranscriptPath != h.TranscriptPath {
			continue // an unrelated sibling: don't clobber another row's proven link
		}
		_, _ = callData("stamp_harness_session", map[string]any{
			"alias": alias, "harness_session_id": h.SessionID, "transcript_path": h.TranscriptPath,
		})
	}
}

// sessionAliasesForHook calls the session_aliases op and returns the sorted,
// deduplicated alias list for the (socket_path, session_id) tuple. Any
// transport/daemon failure or an empty result falls back to a single-element
// list holding today's session-name wording (spec §3) — the hook always has
// something to address.
//
// That fallback is SEEDED, because "session name == alias" is no longer true:
// registration mints seedAlias(<session name>), so the bare name names no
// roster row. Both consumers need the stored form — hookReason's "call
// get_inbox with alias '%s'" is a MODEL surface, and hookStopOwnsAnyAlias
// looks the alias up in the roster. Minting it the same way registration does
// is what makes the guess land on the row registration created; device.Seed's
// empty-alias guard keeps an unreachable tmux from producing a bare
// "<device>-".
//
// sessionCreated is the caller's live incarnation, forwarded so the daemon
// scopes the lineage walk to it (spec §5.1): a legacy row stranded on a
// recycled session ID is not this conversation's identity and must not be
// named in the drain wording. 0 (a paneless tuple, or tmux not answering) is
// the paneless/no-proof value the op already documents.
func sessionAliasesForHook(socketPath, sessionID string, sessionCreated int64) []string {
	raw, err := callData("session_aliases", map[string]any{"socket_path": socketPath, "session_id": sessionID, "session_created": sessionCreated})
	if err == nil {
		var res struct {
			Aliases []string `json:"aliases"`
		}
		if json.Unmarshal(raw, &res) == nil && len(res.Aliases) > 0 {
			return res.Aliases
		}
	}
	return []string{seedAlias(tmuxenv.CurrentSessionName())}
}

// sessionUnreadForHook calls the session_unread op. ok is false on any
// transport/daemon failure, signaling the caller to fall back to the
// @muster_inbox option's count (with no action-count breakdown available).
// sessionCreated carries the caller's live incarnation, exactly as in
// sessionAliasesForHook.
func sessionUnreadForHook(socketPath, sessionID string, sessionCreated int64) (total, action int, ok bool) {
	raw, err := callData("session_unread", map[string]any{"socket_path": socketPath, "session_id": sessionID, "session_created": sessionCreated})
	if err != nil {
		return 0, 0, false
	}
	var res struct {
		Total  int `json:"total"`
		Action int `json:"action"`
	}
	if json.Unmarshal(raw, &res) != nil {
		return 0, 0, false
	}
	return res.Total, res.Action, true
}

// hookStopOwnsAnyAlias is the Stop gate (spec §2): drain only when this pane
// is a named owner. It resolves each alias's stored pane_id via get_agent —
// cheap, local, at most a couple of daemon round trips — and engages ONLY
// when the roster actually names a live pane_id for at least one alias and
// none of them is mine. An empty myPane, an unresolvable roster, or every
// alias having no stored pane_id all mean the roster can't name an owner, so
// this falls through to true — today's unconditional drain. myPane is the
// caller's resolved pane (ambient $TMUX_PANE, or the ancestry walk's pane for
// an env-stripped hook) — not read from the environment here, so the walked
// path (finding F1) can supply its own.
func hookStopOwnsAnyAlias(aliases []string, myPane string) bool {
	if myPane == "" {
		return true
	}
	sawNamedOwner := false
	for _, alias := range aliases {
		// An unanswerable roster is treated exactly like a row that names no
		// owner: it neither grants nor denies. This gate guards a READ (should
		// this pane drain its mail), and if the daemon is down the drain the
		// wake text asks for will simply find nothing — no write, nothing to
		// steal, so the pre-existing permissive posture stands.
		ag, found, err := hookGetAgent(alias)
		if err != nil || !found || ag.Departed || ag.PaneID == "" {
			continue // a tombstoned row neither grants nor denies ownership
		}
		if ag.PaneID == myPane {
			return true
		}
		sawNamedOwner = true
	}
	return !sawNamedOwner
}

// hookReason builds the Stop hook's decision:block reason. When the session
// carries a task label (the operator's chosen name — prefix T / `muster
// label`), the reason leads with it ("You are 'standard 2000'") so the agent
// learns the label vocabulary the operator thinks in; the alias stays in the
// tool instructions because aliases are what the tools accept. An empty
// label renders the pre-label wording unchanged. The instruction line is
// singular for exactly one alias and a for-each across all of them when the
// session has more than one — a split-identity session must drain every
// alias. Both variants end with the CLI fallback (cliFallback).
func hookReason(total, action int, aliases []string, label string) string {
	countLine := fmt.Sprintf("You have %d unread muster thread(s)", total)
	if action > 0 {
		countLine += fmt.Sprintf(", %d needing action", action)
	}

	if len(aliases) <= 1 {
		alias := ""
		if len(aliases) == 1 {
			alias = aliases[0]
		}
		identity := fmt.Sprintf("Your muster alias is '%s' (this tmux session).", alias)
		if label != "" {
			identity = fmt.Sprintf("You are '%s' — muster alias '%s' (this tmux session).", label, alias)
		}
		return fmt.Sprintf(
			"%s. %s "+
				"Call your muster get_inbox tool now with alias '%s', read each new thread with get_thread, "+
				"handle the request, and reply with the muster reply tool only if the sender needs something from you "+
				"(never reply just to acknowledge an ack or closure; close out with fyi=true so nobody is woken). "+
				"Act autonomously — do not ask the user. "+
				cliFallback,
			countLine, identity, alias, alias, alias,
		)
	}

	quoted := make([]string, len(aliases))
	for i, a := range aliases {
		quoted[i] = "'" + a + "'"
	}
	identity := fmt.Sprintf("Your muster aliases are %s (this tmux session).", strings.Join(quoted, ", "))
	if label != "" {
		identity = fmt.Sprintf("You are '%s' — muster aliases %s (this tmux session).", label, strings.Join(quoted, ", "))
	}
	return fmt.Sprintf(
		"%s. %s "+
			"For EACH alias call get_inbox, read each new thread with get_thread, handle the request, "+
			"and reply with the muster reply tool only if the sender needs something from you "+
			"(never reply just to acknowledge an ack or closure; close out with fyi=true so nobody is woken). "+
			"Act autonomously — do not ask the user. "+
			cliFallback,
		countLine, identity, "<alias>", "<alias>",
	)
}

// cliFallback is the shell-equivalent loop appended to every hook reason.
// %s is the alias to drain (the literal placeholder "<alias>" in the
// multi-alias variant, where the agent substitutes each of its own).
const cliFallback = "(If the muster MCP tools are unavailable, the muster CLI is equivalent: " +
	"`muster inbox '%s'`, `muster thread <id>`, `muster reply <id> \"...\" --from '%s' [--fyi]`.)"
