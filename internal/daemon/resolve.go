package daemon

import (
	"fmt"
	"time"

	"github.com/subaud-tools/muster/internal/device"
	"github.com/subaud-tools/muster/internal/resolve"
	"github.com/subaud-tools/muster/internal/store"
)

// requireKnownAlias is resolveAgentTarget's counterpart for an ACTOR alias —
// task_claim/task_transition's `by` and get_inbox's `alias`. Those name who is
// acting or whose mail is being read, not who is being addressed, so no label
// or project scoping applies to them: an actor alias is an alias, exactly.
//
// It does two things, and both are needed because MCP passes these fields
// straight through with no client-side resolution (spec §3 names them as
// expansion sites):
//
//   - EXPAND, local-first, exactly as resolveAgentTarget does: try
//     <device>-<given>, and take it only when that alias already exists in the
//     roster. Without this a model that read a short alias off a human surface
//     addresses an alias nobody holds. "" as the device name disables
//     expansion (Lambda mode, which serves many devices and must never guess
//     one) without disabling the existence check below.
//
//   - REQUIRE the result to name a roster row. What follows a successful
//     return is durable and unreported otherwise: ClaimTask writes `by`
//     verbatim into entries.from_agent with no existence check of its own, so
//     an unknown actor is permanently recorded as the claimer of a task nobody
//     claimed, and notifyForThread then fans out to it; get_inbox answers with
//     an empty thread list, which a model reads as "no mail" forever rather
//     than as "that is not an agent". Both are silent wrongness, and this
//     branch prefers loud (see resolveAgentTarget's black-hole check, the same
//     argument for the addressee half).
//
// A DEPARTED row still passes: a tombstone is a roster row, its leftover mail
// still needs draining, and the agent draining it may still close out that
// identity's tasks. Only an alias with no row at all is refused.
//
// The error names what was actually sent, never the seeded guess — the caller
// never wrote that string, and an error quoting it sends them looking for a
// typo they did not make.
func (d *Daemon) requireKnownAlias(field, given string) (string, error) {
	if given == "" {
		return "", fmt.Errorf("%s is required", field)
	}
	if d.deviceName != "" {
		if seeded := device.Seed(d.deviceName, given); seeded != given {
			_, found, err := d.s.GetAgent(seeded)
			if err != nil {
				return "", err
			}
			if found {
				return seeded, nil
			}
		}
	}
	_, found, err := d.s.GetAgent(given)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("no agent registered as %q", given)
	}
	return given, nil
}

// refuseEndedRecipient refuses a request (reply- or action-requested; a task
// with no intent counts as action-requested, a message with none as fyi, as
// the stores' effectiveIntent rule says) to an alias whose row is departed.
// Requests to departed sessions were never answered (5 of 5, 30 days to
// 2026-10-22); fyi still waits for the alias to return.
func (d *Daemon) refuseEndedRecipient(kind, intent, alias string) error {
	if kind == "task" && intent == "" {
		intent = store.IntentAction
	}
	if intent != store.IntentReply && intent != store.IntentAction {
		return nil
	}
	ag, found, err := d.s.GetAgent(alias)
	if err != nil {
		return err
	}
	if !found || !ag.Departed {
		return nil
	}
	lastSeen := time.UnixMilli(ag.LastSeen).UTC().Format("2006-01-02T15:04Z")
	return fmt.Errorf("%s ended (last seen %s) and can't answer a request. Send it to a live session (list_agents shows who is live), or send it as fyi to leave it waiting", alias, lastSeen)
}

// resolveAgentTarget resolves to_target for a send_message/task_create op
// whose to_kind=="agent" against the CURRENT roster (spec: the black-hole
// fix — an MCP caller passes to_target straight through, with no client-side
// resolution to catch a mistyped or stale label before it reaches the wire,
// so this daemon-side check is the ONLY backstop before a thread gets
// created addressed to nobody). from is the sending agent's alias: its
// CURRENTLY registered project scopes a bare label exactly as the CLI's own
// resolver scopes one by the caller's tmux project. An unregistered sender
// has no project to scope a label against, so it falls back to
// resolve.AliasExact — an exact alias still resolves (spec: "unregistered
// sender → alias-exact matching only"), but no label of any form does.
//
// The daemon builds resolve.Candidate from store.Agent's STORED
// label/label_manual, never a live tmux re-read — internal/daemon is
// tmux-agnostic by rule (CLAUDE.md). The stored copy is kept current by the
// writers: `muster label` pushes its change here via the set_label op in the
// same command that sets the tmux option (see cli.syncLabelToBus), and
// register_agent's upsert re-captures it — so this resolver and the CLI's
// live-tmux resolver see the same manual labels, not eventually-consistent
// ones. (A label written by raw tmux set-option, bypassing muster, still
// only lands at the next register.)
func (d *Daemon) resolveAgentTarget(from, given string) (string, error) {
	agents, err := d.s.ListAgents()
	if err != nil {
		return "", err
	}
	// Expand a short local name to the stored alias. This is the ONLY place a
	// model-supplied target is checked: MCP passes to_target straight through
	// with no client-side resolution, so without this a model that read a
	// short alias off the roster could not address it at all. Local-first,
	// matching the CLI's expandAlias (internal/cli/dispalias.go): try
	// <device>-<given> before the literal given, and expand only when the
	// seeded form actually exists in the roster already in hand — an
	// unexpandable name is left untouched so the caller's error names what
	// was actually sent. "" disables expansion (Lambda mode, which serves
	// many devices and must never guess one).
	if d.deviceName != "" {
		if seeded := device.Seed(d.deviceName, given); seeded != given {
			for _, ag := range agents {
				if ag.Alias == seeded {
					given = seeded
					break
				}
			}
		}
	}
	candidates := make([]resolve.Candidate, len(agents))
	for i, ag := range agents {
		candidates[i] = resolve.Candidate{
			Alias: ag.Alias, Project: ag.Project,
			Label: ag.Label, LabelManual: ag.LabelManual, Departed: ag.Departed,
		}
	}
	sender, found, err := d.s.GetAgent(from)
	if err != nil {
		return "", err
	}
	if !found {
		return resolve.AliasExact(candidates, given)
	}
	return resolve.Target(candidates, given, sender.Project)
}
