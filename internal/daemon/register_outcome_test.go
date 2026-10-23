package daemon

import (
	"testing"

	"github.com/subaud-tools/muster/internal/proto"
)

// TestRegisterAgentOutcomeClassification covers the three register outcomes
// the durable-alias spec defines: a first-sight alias is "new", an upsert
// over a live row is "refreshed", and an upsert over a tombstone is
// "revived" — with the response carrying the alias's unread count so a
// resuming session learns its backlog in the same call.
func TestRegisterAgentOutcomeClassification(t *testing.T) {
	sock := startWithNotifier(t, &fakeNotifier{})

	resp := call(t, sock, "register_agent", map[string]any{
		"alias": "backend", "socket_path": "/s", "session_id": "$1", "session_created": 100,
	})
	if !resp.OK {
		t.Fatalf("register: %+v", resp)
	}
	data, _ := resp.Data.(map[string]any)
	if data["outcome"] != "new" {
		t.Fatalf("first register outcome = %v, want new", data["outcome"])
	}

	resp = call(t, sock, "register_agent", map[string]any{
		"alias": "backend", "socket_path": "/s", "session_id": "$1", "session_created": 100,
	})
	data, _ = resp.Data.(map[string]any)
	if data["outcome"] != "refreshed" {
		t.Fatalf("re-register outcome = %v, want refreshed", data["outcome"])
	}

	// Mail lands while the agent is departed; revival must report it.
	call(t, sock, "register_agent", map[string]any{
		"alias": "sender", "socket_path": "/s", "session_id": "$2", "session_created": 100,
	})
	call(t, sock, "send_message", map[string]any{
		"from": "sender", "to_kind": "agent", "to_target": "backend",
		"subject": "while you were away", "body": "ping",
	})
	call(t, sock, "deregister_agent", map[string]any{"alias": "backend"})

	resp = call(t, sock, "register_agent", map[string]any{
		"alias": "backend", "socket_path": "/s3", "session_id": "$9", "session_created": 100,
	})
	data, _ = resp.Data.(map[string]any)
	if data["outcome"] != "revived" {
		t.Fatalf("revival outcome = %v, want revived", data["outcome"])
	}
	if n, _ := data["unread"].(float64); n < 1 {
		t.Fatalf("revival unread = %v, want >= 1", data["unread"])
	}

	// Revival is journaled so tail/station show a returning agent.
	ev := call(t, sock, "list_events", map[string]any{"agent": "backend"})
	if !containsEventDetail(t, ev, "register", "revived") {
		t.Fatalf("no register/revived journal event after revival: %+v", ev.Data)
	}
}

// containsEventDetail decodes list_events' response — {"events": [...],
// "max_id": ...} over the wire, each event row a map[string]any — and
// reports whether any row matches the given kind/detail pair.
func containsEventDetail(t *testing.T, resp proto.Response, kind, detail string) bool {
	t.Helper()
	data, _ := resp.Data.(map[string]any)
	rows, _ := data["events"].([]any)
	for _, r := range rows {
		m, _ := r.(map[string]any)
		if m["kind"] == kind && m["detail"] == detail {
			return true
		}
	}
	return false
}
