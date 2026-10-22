package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/subaud-tools/muster/internal/tmuxenv"
	"github.com/subaud-tools/tools-common/harness"
)

// SendMessageIn is the input to send_message. Only the message content and a
// recipient are truly needed: `from` defaults to this session's own alias,
// `to_kind` is inferred from the recipient, `subject` is derived from the
// body, and `to`/`message`/`text` are accepted as convenience aliases — so a
// naive {to, message} call succeeds (normalizeSendMessage fills the rest).
type SendMessageIn struct {
	From     string `json:"from,omitempty" jsonschema:"the sending agent's alias; OPTIONAL — defaults to this session's own alias when it has exactly one"`
	ToKind   string `json:"to_kind,omitempty" jsonschema:"agent (default), role, or broadcast; OPTIONAL — inferred from the recipient ('role:<name>' → role, 'broadcast' → broadcast, otherwise agent) when omitted"`
	ToTarget string `json:"to_target,omitempty" jsonschema:"the recipient: an alias, a 'project:label' pair, or a bare label of an agent in your own project (resolved in that order) — or a role with to_kind=role; for broadcast: empty reaches every agent on the bus, or a project name reaches only that project's agents (unknown projects are rejected)"`
	To       string `json:"to,omitempty" jsonschema:"convenience alias for to_target: a bare alias (to_kind defaults to agent), 'role:<name>', or 'broadcast'"`
	Subject  string `json:"subject,omitempty" jsonschema:"a short subject line; OPTIONAL — derived from the first line of the body when omitted"`
	Ref      string `json:"ref,omitempty" jsonschema:"optional pointer to the work (repo/branch/endpoint/file)"`
	Body     string `json:"body,omitempty" jsonschema:"the message body (you may also pass it as 'message' or 'text')"`
	Message  string `json:"message,omitempty" jsonschema:"convenience alias for body"`
	Text     string `json:"text,omitempty" jsonschema:"convenience alias for body"`
	Intent   string `json:"intent,omitempty" jsonschema:"fyi | reply-requested | action-requested; mark FYIs so recipients' drains stay cheap — an FYI doesn't demand a reply. Leave empty when the message's urgency is unspecified."`
	Standing bool   `json:"standing,omitempty" jsonschema:"broadcast-only: a plain broadcast reaches only sessions live now; set standing=true to ALSO reach sessions that start later, once, until they read it. Use for durable standing orders (e.g. 'read CONTRACT.md before editing'), NOT for transient holds — those should stay live-only so they evaporate for future sessions."`
	Wake     bool   `json:"wake,omitempty" jsonschema:"BREAK-GLASS, broadcast-only, use sparingly: a broadcast normally lands politely (badge only, picked up on each recipient's next turn) to avoid waking every session at once. Set wake=true to actively INTERRUPT every recipient now. Only for genuinely urgent, must-act-now announcements."`
	Confirm  bool   `json:"confirm,omitempty" jsonschema:"broadcast-only HARD GATE against accidental fan-out: a broadcast is REFUSED on the first call and returns its blast radius (how many agents, which ones) instead of sending. Read that, then re-send the identical broadcast with confirm=true to actually fan out. Direct and role sends ignore this."`
}

// ThreadIDOut is the output of send_message and task_create.
type ThreadIDOut struct {
	ThreadID int64 `json:"thread_id" jsonschema:"the created thread's id"`
}

// ReplyIn is the input to reply.
type ReplyIn struct {
	ThreadID int64  `json:"thread_id" jsonschema:"the thread to reply to"`
	From     string `json:"from,omitempty" jsonschema:"the replying agent's alias; OPTIONAL — defaults to this session's own alias when it has exactly one"`
	Body     string `json:"body" jsonschema:"the reply text"`
	FYI      bool   `json:"fyi,omitempty" jsonschema:"closing note: the entry lands on the thread but wakes nobody — recipients see it on their next natural inbox check. Use for acks, wrap-ups, and any reply that needs nothing back."`
}

// EntryIDOut is the output of reply.
type EntryIDOut struct {
	EntryID int64 `json:"entry_id" jsonschema:"the created entry's id"`
}

// GetInboxIn is the input to get_inbox.
type GetInboxIn struct {
	Alias string `json:"alias" jsonschema:"the agent whose inbox to read"`
}

// GetInboxOut is the output of get_inbox.
type GetInboxOut struct {
	Threads []ThreadView `json:"threads" jsonschema:"threads that concern the agent: addressed to it, its role, broadcast, or originated by it"`
	// MarkedRead mirrors the daemon's marked_read (spec 2026-08-21 §3.2):
	// true when the caller proved ownership of alias and its read watermark
	// moved; false when this was a harmless peek that changed nothing.
	MarkedRead bool `json:"marked_read" jsonschema:"true if this read moved alias's read watermark (you proved you are this session); false if it was a peek that changed nothing"`
	// Detail carries the peek notice when MarkedRead is false — see
	// getInboxHandler.
	Detail string `json:"detail,omitempty" jsonschema:"present only on a peek: explains that alias's unread state was not changed by this read"`
}

// GetThreadIn is the input to get_thread.
type GetThreadIn struct {
	ThreadID int64 `json:"thread_id" jsonschema:"the thread to fetch"`
}

// GetThreadOut is the output of get_thread.
type GetThreadOut struct {
	Thread  ThreadView  `json:"thread" jsonschema:"the thread"`
	Entries []EntryView `json:"entries" jsonschema:"the thread's entries in order"`
}

// firstNonEmpty returns the first non-empty string, or "".
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// deriveSubject makes a scannable subject from the first non-empty line of the
// body when the caller omitted one — a thread list is unreadable with blank
// subjects, and forcing the caller to invent one is the friction this removes.
func deriveSubject(body string) string {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		const maxLen = 72
		if r := []rune(line); len(r) > maxLen {
			return string(r[:maxLen-1]) + "\u2026"
		}
		return line
	}
	return "(no subject)"
}

// resolveFrom returns the alias to attribute a send/reply to: the explicit
// value when given, else this session's OWN alias when it has exactly one.
// Naming yourself to send is the most unusual thing the tool asked for — the
// session already proved its identity, so the daemon can answer "who am I".
// Zero or several aliases are the two cases that still need an explicit choice.
func resolveFrom(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	identity, err := resolveCallerIdentity()
	if err != nil {
		return "", fmt.Errorf("resolve your own alias for 'from': %w", err)
	}
	switch len(identity.LiveAliases) {
	case 1:
		return identity.LiveAliases[0], nil
	case 0:
		return "", fmt.Errorf("'from' is required: this session has no registered muster alias to send as")
	default:
		return "", fmt.Errorf("'from' is required: this session owns multiple aliases (%s) — name which one to send as", strings.Join(identity.LiveAliases, ", "))
	}
}

// normalizeSendMessage fills the forgiving shorthands so a naive {to, message}
// call succeeds: `to`→to_target with to_kind inferred, message/text→body, an
// omitted subject derived from the body, and an omitted `from` defaulted to
// this session's own alias. A fully-specified call passes through untouched.
func normalizeSendMessage(in *SendMessageIn) error {
	in.Body = firstNonEmpty(in.Body, in.Message, in.Text)
	if in.Body == "" {
		return fmt.Errorf("body is required (you can also pass it as 'message' or 'text')")
	}
	in.Message, in.Text = "", ""

	target := firstNonEmpty(in.ToTarget, in.To)
	in.To = ""
	if in.ToKind == "" {
		switch {
		case strings.HasPrefix(target, "role:"):
			in.ToKind = "role"
			target = strings.TrimPrefix(target, "role:")
		case target == "":
			return fmt.Errorf("a recipient is required: set 'to' (or to_target) to an alias, 'role:<name>', or 'broadcast' — or set to_kind=broadcast with an empty target to deliberately reach everyone")
		case target == "broadcast" || target == "everyone":
			in.ToKind = "broadcast"
			target = ""
		default:
			in.ToKind = "agent"
		}
	}
	in.ToTarget = target

	if in.Subject == "" {
		in.Subject = deriveSubject(in.Body)
	}

	from, err := resolveFrom(in.From)
	if err != nil {
		return err
	}
	in.From = from
	return nil
}

func sendMessageHandler(_ context.Context, _ *mcp.CallToolRequest, in SendMessageIn) (*mcp.CallToolResult, ThreadIDOut, error) {
	if err := normalizeSendMessage(&in); err != nil {
		return nil, ThreadIDOut{}, err
	}
	if err := requireRegisteredFrom(in.From); err != nil {
		return nil, ThreadIDOut{}, err
	}
	raw, err := callDaemon("send_message", map[string]any{
		"from": in.From, "to_kind": in.ToKind, "to_target": in.ToTarget,
		"subject": in.Subject, "ref": in.Ref, "body": in.Body, "intent": in.Intent,
		"standing": in.Standing, "wake": in.Wake, "confirm": in.Confirm,
	})
	if err != nil {
		return nil, ThreadIDOut{}, err
	}
	// The unconfirmed broadcast never sent — the daemon returned its blast
	// radius instead. Surface that as an error so the tool call fails loudly
	// and the agent re-sends with confirm=true, having seen who it reaches.
	var gate struct {
		ConfirmRequired bool     `json:"confirm_required"`
		RecipientCount  int      `json:"recipient_count"`
		Recipients      []string `json:"recipients"`
		ToTarget        string   `json:"to_target"`
	}
	if err := json.Unmarshal(raw, &gate); err == nil && gate.ConfirmRequired {
		scope := "every agent on the bus"
		if gate.ToTarget != "" {
			scope = "project " + gate.ToTarget
		}
		who := "(none currently live)"
		if len(gate.Recipients) > 0 {
			who = strings.Join(gate.Recipients, ", ")
		}
		return nil, ThreadIDOut{}, fmt.Errorf(
			"broadcast not sent, confirm required: it would reach %d agent(s) in %s: %s — re-send the identical broadcast with confirm=true to fan out",
			gate.RecipientCount, scope, who)
	}
	var out ThreadIDOut
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, ThreadIDOut{}, err
	}
	return nil, out, nil
}

func replyHandler(_ context.Context, _ *mcp.CallToolRequest, in ReplyIn) (*mcp.CallToolResult, EntryIDOut, error) {
	from, err := resolveFrom(in.From)
	if err != nil {
		return nil, EntryIDOut{}, err
	}
	in.From = from
	if err := requireRegisteredFrom(in.From); err != nil {
		return nil, EntryIDOut{}, err
	}
	raw, err := callDaemon("reply", map[string]any{
		"thread_id": in.ThreadID, "from": in.From, "body": in.Body, "fyi": in.FYI,
	})
	if err != nil {
		return nil, EntryIDOut{}, err
	}
	var out EntryIDOut
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, EntryIDOut{}, err
	}
	return nil, out, nil
}

func getInboxHandler(_ context.Context, _ *mcp.CallToolRequest, in GetInboxIn) (*mcp.CallToolResult, GetInboxOut, error) {
	// The caller's tmux/harness identity is the proof the daemon's
	// callerOwns check needs (spec 2026-08-21 §3.2) to move alias's read
	// watermark — sourced exactly like register_agent's own mint sites
	// (tmuxenv.CaptureEnv for the tuple, harness.FromEnv for the harness
	// UUID). No caller_device_id: the register path never sends one either
	// (it is stamped server-side only for a fixed set of session-scoped ops
	// in remote mode — see daemon.deviceOps), so a local client mirrors that
	// by omission. No caller_pane_id: ownership here is session-granular, not
	// pane-granular (daemon commit 5a79d0e).
	c := tmuxenv.CaptureEnv()
	h := harness.FromEnv()
	raw, err := callDaemon("get_inbox", map[string]any{
		"alias":                     in.Alias,
		"caller_socket_path":        c.SocketPath,
		"caller_session_id":         c.SessionID,
		"caller_session_created":    c.SessionCreated,
		"caller_harness_session_id": h.SessionID,
	})
	if err != nil {
		return nil, GetInboxOut{}, err
	}
	threads, markedRead, err := decodeInboxResponse(raw)
	if err != nil {
		return nil, GetInboxOut{}, err
	}
	result := GetInboxOut{Threads: threads, MarkedRead: markedRead}
	if !markedRead {
		result.Detail = fmt.Sprintf("peek only — '%s' is not this session's; its unread state is unchanged", in.Alias)
	}
	return nil, result, nil
}

// decodeInboxResponse decodes a get_inbox payload, tolerating BOTH shapes
// (Finding 2): a 0.13.0+ daemon returns {threads, marked_read}, but a client
// upgraded ahead of a still-running pre-0.13.0 daemon (or a device forwarding
// to a not-yet-redeployed hosted lambda) still gets the old bare-array shape.
// Decoding that as marked_read=true matches the old daemon's actual
// behavior — every read moved the watermark — so this is not a guess, it's
// what happened. Deliberately no version handshake: this is decode-only.
func decodeInboxResponse(raw []byte) ([]ThreadView, bool, error) {
	var body struct {
		Threads    []ThreadView `json:"threads"`
		MarkedRead bool         `json:"marked_read"`
	}
	if err := json.Unmarshal(raw, &body); err == nil {
		return body.Threads, body.MarkedRead, nil
	}
	var legacy []ThreadView
	if err := json.Unmarshal(raw, &legacy); err != nil {
		return nil, false, err
	}
	return legacy, true, nil
}

func getThreadHandler(_ context.Context, _ *mcp.CallToolRequest, in GetThreadIn) (*mcp.CallToolResult, GetThreadOut, error) {
	raw, err := callDaemon("get_thread", map[string]any{"thread_id": in.ThreadID})
	if err != nil {
		return nil, GetThreadOut{}, err
	}
	var out GetThreadOut
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, GetThreadOut{}, err
	}
	return nil, out, nil
}

// registerMessageTools registers send_message, reply, get_inbox, and
// get_thread on srv.
func registerMessageTools(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{Name: "send_message", Description: "Send a message to another agent, a role, or many agents at once. Minimal call: {\"to\":\"<alias>\",\"body\":\"...\"} — `from` defaults to your own alias, `to_kind` is inferred (bare alias → agent, 'role:<name>' → role, 'broadcast' → broadcast), and `subject` is derived from the body when omitted; `to`/`message`/`text` are convenience aliases for to_target/body. Fuller control: set to_kind (agent/role/broadcast) and to_target explicitly. A broadcast with empty to_target reaches every agent on the bus; set to_target to a project name to reach only that project's agents. A plain broadcast is live-only (reaches sessions live now, invisible to sessions that start later); set standing=true to also reach future sessions once, until read — for durable standing orders, not transient holds. Set intent to fyi/reply-requested/action-requested so the recipient's inbox and drain reflect what you actually need back. A broadcast is HARD-GATED: the first call returns its blast radius (how many agents, which ones) and sends nothing — re-send with confirm=true to actually fan out, so a reflexive broadcast can't storm every session. A reply-requested or action-requested send (every task without intent fyi) to an agent that has ended is refused with the reason; send fyi to leave it waiting for that alias to return."}, sendMessageHandler)
	mcp.AddTool(srv, &mcp.Tool{Name: "reply", Description: "Append a reply to an existing thread (message or task). Minimal call: {\"thread_id\":<id>,\"body\":\"...\"} — `from` defaults to your own alias. Reply only when the sender needs something from you; never reply just to acknowledge an ack or a closure — the last word is free. For a closing note that needs nothing back, set fyi=true so the entry lands without waking anyone."}, replyHandler)
	mcp.AddTool(srv, &mcp.Tool{Name: "get_inbox", Description: "Read the threads that concern an agent — addressed to it (directly, by role, or broadcast) or originated by it, so replies on threads it started show up here — newest first. Rows carry last_from and an unread count — unread > 0 means entries you have not seen; read those threads with get_thread before reporting their state."}, getInboxHandler)
	mcp.AddTool(srv, &mcp.Tool{Name: "get_thread", Description: "Fetch a single thread and all its entries in order."}, getThreadHandler)
}
