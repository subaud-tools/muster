package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/subaud-tools/muster/internal/channel"
	"github.com/subaud-tools/muster/internal/daemon"
	"github.com/subaud-tools/muster/internal/mustertest"
	"github.com/subaud-tools/muster/internal/store"
	"github.com/subaud-tools/muster/internal/tmuxenv"
	"github.com/subaud-tools/tools-common/channelmcp"
)

// `muster watch` is the channel's carrier with a JSON line on stdout for each
// push, which is what the Claude Code plugin's mod reads. Mail to this pane's
// alias arrives as one line, {"content": ..., "meta": {...}}, with the same
// envelope the channel pushes.
func TestWatchPrintsMailAsOneLine(t *testing.T) {
	dir, cleanup, err := mustertest.ShortHome()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	t.Setenv("MUSTER_HOME", dir)
	t.Setenv("MUSTER_NO_AUTOSPAWN", "1")
	s, err := store.Open(filepath.Join(dir, "bus.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	sock := filepath.Join(dir, "sock")
	d, err := daemon.Serve(sock, s, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	call := channel.DaemonClient(sock)
	if _, err := call("register_agent", map[string]any{"alias": "worker", "role": "producer", "model_type": "claude",
		"socket_path": "/tmp/tsock", "session_id": "$3", "pane_id": "%9", "session_name": "worker", "session_created": 1700000000}); err != nil {
		t.Fatal(err)
	}

	r, w := io.Pipe()
	c := newCarrier(tmuxenv.Capture{SocketPath: "/tmp/tsock", SessionID: "$3", PaneID: "%9", SessionCreated: 1700000000},
		call, channelmcp.NewLines(w))
	c.Interval = channel.MinInterval
	c.Errw = io.Discard
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go c.Run(ctx)

	lines := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 1<<20), 1<<24)
		if sc.Scan() {
			lines <- sc.Text()
		}
	}()
	time.Sleep(2 * channel.MinInterval) // the carrier places its cursor first
	if _, err := call("send_message", map[string]any{"from": "lead", "to_kind": "agent", "to_target": "worker",
		"subject": "review the branch", "body": "please", "intent": "action-requested"}); err != nil {
		t.Fatal(err)
	}
	var line string
	select {
	case line = <-lines:
	case <-time.After(5 * time.Second):
		t.Fatal("no line printed for the mail")
	}
	var ev struct {
		Content string            `json:"content"`
		Meta    map[string]string `json:"meta"`
	}
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		t.Fatalf("not one JSON line: %q (%v)", line, err)
	}
	if !strings.Contains(ev.Content, "action-requested from lead") || !strings.Contains(ev.Content, `"review the branch"`) {
		t.Fatalf("content = %q", ev.Content)
	}
}

// fakeBus answers the carrier's daemon calls for one registered alias and
// counts the journal reads, which only a carrier that is running makes.
type fakeBus struct {
	mu    sync.Mutex
	reads int
}

func (f *fakeBus) call(op string, _ map[string]any) (json.RawMessage, error) {
	switch op {
	case "session_aliases":
		return json.RawMessage(`{"aliases":["worker"]}`), nil
	case "session_unread":
		return json.RawMessage(`{"total":0}`), nil
	case "list_events":
		f.mu.Lock()
		f.reads++
		f.mu.Unlock()
		return json.RawMessage(`{"events":[],"max_id":0}`), nil
	}
	return json.RawMessage(`{}`), nil
}

// Under Claude Code the plugin's mod runs `muster watch`, so `muster channel`
// must not push too: it serves muster_channel_status and leaves the journal
// alone, and the status says where the mail comes from instead. Under pi it
// runs the carrier as it always has.
func TestChannelCarriesOnlyOutsideClaudeCode(t *testing.T) {
	cases := []struct {
		name       string
		claude, pi string
		carries    bool
	}{
		{"pi", "", "pi-session", true},
		{"claude code", "claude-session", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CLAUDE_CODE_SESSION_ID", tc.claude)
			t.Setenv("AGENT_SESSION_ID", tc.pi)
			t.Setenv("AGENT_SESSION_CHILD", "")
			bus := &fakeBus{}
			p := newChannelProc(tmuxenv.Capture{SocketPath: "/tmp/tsock", SessionID: "$3", PaneID: "%9", SessionCreated: 1}, bus.call)
			p.carrier.Interval = channel.MinInterval
			p.carrier.Errw = io.Discard
			inR, inW := io.Pipe()
			outR, outW := io.Pipe()
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { _ = p.run(ctx, inR, outW); close(done) }()
			t.Cleanup(func() { cancel(); _ = inW.Close(); <-done; _ = outW.Close() })

			_, _ = io.WriteString(inW, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"muster_channel_status","arguments":{}}}`+"\n")
			sc := bufio.NewScanner(outR)
			sc.Buffer(make([]byte, 1<<20), 1<<24)
			if !sc.Scan() {
				t.Fatal("no answer to muster_channel_status")
			}
			status := sc.Text()
			go func() {
				for sc.Scan() { //nolint:revive // drain stdout so the server never blocks
				}
			}()
			time.Sleep(3 * channel.MinInterval)
			bus.mu.Lock()
			n := bus.reads
			bus.mu.Unlock()
			if tc.carries && n == 0 {
				t.Fatal("the channel never read the journal under pi")
			}
			if !tc.carries {
				if n != 0 {
					t.Fatalf("the channel read the journal %d times under Claude Code; only watch may", n)
				}
				for _, want := range []string{"muster watch", "%9", "worker"} {
					if !strings.Contains(status, want) {
						t.Fatalf("status lacks %q:\n%s", want, status)
					}
				}
			}
		})
	}
}
