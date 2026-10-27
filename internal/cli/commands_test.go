package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// commandsJSONRow mirrors the family shape tools.CommandsJSON emits, for
// unmarshaling in tests.
type commandsJSONRow struct {
	Name       string `json:"name"`
	Synopsis   string `json:"synopsis"`
	Summary    string `json:"summary"`
	Group      string `json:"group"`
	Help       string `json:"help"`
	SelfRouted bool   `json:"selfRouted"`
	Flags      []struct {
		Name    string `json:"name"`
		Type    string `json:"type"`
		Default string `json:"default"`
		Usage   string `json:"usage"`
	} `json:"flags"`
}

// TestCommandsJSONCoversRegistry checks `muster commands --json` against the
// family shape (tools.CommandsJSON) and full Registry coverage: every
// Registry command appears exactly once, a dispatchable command (Run != nil)
// reports selfRouted: false, a process-mode command (Run == nil, owned
// directly by cmd/muster's main()) reports selfRouted: true, and a
// flag-bearing command's flags are populated.
func TestCommandsJSONCoversRegistry(t *testing.T) {
	var buf bytes.Buffer
	if err := Dispatch([]string{"commands", "--json"}, &buf); err != nil {
		t.Fatalf("commands --json: unexpected error: %v", err)
	}

	var rows []commandsJSONRow
	if err := json.Unmarshal(buf.Bytes(), &rows); err != nil {
		t.Fatalf("commands --json output does not unmarshal as a JSON array: %v\noutput:\n%s", err, buf.String())
	}

	// Registry plus tools.App's seven built-ins (help, version, man, commands,
	// update, login, logout).
	if len(rows) != len(Registry)+7 {
		t.Fatalf("commands --json listed %d commands, want Registry's %d plus 7 built-ins", len(rows), len(Registry))
	}

	byName := make(map[string]commandsJSONRow, len(rows))
	for _, r := range rows {
		byName[r.Name] = r
	}
	for _, c := range Registry {
		if _, ok := byName[c.Name]; !ok {
			t.Errorf("commands --json is missing Registry command %q", c.Name)
		}
	}

	send, ok := byName["send"]
	if !ok {
		t.Fatal("commands --json missing \"send\"")
	}
	if send.SelfRouted {
		t.Errorf("\"send\" (Run != nil in Registry) reported selfRouted: true, want false")
	}

	serve, ok := byName["serve"]
	if !ok {
		t.Fatal("commands --json missing \"serve\"")
	}
	if !serve.SelfRouted {
		t.Errorf("\"serve\" (Run == nil in Registry, owned by cmd/muster's main()) reported selfRouted: false, want true")
	}

	status, ok := byName["status"]
	if !ok {
		t.Fatal("commands --json missing \"status\"")
	}
	if len(status.Flags) == 0 {
		t.Errorf("\"status\" has flags in its NewFlags constructor but commands --json reported none")
	}
	foundJSONFlag := false
	for _, fl := range status.Flags {
		if fl.Name == "json" {
			foundJSONFlag = true
		}
	}
	if !foundJSONFlag {
		t.Errorf("\"status\" flags missing the known --json flag: %+v", status.Flags)
	}
}

// TestCommandsBareShowsGroupedListing checks that bare `muster commands`
// (no --json) renders the same grouped usage listing as bare `muster` /
// `muster help`.
func TestCommandsBareShowsGroupedListing(t *testing.T) {
	var buf bytes.Buffer
	if err := Dispatch([]string{"commands"}, &buf); err != nil {
		t.Fatalf("commands: unexpected error: %v", err)
	}
	out := buf.String()
	for _, heading := range []string{"\nTalk\n", "\nWatch\n", "\nIdentity\n", "\nPlumbing\n"} {
		if !strings.Contains(out, heading) {
			t.Errorf("commands output missing group heading %q:\n%s", heading, out)
		}
	}
	if !strings.Contains(out, "commands") {
		t.Errorf("commands output does not list itself:\n%s", out)
	}
}
