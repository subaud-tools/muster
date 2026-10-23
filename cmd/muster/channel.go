package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/subaud-tools/muster/internal/channel"
	"github.com/subaud-tools/muster/internal/paths"
	"github.com/subaud-tools/muster/internal/tmuxenv"
	"github.com/subaud-tools/muster/internal/version"
	"github.com/subaud-tools/tools-common/channelmcp"
	"github.com/subaud-tools/tools-common/harness"
)

// channelCapture resolves the tmux identity the channel pushes for: the
// environment when the harness passed $TMUX/$TMUX_PANE through, else the
// process-ancestry walk. This mirrors cli.hookCapture on purpose — the
// channel MCP server is a synchronous, long-lived child of the harness
// process, so it shares the pane's ancestry exactly as a hook does. Without
// the fallback, a harness that spawns MCP servers env-stripped (the hook is
// registered via ancestry, but the channel saw only $TMUX) leaves the carrier
// paneless and silently idle: mail lands in the inbox but never wakes the
// session (the 2026-09 "no tmux identity" report).
func channelCapture() tmuxenv.Capture {
	if c := tmuxenv.CaptureEnv(); c.SocketPath != "" && c.PaneID != "" {
		return c
	}
	return tmuxenv.CaptureFromAncestry()
}

// channelInstructions is the durable core, taught once at handshake: what
// the channel is and how a push is shaped. Everything specific to a kind of
// event travels WITH that event, after the separator, so the rule sits next
// to the action it governs instead of thousands of tokens back.
const channelInstructions = `This channel is muster, the local coordination bus. An event from muster means mail arrived on the bus for THIS session: a <channel source="muster-channel"> message, or a turn whose first line is muster event.
- Each event is an envelope line (intent, sender, thread id, subject — never the body), then a line containing only "---", then guidance for that event. Follow the guidance; it names the tool to call.
- The body lives in the bus: get_thread <id> reads one thread, get_inbox lists everything unread, reply answers.
- Act autonomously. Do not ask the user whether to check mail; reading it is the point.
- muster_channel_status reports which aliases this channel pushes for and why it might be idle.`

// ChannelMaxListedEnv caps how many threads a batch push still lists on its
// envelope line; above it the push collapses to a count and the strictest
// intent. Non-numeric or non-positive values fall back to the default.
const ChannelMaxListedEnv = "MUSTER_CHANNEL_MAX_LISTED"

// channelMaxListed reads ChannelMaxListedEnv, defaulting to channel.MaxListed.
func channelMaxListed() int {
	raw := os.Getenv(ChannelMaxListedEnv)
	if raw == "" {
		return channel.MaxListed
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		fmt.Fprintf(os.Stderr, "muster: ignoring %s=%q (want a positive integer), using %d\n", ChannelMaxListedEnv, raw, channel.MaxListed)
		return channel.MaxListed
	}
	return n
}

// newCarrier is the carrier `muster channel` and `muster watch` share: the
// session's tmux identity, the daemon, and where each push goes.
func newCarrier(capture tmuxenv.Capture, call channel.Client, push channelmcp.Pusher) *channel.Carrier {
	return &channel.Carrier{
		Call:     call,
		Notify:   push.Notify,
		Ident:    channel.Identity{SocketPath: capture.SocketPath, SessionID: capture.SessionID, PaneID: capture.PaneID, SessionCreated: capture.SessionCreated},
		Interval: channelInterval(),
	}
}

// channelProc is `muster channel`: the MCP server and the carrier beside it.
type channelProc struct {
	srv     *channelmcp.Server
	carrier *channel.Carrier
	// serveOnly is `muster channel` under Claude Code: the plugin's mod runs
	// `muster watch`, which pushes the mail, so this process serves
	// muster_channel_status and never runs the carrier. Two carriers would
	// start two turns for each message.
	serveOnly bool
}

func newChannelProc(capture tmuxenv.Capture, call channel.Client) *channelProc {
	p := &channelProc{serveOnly: harness.FromEnv().IsClaude()}
	p.srv = channelmcp.New(channelmcp.Handler{
		Name:         "muster-channel",
		Version:      version.Version(),
		Instructions: channelInstructions,
		Tools: []channelmcp.Tool{{
			Name:        "muster_channel_status",
			Description: "Is the muster channel attached? Reports the tmux pane, the aliases it pushes for, the journal cursor, the last push, and why it is idle if it is.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		}},
		Call: func(name string, _ json.RawMessage) (string, error) {
			if name != "muster_channel_status" {
				return "", fmt.Errorf("unknown tool %q", name)
			}
			if p.serveOnly {
				return "This is a Claude Code session: mail reaches it through `muster watch`, which the muster plugin starts, " +
					"not through this process, so this channel pushes nothing. If mail never arrives, check that the muster plugin " +
					"is installed and enabled (/plugin).\n" + p.carrier.Identify(), nil
			}
			return p.carrier.Status(), nil
		},
	})
	p.carrier = newCarrier(capture, call, p.srv)
	return p
}

// run serves MCP on r/w and, unless the channel is serve-only, runs the
// carrier beside it until ctx ends or the reader closes.
func (p *channelProc) run(ctx context.Context, r io.Reader, w io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if p.serveOnly {
		fmt.Fprintln(os.Stderr, "[muster channel] Claude Code session: serving tools only; the muster plugin's `muster watch` pushes the mail")
	} else {
		go p.carrier.Run(ctx)
	}
	return p.srv.Run(r, w)
}

// runChannel serves a claude/channel MCP server on stdio for the session that
// launched it and runs the carrier beside it. stdout is the protocol.
//
// Shutdown has two triggers and both must end the process: the client
// closing stdin (srv.Run returns), or SIGINT/SIGTERM. signal.NotifyContext
// suppresses the signal's default action, so if the server loop were the
// only thing we waited on, a harness that sends SIGTERM while still holding
// our pipes would leave a process that never exits — measured by the
// pi-channels client, which had to SIGKILL after a grace period.
func runChannel() {
	channel.MaxListed = channelMaxListed()
	p := newChannelProc(channelCapture(), channel.DaemonClient(paths.SocketPath()))

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- p.run(ctx, os.Stdin, os.Stdout) }()
	select {
	case <-ctx.Done():
		// Signalled while the client still holds our pipes: exit now.
		cancel()
	case err := <-done:
		cancel()
		if err != nil {
			fmt.Fprintln(os.Stderr, "muster: channel:", err)
			os.Exit(1)
		}
	}
}

// runWatch is `muster watch`: the channel's carrier, printing each push as
// one JSON line on stdout for the Claude Code plugin's mod. It binds to the
// same tmux identity the channel would (the mod starts it as a child of the
// claude process inside the pane). stdout carries only those lines; every
// diagnostic goes to stderr. It runs until signalled.
func runWatch() {
	channel.MaxListed = channelMaxListed()
	capture := channelCapture()
	fmt.Fprintf(os.Stderr, "[muster watch] pane=%q session=%q\n", capture.PaneID, capture.SessionID)
	c := newCarrier(capture, channel.DaemonClient(paths.SocketPath()), channelmcp.NewLines(os.Stdout))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	c.Run(ctx)
}
