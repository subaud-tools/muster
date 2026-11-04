package serve

// Run's server, entry, stop and token are pageserve's, tested there; this
// tests what muster adds: its agents page and route.

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/subaud-tools/muster/internal/daemon"
	"github.com/subaud-tools/muster/internal/proto"
	"github.com/subaud-tools/muster/internal/store"
	"github.com/subaud-tools/tools-common/localweb"
	"github.com/subaud-tools/tools-common/pageserve"
)

func dispatch(t *testing.T, d *daemon.Daemon, op string, args map[string]any) {
	t.Helper()
	if resp := d.Dispatch(proto.Request{Op: op, Args: args}); !resp.OK {
		t.Fatalf("%s: %+v", op, resp)
	}
}

// TestAgentsListsARegisteredAgentWithItsUnread: GET /api/agents is one row per
// agent from list_agents, joined with status's counts.
func TestAgentsListsARegisteredAgentWithItsUnread(t *testing.T) {
	t.Setenv("MUSTER_HOME", t.TempDir())
	s, err := store.Open(filepath.Join(t.TempDir(), "bus.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	d := daemon.New(s, nil, "")
	for _, alias := range []string{"sender", "worker"} {
		dispatch(t, d, "register_agent", map[string]any{"alias": alias, "role": "r", "model_type": "claude", "project": "proj"})
	}
	dispatch(t, d, "send_message", map[string]any{
		"from": "sender", "to_kind": "agent", "to_target": "worker",
		"subject": "s", "body": "b", "intent": "action-requested",
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, d, Options{Version: "v"}) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	var e pageserve.Entry
	for deadline := time.Now().Add(5 * time.Second); ; {
		if e, err = Live().Running(); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no entry: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	req, _ := http.NewRequest(http.MethodGet, e.Base+"/api/agents", nil)
	req.Header.Set(localweb.TokenHeader, e.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/agents: %d", resp.StatusCode)
	}
	var got struct {
		Agents []Agent `json:"agents"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	var worker *Agent
	for i := range got.Agents {
		if got.Agents[i].Alias == "worker" {
			worker = &got.Agents[i]
		}
	}
	if worker == nil {
		t.Fatalf("worker not listed: %+v", got.Agents)
	}
	if worker.Unread != 1 || worker.ActionRequired != 1 || worker.Project != "proj" {
		t.Fatalf("worker = %+v, want project proj, 1 unread, 1 action required", *worker)
	}

	noToken, err := http.Get(e.Base + "/api/agents")
	if err != nil {
		t.Fatal(err)
	}
	_ = noToken.Body.Close()
	if noToken.StatusCode == http.StatusOK {
		t.Fatal("GET /api/agents answered without the token")
	}
	index, err := http.Get(e.Base + "/")
	if err != nil {
		t.Fatal(err)
	}
	_ = index.Body.Close()
	if index.StatusCode != http.StatusOK {
		t.Fatalf("GET /: %d", index.StatusCode)
	}
}

// countingDispatcher counts the ops it is asked, answering each with no rows.
type countingDispatcher struct{ n atomic.Int64 }

func (c *countingDispatcher) Dispatch(proto.Request) proto.Response {
	c.n.Add(1)
	return proto.Response{OK: true, Data: map[string]any{}}
}

// With no page open, the rows are never read: on the remote backend each read
// is a scan of every agent ever registered.
func TestWatchReadsNothingWithNoPageOpen(t *testing.T) {
	d := &countingDispatcher{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { watch(ctx, d, pageserve.NewEvents(), time.Millisecond); close(done) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done
	if n := d.n.Load(); n != 0 {
		t.Fatalf("read the bus %d times with no page open", n)
	}
}
