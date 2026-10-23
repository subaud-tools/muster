package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	tools "github.com/subaud-tools/tools-common"

	"github.com/subaud-tools/muster/internal/version"
)

// musterCLI runs muster exactly as the binary does for every command main()
// does not route itself: NewApp().Dispatch.
func musterCLI(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errw bytes.Buffer
	code = NewApp().Dispatch(args, &out, &errw)
	return out.String(), errw.String(), code
}

type indexEntry struct {
	Name       string   `json:"name"`
	Aliases    []string `json:"aliases"`
	SelfRouted bool     `json:"selfRouted"`
}

func commandIndex(t *testing.T) []indexEntry {
	t.Helper()
	stdout, stderr, code := musterCLI(t, "commands", "--json")
	if code != 0 {
		t.Fatalf("commands --json: exit %d, %s", code, stderr)
	}
	var idx []indexEntry
	if err := json.Unmarshal([]byte(stdout), &idx); err != nil {
		t.Fatal(err)
	}
	return idx
}

// Every alias (muster accepts its MCP tool names as CLI words) resolves for
// help and for -h, for every command that has one, not a sample.
func TestEveryAliasDispatches(t *testing.T) {
	n := 0
	for _, c := range commandIndex(t) {
		for _, a := range c.Aliases {
			n++
			if out, errw, code := musterCLI(t, "help", a); code != 0 || !strings.Contains(out, "Usage: muster "+c.Name) {
				t.Errorf("help %s: exit %d, stdout %q, stderr %q", a, code, out, errw)
			}
			if _, errw, code := musterCLI(t, a, "-h"); code != 0 {
				t.Errorf("%s -h: exit %d, stderr %q", a, code, errw)
			}
		}
	}
	if n < 6 {
		t.Fatalf("only %d aliases registered; the MCP tool-name aliases are missing", n)
	}
}

func TestVersionUnchanged(t *testing.T) {
	want := "muster " + tools.Version{Number: version.Version(), Commit: version.Commit(), Date: version.Date()}.String()
	for _, arg := range []string{"version", "--version", "-v"} {
		out, _, code := musterCLI(t, arg)
		if code != 0 || strings.TrimSpace(out) != want {
			t.Fatalf("%s: exit %d, %q, want %q", arg, code, out, want)
		}
	}
}

// A hook must never fail a harness: with no daemon reachable it exits 0.
func TestHookNeverFails(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MUSTER_HOME", dir)
	t.Setenv("MUSTER_NO_AUTOSPAWN", "1")
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	pinAncestryWalkAway(t)
	for _, args := range [][]string{{"hook", "Stop", "claude"}, {"hook", "SessionStart", "claude"}, {"hook", "SessionEnd", "claude"}} {
		if _, errw, code := musterCLI(t, args...); code != 0 {
			t.Errorf("%v: exit %d, stderr %q", args, code, errw)
		}
	}
}

// The machine-readable success output is the command's own; dispatch through
// tools.App adds and removes nothing.
func TestJSONSuccessOutputUnchanged(t *testing.T) {
	startTestDaemon(t)
	// whereami needs a resolvable pane; pin it (CI has no tmux, and a dev
	// machine's real pane must not decide the answer).
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	stubAncestryMatch(t)
	if _, err := callData("register_agent", map[string]any{"alias": "web/a", "model_type": "claude", "project": "web"}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args []string
		run  func([]string, *bytes.Buffer) error
	}{
		{[]string{"status", "--json"}, func(a []string, b *bytes.Buffer) error { return cmdStatus(a, b) }},
		{[]string{"whereami", "--json"}, func(a []string, b *bytes.Buffer) error { return cmdWhereami(a, b) }},
		{[]string{"standing", "web", "--json"}, func(a []string, b *bytes.Buffer) error { return cmdStanding(a, b) }},
	} {
		var direct bytes.Buffer
		derr := tc.run(tc.args[1:], &direct)
		out, errw, code := musterCLI(t, tc.args...)
		if derr != nil {
			t.Fatalf("%v direct: %v", tc.args, derr)
		}
		if code != 0 || out != direct.String() {
			t.Fatalf("%v: exit %d (stderr %q)\nvia dispatch: %q\ndirect:       %q", tc.args, code, errw, out, direct.String())
		}
	}
}

func TestUnknownCommandIsUsageError(t *testing.T) {
	_, errw, code := musterCLI(t, "bogus")
	if code != 2 || !strings.Contains(errw, `unknown command "bogus"`) {
		t.Fatalf("exit %d, stderr %q", code, errw)
	}
}

func TestBareMusterIsUsageError(t *testing.T) {
	out, errw, code := musterCLI(t)
	if code != 2 || out != "" || !strings.HasPrefix(errw, "usage: muster") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errw)
	}
}

// help, man and commands --json still cover the commands main() routes itself.
func TestManAndCommandsCoverSelfRouted(t *testing.T) {
	self := map[string]bool{}
	for _, c := range commandIndex(t) {
		if c.SelfRouted {
			self[c.Name] = true
		}
	}
	man, _, _ := musterCLI(t, "man")
	for _, name := range []string{"serve", "mcp", "channel", "watch", "debug", "lambda"} {
		if !self[name] {
			t.Errorf("commands --json does not mark %s self-routed", name)
		}
		if !strings.Contains(man, ".B "+name) {
			t.Errorf("man page lacks %s", name)
		}
	}
}

// help shows muster's overview above the grouped list, in muster's group order.
func TestHelpIsGroupedWithOverview(t *testing.T) {
	out, _, code := musterCLI(t, "help")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	idx := []int{
		strings.Index(out, "local multi-agent coordination bus"),
		strings.Index(out, "\nTalk\n"),
		strings.Index(out, "\nWatch\n"),
		strings.Index(out, "\nIdentity\n"),
		strings.Index(out, "\nPlumbing\n"),
	}
	for i, v := range idx {
		if v < 0 || (i > 0 && v < idx[i-1]) {
			t.Fatalf("help lacks the overview or the groups in order (%v):\n%s", idx, out)
		}
	}
}

// `standing set -h` belongs to standing's sub-verb, not to tools.App.
func TestStandingSubverbHelp(t *testing.T) {
	for _, args := range [][]string{{"standing", "-h"}, {"standing", "set", "-h"}, {"standing", "retract", "-h"}} {
		out, errw, code := musterCLI(t, args...)
		if code != 0 || !strings.Contains(out, "standing") {
			t.Errorf("%v: exit %d, stdout %q, stderr %q", args, code, out, errw)
		}
	}
}
