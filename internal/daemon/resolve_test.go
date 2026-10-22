package daemon

import (
	"strings"
	"testing"

	"github.com/subaud-tools/muster/internal/proto"
)

// TestSendMessageResolvesManualLabelInSenderProject is the direct fix for
// the black-hole incident (spec queue item 1): unlike the CLI, an MCP caller
// never pre-resolves to_target client-side — it passes a label straight
// through. The daemon itself must resolve it against the sender's own
// registered project, and the ALIAS it resolves to (not the raw label) is
// what lands in the stored thread, so a later Inbox/get_thread lookup by
// alias actually finds it.
func TestSendMessageResolvesManualLabelInSenderProject(t *testing.T) {
	sock := startTestDaemon(t)
	call(t, sock, "register_agent", map[string]any{
		"alias": "sender-1", "role": "producer", "model_type": "claude", "project": "lake",
	})
	call(t, sock, "register_agent", map[string]any{
		"alias": "datalake-1", "role": "worker", "model_type": "claude", "project": "lake",
		"label": "datalake", "label_manual": true,
	})

	resp := call(t, sock, "send_message", map[string]any{
		"from": "sender-1", "to_kind": "agent", "to_target": "datalake", "subject": "s", "body": "b",
	})
	if !resp.OK {
		t.Fatalf("expected label 'datalake' to resolve within sender's project, got %+v", resp)
	}

	list := call(t, sock, "list_threads", map[string]any{"limit": 10})
	var res struct {
		Threads []struct {
			ToTarget string `json:"to_target"`
		} `json:"threads"`
	}
	decode(t, list, &res)
	if len(res.Threads) != 1 || res.Threads[0].ToTarget != "datalake-1" {
		t.Fatalf("expected stored to_target = resolved alias 'datalake-1', got %+v", res.Threads)
	}
}

// TestSendMessageLabelInAnotherProjectFailsWithHint: a label that exists only
// in a DIFFERENT project than the sender's must fail loudly with a
// proj:label hint — never guess across a project boundary.
func TestSendMessageLabelInAnotherProjectFailsWithHint(t *testing.T) {
	sock := startTestDaemon(t)
	call(t, sock, "register_agent", map[string]any{
		"alias": "sender-1", "role": "producer", "model_type": "claude", "project": "muster",
	})
	call(t, sock, "register_agent", map[string]any{
		"alias": "datalake-1", "role": "worker", "model_type": "claude", "project": "lake",
		"label": "datalake", "label_manual": true,
	})

	resp := call(t, sock, "send_message", map[string]any{
		"from": "sender-1", "to_kind": "agent", "to_target": "datalake", "subject": "s", "body": "b",
	})
	if resp.OK {
		t.Fatalf("expected cross-project label to fail loudly, got %+v", resp)
	}
	if !strings.Contains(resp.Error, "lake:datalake") {
		t.Fatalf("expected error to hint the qualified target 'lake:datalake', got %q", resp.Error)
	}
}

// TestSendMessageUnknownTargetFailsLoudly is the direct regression test for
// the black-hole incident: an MCP agent addressing a target that resolves to
// NOTHING must fail the op outright — never silently create an
// undeliverable thread.
func TestSendMessageUnknownTargetFailsLoudly(t *testing.T) {
	sock := startTestDaemon(t)
	call(t, sock, "register_agent", map[string]any{"alias": "sender-1", "role": "producer", "model_type": "claude"})

	resp := call(t, sock, "send_message", map[string]any{
		"from": "sender-1", "to_kind": "agent", "to_target": "datalake", "subject": "s", "body": "b",
	})
	if resp.OK {
		t.Fatal("expected unknown target to fail the op")
	}

	list := call(t, sock, "list_threads", map[string]any{"limit": 10})
	var res struct {
		Threads []any `json:"threads"`
	}
	decode(t, list, &res)
	if len(res.Threads) != 0 {
		t.Fatalf("a failed resolution must not create a thread, got %+v", res.Threads)
	}
}

// TestSendMessageDepartedAliasStillAccepted: a departed (tombstoned) agent's
// ALIAS remains addressable for fyi mail (a message with no intent is fyi) —
// it may be waiting for them to return. A request is refused; see
// TestSendMessageRequestToDepartedRefused.
func TestSendMessageDepartedAliasStillAccepted(t *testing.T) {
	sock := startTestDaemon(t)
	call(t, sock, "register_agent", map[string]any{"alias": "sender-1", "role": "producer", "model_type": "claude"})
	call(t, sock, "register_agent", map[string]any{"alias": "gone-1", "role": "worker", "model_type": "claude"})
	call(t, sock, "deregister_agent", map[string]any{"alias": "gone-1"})

	resp := call(t, sock, "send_message", map[string]any{
		"from": "sender-1", "to_kind": "agent", "to_target": "gone-1", "subject": "s", "body": "b",
	})
	if !resp.OK {
		t.Fatalf("expected a departed agent's ALIAS to still resolve, got %+v", resp)
	}
}

// registerSenderAndGone registers sender-1 and gone-1, then departs gone-1.
func registerSenderAndGone(t *testing.T, sock string) {
	t.Helper()
	call(t, sock, "register_agent", map[string]any{"alias": "sender-1", "role": "producer", "model_type": "claude"})
	call(t, sock, "register_agent", map[string]any{"alias": "gone-1", "role": "worker", "model_type": "claude"})
	call(t, sock, "deregister_agent", map[string]any{"alias": "gone-1"})
}

// requireEndedRefusal fails unless resp is the ended-session refusal.
func requireEndedRefusal(t *testing.T, resp proto.Response) {
	t.Helper()
	if resp.OK {
		t.Fatalf("expected a request to a departed agent to be refused, got %+v", resp)
	}
	for _, want := range []string{"gone-1", "ended", "fyi"} {
		if !strings.Contains(resp.Error, want) {
			t.Fatalf("error %q does not contain %q", resp.Error, want)
		}
	}
}

// TestSendMessageRequestToDepartedRefused: a reply- or action-requested
// message to a departed alias fails with the reason and creates no thread.
func TestSendMessageRequestToDepartedRefused(t *testing.T) {
	for _, intent := range []string{"reply-requested", "action-requested"} {
		t.Run(intent, func(t *testing.T) {
			sock := startTestDaemon(t)
			registerSenderAndGone(t, sock)
			resp := call(t, sock, "send_message", map[string]any{
				"from": "sender-1", "to_kind": "agent", "to_target": "gone-1",
				"subject": "s", "body": "b", "intent": intent,
			})
			requireEndedRefusal(t, resp)
			if n := threadCount(t, call(t, sock, "list_threads", map[string]any{"limit": 10})); n != 0 {
				t.Fatalf("a refused request must create no thread, got %d", n)
			}
		})
	}
}

// TestTaskCreateToDepartedRefused: a task with no intent is a request, so it
// is refused to a departed alias; an fyi task still waits.
func TestTaskCreateToDepartedRefused(t *testing.T) {
	sock := startTestDaemon(t)
	registerSenderAndGone(t, sock)
	resp := call(t, sock, "task_create", map[string]any{
		"from": "sender-1", "to_kind": "agent", "to_target": "gone-1", "subject": "s", "body": "b",
	})
	requireEndedRefusal(t, resp)
	if n := threadCount(t, call(t, sock, "list_threads", map[string]any{"limit": 10})); n != 0 {
		t.Fatalf("a refused task must create no thread, got %d", n)
	}

	resp = call(t, sock, "task_create", map[string]any{
		"from": "sender-1", "to_kind": "agent", "to_target": "gone-1", "subject": "s", "body": "b", "intent": "fyi",
	})
	if !resp.OK {
		t.Fatalf("expected an fyi task to a departed agent to be accepted, got %+v", resp)
	}
}

// TestSendMessageFyiToDepartedStillAccepted: an explicit fyi to a departed
// alias still waits for them to return.
func TestSendMessageFyiToDepartedStillAccepted(t *testing.T) {
	sock := startTestDaemon(t)
	registerSenderAndGone(t, sock)
	resp := call(t, sock, "send_message", map[string]any{
		"from": "sender-1", "to_kind": "agent", "to_target": "gone-1", "subject": "s", "body": "b", "intent": "fyi",
	})
	if !resp.OK {
		t.Fatalf("expected an fyi to a departed agent to be accepted, got %+v", resp)
	}
}

// TestSendMessageRequestToLiveAgentAccepted guards against over-refusal: a
// request to a registered, non-departed alias goes through.
func TestSendMessageRequestToLiveAgentAccepted(t *testing.T) {
	sock := startTestDaemon(t)
	call(t, sock, "register_agent", map[string]any{"alias": "sender-1", "role": "producer", "model_type": "claude"})
	call(t, sock, "register_agent", map[string]any{"alias": "live-1", "role": "worker", "model_type": "claude"})
	resp := call(t, sock, "send_message", map[string]any{
		"from": "sender-1", "to_kind": "agent", "to_target": "live-1", "subject": "s", "body": "b", "intent": "action-requested",
	})
	if !resp.OK {
		t.Fatalf("expected a request to a live agent to be accepted, got %+v", resp)
	}
}

// TestSendMessageDepartedLabelNotAddressable: once an agent has departed,
// its LABEL must stop being addressable — a NEW message naming the label a
// departed agent used to answer to must fail loudly instead of silently
// landing on a thread nobody will ever read again.
func TestSendMessageDepartedLabelNotAddressable(t *testing.T) {
	sock := startTestDaemon(t)
	call(t, sock, "register_agent", map[string]any{
		"alias": "sender-1", "role": "producer", "model_type": "claude", "project": "lake",
	})
	call(t, sock, "register_agent", map[string]any{
		"alias": "gone-1", "role": "worker", "model_type": "claude", "project": "lake",
		"label": "datalake", "label_manual": true,
	})
	call(t, sock, "deregister_agent", map[string]any{"alias": "gone-1"})

	resp := call(t, sock, "send_message", map[string]any{
		"from": "sender-1", "to_kind": "agent", "to_target": "datalake", "subject": "s", "body": "b",
	})
	if resp.OK {
		t.Fatal("expected a departed agent's label to no longer be addressable")
	}
}

// TestSendMessageAutoLabelNotAddressable: an auto (non-manual) topic label —
// derived from conversation content, not pinned by the operator — must
// never be addressable, at the daemon exactly as at the CLI (only a
// manually-pinned label is a stable address).
func TestSendMessageAutoLabelNotAddressable(t *testing.T) {
	sock := startTestDaemon(t)
	call(t, sock, "register_agent", map[string]any{
		"alias": "sender-1", "role": "producer", "model_type": "claude", "project": "lake",
	})
	call(t, sock, "register_agent", map[string]any{
		"alias": "auto-1", "role": "worker", "model_type": "claude", "project": "lake",
		"label": "some topic", "label_manual": false,
	})

	resp := call(t, sock, "send_message", map[string]any{
		"from": "sender-1", "to_kind": "agent", "to_target": "some topic", "subject": "s", "body": "b",
	})
	if resp.OK {
		t.Fatal("expected an auto (non-manual) label to be unaddressable")
	}
}

// TestSendMessageAmbiguousLabelListsCandidates: two manually-pinned labels
// colliding in the sender's own project must fail with an ambiguity error
// naming both candidate aliases — never silently pick one.
func TestSendMessageAmbiguousLabelListsCandidates(t *testing.T) {
	sock := startTestDaemon(t)
	call(t, sock, "register_agent", map[string]any{
		"alias": "sender-1", "role": "producer", "model_type": "claude", "project": "lake",
	})
	call(t, sock, "register_agent", map[string]any{
		"alias": "worker-a", "role": "worker", "model_type": "claude", "project": "lake",
		"label": "datalake", "label_manual": true,
	})
	call(t, sock, "register_agent", map[string]any{
		"alias": "worker-b", "role": "worker", "model_type": "claude", "project": "lake",
		"label": "datalake", "label_manual": true,
	})

	resp := call(t, sock, "send_message", map[string]any{
		"from": "sender-1", "to_kind": "agent", "to_target": "datalake", "subject": "s", "body": "b",
	})
	if resp.OK {
		t.Fatal("expected ambiguous label to fail")
	}
	if !strings.Contains(resp.Error, "worker-a") || !strings.Contains(resp.Error, "worker-b") {
		t.Fatalf("expected ambiguity error to list both candidates, got %q", resp.Error)
	}
}

// TestTaskCreateUnregisteredSenderAliasExactOnly: an unregistered sender has
// no registered project to scope a bare label against — task_create still
// resolves an EXACT alias for it, but bare-label resolution is skipped
// entirely rather than guessing "" as if it were a real project.
func TestTaskCreateUnregisteredSenderAliasExactOnly(t *testing.T) {
	sock := startTestDaemon(t)
	call(t, sock, "register_agent", map[string]any{
		"alias": "worker-1", "role": "worker", "model_type": "claude", "project": "",
		"label": "datalake", "label_manual": true,
	})

	okResp := call(t, sock, "task_create", map[string]any{
		"from": "nobody", "to_kind": "agent", "to_target": "worker-1", "subject": "s", "body": "b",
	})
	if !okResp.OK {
		t.Fatalf("expected exact alias to resolve for an unregistered sender, got %+v", okResp)
	}

	failResp := call(t, sock, "task_create", map[string]any{
		"from": "nobody", "to_kind": "agent", "to_target": "datalake", "subject": "s", "body": "b",
	})
	if failResp.OK {
		t.Fatal("expected bare-label resolution to fail for an unregistered sender")
	}
}

// TestSendMessageRoleAndBroadcastUnaffected: role and broadcast targets
// never go through agent-alias resolution — a role name or an empty
// broadcast target is not, and never was, a to_target the daemon resolves.
func TestSendMessageRoleAndBroadcastUnaffected(t *testing.T) {
	sock := startTestDaemon(t)
	call(t, sock, "register_agent", map[string]any{"alias": "sender-1", "role": "producer", "model_type": "claude"})
	call(t, sock, "register_agent", map[string]any{"alias": "rev", "role": "reviewer", "model_type": "claude"})

	if resp := call(t, sock, "send_message", map[string]any{
		"from": "sender-1", "to_kind": "role", "to_target": "reviewer", "subject": "s", "body": "b",
	}); !resp.OK {
		t.Fatalf("role-addressed send must be unaffected by agent resolution, got %+v", resp)
	}
	if resp := call(t, sock, "send_message", map[string]any{
		"from": "sender-1", "to_kind": "broadcast", "subject": "s", "body": "b",
	}); !resp.OK {
		t.Fatalf("broadcast send must be unaffected by agent resolution, got %+v", resp)
	}
}

// registerTestAgent registers alias against d with no tmux identity, via
// Dispatch — the same seam lambda mode uses to reach the daemon without a
// unix socket. t.Fatal on failure so a broken fixture never masquerades as a
// resolution bug in the test that calls it.
func registerTestAgent(t *testing.T, d *Daemon, alias string) {
	t.Helper()
	resp := d.Dispatch(proto.Request{Op: "register_agent", Args: map[string]any{"alias": alias}})
	if !resp.OK {
		t.Fatalf("register_agent %q: %+v", alias, resp)
	}
}

// TestResolveAgentTargetExpandsAShortLocalAlias covers the path a model takes:
// MCP passes to_target straight through with no client-side resolution, so if
// the daemon does not expand, a model that read a short alias off the roster
// cannot address it at all.
func TestResolveAgentTargetExpandsAShortLocalAlias(t *testing.T) {
	d := New(newDaemonTestStore(t), nil, "personal")
	registerTestAgent(t, d, "personal-dotfiles/main")

	got, err := d.resolveAgentTarget("", "dotfiles/main")
	if err != nil {
		t.Fatalf("resolveAgentTarget: %v", err)
	}
	if want := "personal-dotfiles/main"; got != want {
		t.Fatalf("resolveAgentTarget = %q, want %q", got, want)
	}
}

// TestResolveAgentTargetKeepsForeignAliasesExact: the full form always
// resolves to itself, which is what makes cross-machine references written
// into message bodies correct wherever they are read.
func TestResolveAgentTargetKeepsForeignAliasesExact(t *testing.T) {
	d := New(newDaemonTestStore(t), nil, "personal")
	registerTestAgent(t, d, "work-dotfiles/main")

	got, err := d.resolveAgentTarget("", "work-dotfiles/main")
	if err != nil {
		t.Fatalf("resolveAgentTarget: %v", err)
	}
	if want := "work-dotfiles/main"; got != want {
		t.Fatalf("resolveAgentTarget = %q, want %q", got, want)
	}
}

// TestResolveAgentTargetPrefersLocalSeededOverForeignBareAlias is the
// discriminator between local-first and exact-first precedence: the roster
// holds BOTH a foreign row registered under the exact literal
// "dotfiles/main" AND this device's own seeded row for the same short name.
// Local-first means the given short name must resolve to THIS device's
// agent, not the foreign one that happens to already match the literal
// input — an isolated-call test (only one of the two rows present) cannot
// catch a resolver that got exact-first backwards, since either precedence
// produces the same answer when there is nothing to prefer between.
func TestResolveAgentTargetPrefersLocalSeededOverForeignBareAlias(t *testing.T) {
	d := New(newDaemonTestStore(t), nil, "personal")
	registerTestAgent(t, d, "dotfiles/main")          // foreign bare alias, exact match on the literal input
	registerTestAgent(t, d, "personal-dotfiles/main") // this device's own seeded alias

	got, err := d.resolveAgentTarget("", "dotfiles/main")
	if err != nil {
		t.Fatalf("resolveAgentTarget: %v", err)
	}
	if want := "personal-dotfiles/main"; got != want {
		t.Fatalf("resolveAgentTarget = %q, want %q (local-first must win over the foreign exact match)", got, want)
	}
}
