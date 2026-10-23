package cli

import (
	"bytes"
	"sort"
	"strings"
	"testing"
)

// wantCommandNames is muster's full subcommand vocabulary, independent of
// Registry — a second, hand-maintained list so a future edit that silently
// drops (or typos) a Registry entry fails a test instead of shipping quietly.
// Keep this in sync with CLAUDE.md's package-map/usage line and
// cmd/muster/main.go's routing comment.
var wantCommandNames = []string{
	"send", "nudge", "reply", "standing",
	"agents", "status", "inbox", "tasks", "thread", "events", "tail", "station",
	"register", "become", "deregister", "label", "whereami", "device", "gc",
	"serve", "mcp", "channel", "lambda", "hook", "setup", "debug",
	// tools.App built-ins
	"help", "version", "man", "commands", "update",
}

// mainOwnedCommands are the Registry names cmd/muster's main() dispatches
// directly (never through tools.App's dispatch) — they need process-level setup
// (daemon startup, MCP stdio framing, a one-off raw daemon call, the Lambda
// runtime behind a build tag) this package deliberately doesn't do. Every
// other Registry command must have a non-nil Run.
var mainOwnedCommands = map[string]bool{"serve": true, "mcp": true, "channel": true, "lambda": true, "debug": true}

// TestMCPToolNameAliasesResolve pins the cross-surface aliases: the CLI must
// accept the MCP tool name for the same operation, so an agent that knows the
// get_inbox TOOL and drops to a shell never has to translate it into `muster
// inbox` (the mistranslation that left a mailbox badge lit). Aliases resolve
// in lookup but must NOT appear as their own rows in the command vocabulary.
func TestMCPToolNameAliasesResolve(t *testing.T) {
	want := map[string]string{
		"get_inbox":        "inbox",
		"get_thread":       "thread",
		"send_message":     "send",
		"register_agent":   "register",
		"deregister_agent": "deregister",
		"list_agents":      "agents",
	}
	for alias, canonical := range want {
		cmd, ok := lookup(alias)
		if !ok {
			t.Errorf("alias %q did not resolve", alias)
			continue
		}
		if cmd.Name != canonical {
			t.Errorf("alias %q resolved to %q, want %q", alias, cmd.Name, canonical)
		}
		// An alias must not itself be a top-level command name.
		for _, n := range appCommandNames(t) {
			if n == alias {
				t.Errorf("alias %q leaked into the command vocabulary", alias)
			}
		}
	}
}

// appCommandNames is every command the app answers to by name (Registry rows
// and tools.App built-ins), not aliases.
func appCommandNames(t *testing.T) []string {
	t.Helper()
	var names []string
	for _, c := range commandIndex(t) {
		names = append(names, c.Name)
	}
	return names
}

func TestRegistryCompleteness(t *testing.T) {
	got := appCommandNames(t)
	want := append([]string(nil), wantCommandNames...)
	sort.Strings(got)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("Registry has %d commands, want %d\ngot:  %v\nwant: %v", len(got), len(want), got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("Registry command set mismatch:\ngot:  %v\nwant: %v", got, want)
		}
	}
}

// TestRegistryRunNilOnlyForMainOwned enforces the Command.Run contract
// documented on the type: nil exactly for the three commands main() owns,
// non-nil (and dispatchable) for every other Registry entry. This is the
// "switch vs registry" cross-check the CLI-help spec asks for — Dispatch has
// no separate switch anymore (it walks Registry directly via lookup), so the
// only place drift could reappear is a Run left nil (or wrongly non-nil).
func TestRegistryRunNilOnlyForMainOwned(t *testing.T) {
	for _, c := range Registry {
		wantNil := mainOwnedCommands[c.Name]
		gotNil := c.Run == nil
		if gotNil != wantNil {
			t.Errorf("%s: Run nil = %v, want %v (mainOwnedCommands = %v)", c.Name, gotNil, wantNil, mainOwnedCommands[c.Name])
		}
	}
}

// TestEveryCommandHelpExitsCleanAndDocumentsSynopsis is table-driven over
// Registry: `muster <cmd> -h` must succeed (nil error, i.e. exit 0) and its
// output must contain the command's synopsis — the same guarantee
// `muster help <cmd>` gives, exercised through the real dispatch path
// (Command.Run) instead of calling HelpFor directly, so it also proves each
// command's own flag.ErrHelp/helpRequested interception actually fires.
func TestEveryCommandHelpExitsCleanAndDocumentsSynopsis(t *testing.T) {
	for _, c := range Registry {
		if c.Run == nil {
			continue // serve/mcp/debug: help-tested at the main package level
		}
		t.Run(c.Name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := Dispatch([]string{c.Name, "-h"}, &buf); err != nil {
				t.Fatalf("%s -h: unexpected error: %v", c.Name, err)
			}
			if !strings.Contains(buf.String(), c.Synopsis) {
				t.Fatalf("%s -h output missing synopsis %q:\n%s", c.Name, c.Synopsis, buf.String())
			}

			buf.Reset()
			if err := Dispatch([]string{c.Name, "--help"}, &buf); err != nil {
				t.Fatalf("%s --help: unexpected error: %v", c.Name, err)
			}
			if !strings.Contains(buf.String(), c.Synopsis) {
				t.Fatalf("%s --help output missing synopsis %q:\n%s", c.Name, c.Synopsis, buf.String())
			}
		})
	}
}

// TestHelpCommandMatchesDirectFlag checks `muster help <cmd>` renders
// identically to `muster <cmd> -h`, for every Registry command (main-owned
// ones included, since `help <cmd>` always routes through HelpFor
// regardless of who owns Run).
func TestHelpCommandMatchesDirectFlag(t *testing.T) {
	for _, c := range Registry {
		t.Run(c.Name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := Dispatch([]string{"help", c.Name}, &buf); err != nil {
				t.Fatalf("help %s: unexpected error: %v", c.Name, err)
			}
			if !strings.Contains(buf.String(), c.Synopsis) {
				t.Fatalf("help %s output missing synopsis %q:\n%s", c.Name, c.Synopsis, buf.String())
			}
			if !strings.Contains(buf.String(), c.Summary) {
				t.Fatalf("help %s output missing summary %q:\n%s", c.Name, c.Summary, buf.String())
			}
		})
	}
}
