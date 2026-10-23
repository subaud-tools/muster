package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestTailBacklogThenFollowsAndResets(t *testing.T) {
	startTestDaemon(t)
	if _, err := callData("register_agent", map[string]any{"alias": "api", "model_type": "claude"}); err != nil {
		t.Fatal(err)
	}
	if _, err := callData("send_message", map[string]any{"from": "web", "to_kind": "agent", "to_target": "api", "subject": "first", "body": "b"}); err != nil {
		t.Fatal(err)
	}
	polls := 0
	var out, errw bytes.Buffer
	o := tailOpts{
		maxPolls: 2,
		errw:     &errw,
		wait: func(time.Duration) bool {
			polls++
			if polls == 1 { // inject a new event between poll 1 and 2
				if _, err := callData("reply", map[string]any{"thread_id": 1, "from": "api", "body": "done"}); err != nil {
					t.Fatal(err)
				}
			}
			return true
		},
	}
	if err := cmdTail([]string{"--interval", "1ms"}, &out, o); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "send") || !strings.Contains(got, "first") {
		t.Fatalf("backlog missing:\n%s", got)
	}
	if !strings.Contains(got, "reply") {
		t.Fatalf("followed event missing:\n%s", got)
	}
	sendIdx, replyIdx := strings.Index(got, "send"), strings.Index(got, "reply")
	if sendIdx > replyIdx {
		t.Fatalf("backlog must print before followed rows:\n%s", got)
	}
}

func TestTailBacklogZeroPrintsNoHistory(t *testing.T) {
	startTestDaemon(t)
	if _, err := callData("register_agent", map[string]any{"alias": "api", "model_type": "claude"}); err != nil {
		t.Fatal(err)
	}
	if _, err := callData("send_message", map[string]any{"from": "web", "to_kind": "agent", "to_target": "api", "subject": "old", "body": "b"}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	o := tailOpts{maxPolls: 1, wait: func(time.Duration) bool { return true }}
	if err := cmdTail([]string{"--backlog", "0"}, &out, o); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "old") {
		t.Fatalf("--backlog 0 printed history:\n%s", out.String())
	}
}
