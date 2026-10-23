package station

import (
	"encoding/json"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/subaud-tools/muster/internal/render"
	"github.com/subaud-tools/muster/internal/tmuxenv"
)

// tickMsg fires every --interval; Update's tickMsg branch re-issues the
// three fetch Cmds and reschedules the next tick.
type tickMsg time.Time

// eventsMsg carries one list_events page — follow mode on every tick after
// the model has bootstrapped, or the one-time cold-start BACKLOG fetch
// (backlog=true) before it has. gen is the poll generation that issued the
// fetch (Model.pollGen at dispatch time); Update discards any eventsMsg whose
// gen no longer matches the model's current pollGen, so a slow in-flight
// fetch from an older tick can never apply after a newer tick's fetch already
// has (see Update's eventsMsg case and applyEvents).
type eventsMsg struct {
	page    render.EventsPage
	err     error
	gen     int64
	backlog bool
}

// agentsMsg carries one enriched roster snapshot (or a fetch error).
type agentsMsg struct {
	rows []agentEnriched
	err  error
}

// threadsMsg carries one list_threads snapshot (or a fetch error).
type threadsMsg struct {
	threads              []listThreadRow
	activeTasksTruncated bool
	err                  error
}

func tickCmd(interval time.Duration) tea.Cmd {
	return tea.Tick(interval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// eventFetchLimit mirrors `muster tail`'s follow-mode call (limit 0 = the
// daemon's own cap) — station's tick fetch is a follow-mode poll exactly
// like tail's, just issued from a tea.Cmd instead of a for-loop.
const eventFetchLimit = 0

func fetchEventsCmd(caller render.Caller, cursor, gen int64) tea.Cmd {
	return func() tea.Msg {
		page, err := render.FetchEvents(caller, "", "", 0, cursor, eventFetchLimit)
		return eventsMsg{page: page, err: err, gen: gen}
	}
}

// fetchBacklogEventsCmd issues the cold-start (or bootstrap-retry) fetch in
// BACKLOG mode — afterID -1 — exactly like `muster tail`'s initial fetch.
// Follow mode from after_id=0 would let the daemon cap the page at its
// oldest-1000-row window while still reporting the GLOBAL tail as max_id,
// silently skipping every event between row 1000 and the tail on a mature
// journal; backlog mode instead returns the most recent `limit` rows and lets
// the model seed its cursor from their max_id (see Model.pollCmd,
// Model.applyEvents).
func fetchBacklogEventsCmd(caller render.Caller, limit int, gen int64) tea.Cmd {
	return func() tea.Msg {
		page, err := render.FetchEvents(caller, "", "", 0, -1, limit)
		return eventsMsg{page: page, err: err, gen: gen, backlog: true}
	}
}

// agentRow mirrors the list_agents wire JSON — station is a peer client of
// the daemon, like humancli (see internal/cli.agentRow); it decodes its
// own copy rather than importing internal/cli or internal/store.
type agentRow struct {
	Alias          string `json:"alias"`
	Role           string `json:"role"`
	ModelType      string `json:"model_type"`
	DeviceName     string `json:"device_name"`
	DeviceID       string `json:"device_id"`
	Departed       bool   `json:"departed"`
	SocketPath     string `json:"socket_path"`
	PaneID         string `json:"pane_id"`
	SessionID      string `json:"session_id"`
	SessionCreated int64  `json:"session_created"`
	Project        string `json:"project"`
	Label          string `json:"label"`
	LabelManual    bool   `json:"label_manual"`
}

func fetchAgentsCmd(caller render.Caller) tea.Cmd {
	return func() tea.Msg {
		rows, err := fetchAgents(caller)
		return agentsMsg{rows: rows, err: err}
	}
}

// sessionUnreadCount caches one (socket_path, session_id) tuple's
// session_unread result across the agent rows that share it (spec §5:
// "per-tuple unread" — a session with several sibling aliases must not be
// queried once per alias).
type sessionUnreadCount struct{ total, action int }

// sessionTupleKey is that cache's key: the incarnation belongs in it, since
// session_unread now answers per (socket_path, session_id, session_created)
// — two rows sharing a recycled session ID under different creation times
// are different sessions and must not share a cached count (spec §5.1).
type sessionTupleKey struct {
	socketPath, sessionID string
	sessionCreated        int64
}

func effectiveAgentLive(departed, sessionAlive bool) bool {
	return !departed && sessionAlive
}

// fetchAgents lists agents, overlays live tmux state (liveness + current
// label, exactly like `muster agents`), and looks up each distinct live
// session tuple's unread count once.
func fetchAgents(caller render.Caller) ([]agentEnriched, error) {
	raw, err := caller.Call("list_agents", nil)
	if err != nil {
		return nil, err
	}
	var rows []agentRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, err
	}
	cache := map[sessionTupleKey]sessionUnreadCount{}
	out := make([]agentEnriched, 0, len(rows))
	for _, a := range rows {
		e := agentEnriched{
			Alias: a.Alias, Project: a.Project, ModelType: a.ModelType, Role: a.Role,
			DeviceName: a.DeviceName, DeviceID: a.DeviceID, Departed: a.Departed,
			Label: a.Label, LabelManual: a.LabelManual,
			SocketPath: a.SocketPath, PaneID: a.PaneID, SessionID: a.SessionID, SessionCreated: a.SessionCreated,
		}
		e.Live = effectiveAgentLive(a.Departed, tmuxenv.IsSessionAlive(a.SocketPath, a.SessionID, a.SessionCreated))
		if e.Live {
			e.Label, e.LabelManual = tmuxenv.SessionLabel(a.SocketPath, a.SessionID)
		}
		if a.SocketPath != "" && a.SessionID != "" {
			key := sessionTupleKey{a.SocketPath, a.SessionID, a.SessionCreated}
			u, ok := cache[key]
			if !ok {
				if total, action, uerr := sessionUnread(caller, a.SocketPath, a.SessionID, a.SessionCreated); uerr == nil {
					u = sessionUnreadCount{total, action}
					cache[key] = u
				}
			}
			e.Unread, e.Action, e.ActionCount = u.total, u.action > 0, u.action
		}
		out = append(out, e)
	}
	return out, nil
}

func sessionUnread(caller render.Caller, socketPath, sessionID string, sessionCreated int64) (total, action int, err error) {
	raw, err := caller.Call("session_unread", map[string]any{"socket_path": socketPath, "session_id": sessionID, "session_created": sessionCreated})
	if err != nil {
		return 0, 0, err
	}
	var res struct {
		Total  int `json:"total"`
		Action int `json:"action"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, 0, err
	}
	return res.Total, res.Action, nil
}

// threadListLimit is the page size station's tick fetch passes to
// list_threads — generous enough for one screen's worth of rows without
// pulling the whole table every second.
const threadListLimit = 200

func fetchThreadsCmd(caller render.Caller) tea.Cmd {
	return func() tea.Msg {
		threads, truncated, err := fetchThreads(caller)
		return threadsMsg{threads: threads, activeTasksTruncated: truncated, err: err}
	}
}

func fetchThreads(caller render.Caller) ([]listThreadRow, bool, error) {
	raw, err := caller.Call("list_threads", map[string]any{
		"limit": threadListLimit, "include_nonterminal_tasks": true,
	})
	if err != nil {
		return nil, false, err
	}
	var res struct {
		Threads               []listThreadRow `json:"threads"`
		ActiveTasksTruncated  bool            `json:"active_tasks_truncated"`
		RetainedActiveTaskIDs []int64         `json:"retained_active_task_ids"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, false, err
	}
	retained := make(map[int64]bool, len(res.RetainedActiveTaskIDs))
	for _, id := range res.RetainedActiveTaskIDs {
		retained[id] = true
	}
	for i := range res.Threads {
		res.Threads[i].RetainedActiveTask = retained[res.Threads[i].ID]
	}
	return res.Threads, res.ActiveTasksTruncated, nil
}

// threadViewPageSize bounds the thread view's initial get_thread fetch (spec
// §5: "station passes limit 200"). A "load older" fetch (see
// fetchThreadPageCmd's older=true path) instead asks for exactly the entries
// missing below the currently loaded window.
const threadViewPageSize = 200

// threadEntryRow mirrors store.Entry's wire JSON — station decodes its own
// copy (get_thread's "entries" field) rather than importing internal/store,
// the same peer-client pattern as agentRow/listThreadRow.
type threadEntryRow struct {
	ID           int64  `json:"id"`
	ThreadID     int64  `json:"thread_id"`
	FromAgent    string `json:"from_agent"`
	Body         string `json:"body"`
	StatusChange string `json:"status_change"`
	CreatedAt    int64  `json:"created_at"`
}

// threadPageMsg carries one get_thread{offset,limit} page. older marks a
// "load older" fetch (Update prepends its entries instead of replacing the
// loaded window); threadID lets Update discard a page that resolves after
// the thread view moved on to a different thread (or closed).
//
// gen mirrors pollGen's role but for one thread-view "opening": stamped from
// Model.viewGen at dispatch time (bumped only when a thread is (re)opened, in
// openSelectedThread), so a page left over from a PREVIOUS opening of the
// very same thread ID — which the threadID check alone can't catch, since
// it's unchanged across a close/reopen of the same thread — is discarded
// rather than applied once a fresher opening has already landed (spec §5
// carried-over fix: threadPageMsg staleness).
//
// total is the live entry count get_thread now reports (spec §5
// carried-over fix: the newest-entries gap) — corrected marks a page that IS
// itself the result of applyThreadPage's one-shot self-correction, so that
// correction can never chain into a second one.
type threadPageMsg struct {
	threadID  int64
	offset    int64
	older     bool
	corrected bool
	gen       int64
	total     int64
	entries   []threadEntryRow
	err       error
}

// fetchThreadPageCmd issues one get_thread call for threadID's
// [offset, offset+limit) window (entries ordered oldest-first; see
// paginateEntries in internal/daemon). older tags the response so Update
// knows to prepend rather than replace; corrected and gen are carried
// straight through to the resulting threadPageMsg (see its doc).
func fetchThreadPageCmd(caller render.Caller, threadID, offset, limit int64, older, corrected bool, gen int64) tea.Cmd {
	return func() tea.Msg {
		raw, err := caller.Call("get_thread", map[string]any{
			"thread_id": threadID, "offset": offset, "limit": limit,
		})
		if err != nil {
			return threadPageMsg{threadID: threadID, offset: offset, older: older, corrected: corrected, gen: gen, err: err}
		}
		var res struct {
			Entries []threadEntryRow `json:"entries"`
			Total   int64            `json:"total"`
		}
		if err := json.Unmarshal(raw, &res); err != nil {
			return threadPageMsg{threadID: threadID, offset: offset, older: older, corrected: corrected, gen: gen, err: err}
		}
		return threadPageMsg{
			threadID: threadID, offset: offset, older: older, corrected: corrected, gen: gen,
			total: res.Total, entries: res.Entries,
		}
	}
}

// inboxAckMsg carries the result of the ONE explicit read station ever
// performs: get_inbox for its own alias, issued exactly once when the
// operator OPENS a thread addressed to station (spec §5's open-to-
// acknowledge exception) — never on focus, selection, or poll.
type inboxAckMsg struct{ err error }

func fetchInboxAckCmd(caller render.Caller, alias, socketPath, sessionID string, sessionCreated int64) tea.Cmd {
	return func() tea.Msg {
		_, err := caller.Call("get_inbox", map[string]any{
			"alias": alias,
			// Proves ownership (spec 2026-08-21 §3.2, task 7): without this,
			// get_inbox is a peek and never advances station's own read
			// watermark.
			"caller_socket_path": socketPath, "caller_session_id": sessionID, "caller_session_created": sessionCreated,
		})
		return inboxAckMsg{err: err}
	}
}

// lastActiveMsg carries one alias's newest ACTOR-event timestamp (spec
// iteration-5 Tier 0a) — gen is the poll generation that issued the fetch
// (mirrors eventsMsg's gen/pollGen discipline exactly): Update discards any
// lastActiveMsg whose gen no longer matches the model's current pollGen (see
// applyLastActive), so a slow in-flight fetch from an older tick never
// clobbers a fresher one. ts is 0 when no actor row turned up within the
// fetch's small window — NOT the same claim as "never active" (see
// applyLastActive's doc).
type lastActiveMsg struct {
	alias string
	ts    int64
	gen   int64
	err   error
}

// lastActiveFetchLimit bounds each per-agent list_events lookup — small,
// since only the newest row where the agent filter's alias is the ACTOR
// (Event.Agent == alias, not merely a thread/target CONCERNING alias — the
// broader match list_events' agent filter itself applies) is needed, and
// backlog mode returns newest-first.
const lastActiveFetchLimit = 20

// fetchLastActiveCmd issues one list_events(agent=alias, backlog) lookup and
// picks the first (newest) row where alias is the actual actor — "read",
// "reply", "send", etc. — discarding rows the agent filter matched only
// because they CONCERN alias (e.g. a message addressed to alias, sent by
// someone else).
func fetchLastActiveCmd(caller render.Caller, alias string, gen int64) tea.Cmd {
	return func() tea.Msg {
		page, err := render.FetchEvents(caller, alias, "", 0, -1, lastActiveFetchLimit)
		if err != nil {
			return lastActiveMsg{alias: alias, gen: gen, err: err}
		}
		for _, e := range page.Events {
			// A "peek" event's Agent is the PEEKED alias, not the caller who
			// did the peeking (Finding 5) — an unowned get_inbox read moves
			// no watermark and is not that alias's own activity, so treating
			// it as one would show an idle agent as just-active on every
			// sweep of its inbox by someone else (an operator, a station
			// poll, another agent checking in).
			if e.Kind == "peek" {
				continue
			}
			if e.Agent == alias {
				return lastActiveMsg{alias: alias, ts: e.TS, gen: gen}
			}
		}
		return lastActiveMsg{alias: alias, gen: gen}
	}
}
