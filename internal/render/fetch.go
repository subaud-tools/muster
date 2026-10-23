// Package render holds the shared bus-journal rendering machinery: decoding
// list_events pages, resolving the alias→label map, and rendering rows into
// the WHO/WHAT vocabulary the humancli `events`/`tail` commands and the
// station TUI's feed pane both use. It has no daemon-transport code of its
// own — callers hand in a small Caller so render stays a peer of humancli and
// station rather than a third copy of daemon-client plumbing — and it uses
// internal/display for the one canonical sanitizer.
package render

import (
	"encoding/json"
	"strconv"

	"github.com/subaud-tools/muster/internal/tmuxenv"
)

// Caller sends one daemon op and returns its Data as JSON, or an error if the
// transport failed or the daemon reported !OK. humancli and station each wrap
// their own daemon-client plumbing (both ultimately through
// internal/client.Call) behind this interface, so render never dials a
// socket itself.
type Caller interface {
	Call(op string, args map[string]any) (json.RawMessage, error)
}

// EventRow mirrors store.Event's wire JSON.
type EventRow struct {
	ID       int64  `json:"id"`
	TS       int64  `json:"ts"`
	Kind     string `json:"kind"`
	Agent    string `json:"agent"`
	Target   string `json:"target"`
	ThreadID int64  `json:"thread_id"`
	Count    int    `json:"count"`
	Detail   string `json:"detail"`
	Subject  string `json:"subject"`
	// Intent is the event's thread's EFFECTIVE intent (store's effectiveIntent),
	// joined at query time exactly like Subject: "" (unspecified) | "fyi" |
	// "reply-requested" | "action-requested". what() renders it as a tag on
	// send/task rows.
	Intent string `json:"intent"`
}

// EventsPage is the decoded {events, max_id} envelope list_events returns.
type EventsPage struct {
	Events []EventRow `json:"events"`
	MaxID  int64      `json:"max_id"`
}

// FetchEvents calls list_events with the given filters. afterID < 0 selects
// backlog mode (send backlog:true + limit, omit after_id); afterID >= 0
// selects follow mode (send after_id as a decimal string, omit backlog) —
// never both in the same call.
func FetchEvents(c Caller, agent, kind string, threadID, afterID int64, limit int) (EventsPage, error) {
	args := map[string]any{"agent": agent, "kind": kind, "thread_id": threadID, "limit": limit}
	if afterID >= 0 {
		args["after_id"] = strconv.FormatInt(afterID, 10)
	} else {
		args["backlog"] = true
	}
	raw, err := c.Call("list_events", args)
	if err != nil {
		return EventsPage{}, err
	}
	var page EventsPage
	if err := json.Unmarshal(raw, &page); err != nil {
		return EventsPage{}, err
	}
	return page, nil
}

// labelRow decodes just the list_agents fields LoadLabels needs.
type labelRow struct {
	Alias          string `json:"alias"`
	Label          string `json:"label"`
	LabelManual    bool   `json:"label_manual"`
	SocketPath     string `json:"socket_path"`
	SessionID      string `json:"session_id"`
	SessionCreated int64  `json:"session_created"`
}

// LoadLabels fetches the current alias→label map, best-effort: on any error
// the map is nil and aliases render as-is. Labels are LIVE (a live session's
// label is re-read from tmux exactly like `muster agents`), so callers read
// in today's terms; a dead session's stored label snapshot stands in.
func LoadLabels(c Caller) map[string]string {
	raw, err := c.Call("list_agents", nil)
	if err != nil {
		return nil
	}
	var rows []labelRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil
	}
	m := make(map[string]string, len(rows))
	for _, a := range rows {
		label := a.Label
		if tmuxenv.IsSessionAlive(a.SocketPath, a.SessionID, a.SessionCreated) {
			label, _ = tmuxenv.SessionLabel(a.SocketPath, a.SessionID)
		}
		m[a.Alias] = label
	}
	return m
}
