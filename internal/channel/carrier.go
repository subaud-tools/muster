package channel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/subaud-tools/muster/internal/client"
	"github.com/subaud-tools/muster/internal/proto"
)

// DefaultInterval is the poll cadence when MUSTER_CHANNEL_INTERVAL is unset;
// MinInterval is the floor a knob cannot go under (a tighter loop would just
// hammer the daemon socket for no visible gain).
const (
	DefaultInterval = time.Second
	MinInterval     = 250 * time.Millisecond
)

// mailKinds are the journal kinds that mean "something arrived for you".
// notify/read/nudge/claim/transition/register/become are wake-layer or
// lifecycle noise and never wake a session.
var mailKinds = map[string]bool{"send": true, "task": true, "reply": true}

// Client sends one op to the daemon and returns its Data as JSON, or an error
// if the transport failed or the daemon reported !OK. Injectable so tests run
// against an in-memory fake.
type Client func(op string, args map[string]any) (json.RawMessage, error)

// DaemonClient is the production Client over the unix socket (lazily
// starting the daemon exactly as every other peer client does).
func DaemonClient(socketPath string) Client {
	return func(op string, args map[string]any) (json.RawMessage, error) {
		resp, err := client.Call(socketPath, proto.Request{Op: op, Args: args})
		if err != nil {
			return nil, err
		}
		if !resp.OK {
			return nil, fmt.Errorf("%s: %s", op, resp.Error)
		}
		b, err := json.Marshal(resp.Data)
		if err != nil {
			return nil, fmt.Errorf("marshal %s result: %w", op, err)
		}
		return b, nil
	}
}

// Identity is the session tuple the carrier pushes for — the same tuple
// register_agent stores, captured through tmuxenv by the caller.
type Identity struct {
	SocketPath     string
	SessionID      string
	PaneID         string
	SessionCreated int64
}

func (id Identity) paneless() bool {
	return id.SocketPath == "" || id.SessionID == "" || id.PaneID == ""
}

// Carrier tails the journal for one session's aliases and pushes envelopes.
// Zero-value seams: Interval → DefaultInterval, Sleep → time.Sleep, Errw →
// os.Stderr.
type Carrier struct {
	Call     Client
	Notify   func(content string, meta map[string]string) error
	Ident    Identity
	Interval time.Duration
	Sleep    func(time.Duration)
	Errw     io.Writer

	mu       sync.Mutex
	aliases  []string
	cursor   int64
	lastPush time.Time
	lastErr  string
}

func (c *Carrier) errw() io.Writer {
	if c.Errw == nil {
		return os.Stderr
	}
	return c.Errw
}

// resolve asks the daemon which live aliases this session holds. An empty
// list is not an error — the SessionStart hook may not have registered yet.
func (c *Carrier) resolve() ([]string, error) {
	raw, err := c.Call("session_aliases", map[string]any{
		"socket_path": c.Ident.SocketPath, "session_id": c.Ident.SessionID, "session_created": c.Ident.SessionCreated,
	})
	if err != nil {
		return nil, err
	}
	var out struct {
		Aliases []string `json:"aliases"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode session_aliases: %w", err)
	}
	return out.Aliases, nil
}

func (c *Carrier) maxEventID() (int64, error) {
	raw, err := c.Call("list_events", map[string]any{"backlog": true, "limit": 1})
	if err != nil {
		return 0, err
	}
	var out struct {
		MaxID int64 `json:"max_id"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return 0, fmt.Errorf("decode list_events: %w", err)
	}
	return out.MaxID, nil
}

// Start places the cursor at the journal's current head — nothing before the
// channel existed is replayed — and emits one summary if mail is already
// waiting. A paneless session idles: no error, nothing pushed, Status says why.
func (c *Carrier) Start() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Ident.paneless() {
		c.lastErr = "no tmux pane (channel started outside tmux); idle"
		_, _ = fmt.Fprintln(c.errw(), "[muster channel]", c.lastErr)
		return nil
	}
	head, err := c.maxEventID()
	if err != nil {
		c.lastErr = err.Error()
		return err
	}
	c.cursor = head
	c.lastErr = ""
	raw, err := c.Call("session_unread", map[string]any{
		"socket_path": c.Ident.SocketPath, "session_id": c.Ident.SessionID, "session_created": c.Ident.SessionCreated,
	})
	if err != nil {
		c.lastErr = err.Error()
		return err
	}
	var unread struct {
		Total int `json:"total"`
	}
	if err := json.Unmarshal(raw, &unread); err != nil {
		return fmt.Errorf("decode session_unread: %w", err)
	}
	if unread.Total > 0 {
		// Resolve the session's own aliases so the summary's drain clause can
		// name them (best-effort: an unresolved registration falls back to the
		// generic placeholder). resolve() takes no lock, so calling it while we
		// hold c.mu is safe.
		aliases, _ := c.resolve()
		c.aliases = aliases
		c.push(Summary(unread.Total, aliases))
	}
	return nil
}

// Tick is one poll: re-resolve aliases, read everything past the cursor that
// concerns them, drop self-authored and non-mail rows, push one envelope.
func (c *Carrier) Tick() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Ident.paneless() {
		return nil
	}
	aliases, err := c.resolve()
	if err != nil {
		c.lastErr = err.Error()
		return err
	}
	c.aliases = aliases
	if len(aliases) == 0 {
		c.lastErr = "session not registered on the bus yet (waiting for register_agent)"
		return nil
	}
	mine := make(map[string]bool, len(aliases))
	for _, a := range aliases {
		mine[a] = true
	}
	seen := map[int64]bool{}
	var batch []Event
	// head advances to the largest max_id any per-alias query reports. With
	// several aliases there is a narrow race: an event for an already-queried
	// alias can land before a later alias's query raises max_id past it, and
	// that event is then never pushed. Accepted: multi-alias sessions are
	// rare, the 📬 badge and Stop-hook drain still deliver the mail, and the
	// alternative (advancing past pushed ids only) re-pushes duplicates.
	head := c.cursor
	for _, a := range aliases {
		raw, err := c.Call("list_events", map[string]any{"agent": a, "after_id": c.cursor})
		if err != nil {
			c.lastErr = err.Error()
			return err
		}
		var out struct {
			Events []Event `json:"events"`
			MaxID  int64   `json:"max_id"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			c.lastErr = err.Error()
			return fmt.Errorf("decode list_events: %w", err)
		}
		if out.MaxID > head {
			head = out.MaxID
		}
		for _, e := range out.Events {
			if seen[e.ID] || !mailKinds[e.Kind] || mine[e.Agent] {
				continue
			}
			seen[e.ID] = true
			// Wake discipline: only ACTIVELY push events that warrant an
			// interrupt. The rest are carried by the mailbox badge + the
			// next-turn Stop-hook drain (the cursor still advances past them
			// below, so they are never re-examined). This is what keeps a
			// broadcast from waking every session at once and an ack from
			// re-waking the whole audience.
			if !shouldPush(e, mine) {
				continue
			}
			batch = append(batch, e)
		}
	}
	c.cursor = head
	c.lastErr = ""
	if len(batch) > 0 {
		content, meta := Format(batch, aliases)
		c.push(content, meta)
	}
	return nil
}

// push hands one envelope to the channel server. Must hold c.mu.
func (c *Carrier) push(content string, meta map[string]string) {
	if err := c.Notify(content, meta); err != nil {
		c.lastErr = "push failed: " + err.Error()
		_, _ = fmt.Fprintln(c.errw(), "[muster channel]", c.lastErr)
		return
	}
	c.lastPush = time.Now()
}

// Run is Start followed by Tick every Interval until ctx ends. Errors are
// logged and retried on the next tick — a daemon restart must not kill the
// channel, and the process lives exactly as long as the session does.
func (c *Carrier) Run(ctx context.Context) {
	interval := c.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	if interval < MinInterval {
		interval = MinInterval
	}
	sleep := c.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	if err := c.Start(); err != nil {
		_, _ = fmt.Fprintln(c.errw(), "[muster channel] start:", err)
	}
	for ctx.Err() == nil {
		if err := c.Tick(); err != nil {
			_, _ = fmt.Fprintln(c.errw(), "[muster channel] tick:", err)
		}
		sleep(interval)
	}
}

// Status is the muster_channel_status tool's answer: who this channel pushes
// for, where the cursor sits, when it last pushed, and why it is idle if it is.
func (c *Carrier) Status() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var b strings.Builder
	if c.Ident.paneless() {
		b.WriteString("idle: no tmux pane — the channel only serves sessions running inside tmux.\n")
		return b.String()
	}
	fmt.Fprintf(&b, "pane %s on session %s (socket %s)\n", c.Ident.PaneID, c.Ident.SessionID, c.Ident.SocketPath)
	if len(c.aliases) == 0 {
		b.WriteString("aliases: none — session not registered on the bus yet; call register_agent (or wait for the SessionStart hook)\n")
	} else {
		fmt.Fprintf(&b, "aliases: %s\n", strings.Join(c.aliases, ", "))
	}
	fmt.Fprintf(&b, "journal cursor %d\n", c.cursor)
	if c.lastPush.IsZero() {
		b.WriteString("last push: never\n")
	} else {
		fmt.Fprintf(&b, "last push: %s ago\n", time.Since(c.lastPush).Round(time.Second))
	}
	if c.lastErr != "" {
		fmt.Fprintf(&b, "last problem: %s\n", c.lastErr)
	}
	return b.String()
}

// Identify is the part of Status a channel that does not run the carrier can
// still answer: the pane, and the aliases the daemon holds for it now.
func (c *Carrier) Identify() string {
	if c.Ident.paneless() {
		return "idle: no tmux pane — muster only reaches sessions running inside tmux.\n"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "pane %s on session %s (socket %s)\n", c.Ident.PaneID, c.Ident.SessionID, c.Ident.SocketPath)
	aliases, err := c.resolve()
	switch {
	case err != nil:
		fmt.Fprintf(&b, "aliases: unknown (%v)\n", err)
	case len(aliases) == 0:
		b.WriteString("aliases: none — session not registered on the bus yet; call register_agent (or wait for the SessionStart hook)\n")
	default:
		fmt.Fprintf(&b, "aliases: %s\n", strings.Join(aliases, ", "))
	}
	return b.String()
}
