package cli

import (
	"bytes"
	"encoding/json"
	"github.com/subaud-tools/tools-common/hookconfig"
	"github.com/subaud-tools/tools-common/setupbundle"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	toml "github.com/pelletier/go-toml/v2"
)

// setupHome points setup at a fresh temp HOME and a PATH with no `kempt` on
// it, so the direct-apply path is exercised by default. It returns the home
// dir. (This is not a unix-socket test, so t.TempDir's long path is fine.)
func setupHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	// Isolate PATH so exec.LookPath("kempt") fails unless a test opts in.
	t.Setenv("PATH", t.TempDir())
	return home
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return m
}

func runSetup(t *testing.T, args ...string) string {
	t.Helper()
	var buf bytes.Buffer
	if err := cmdSetup(args, &buf); err != nil {
		t.Fatalf("cmdSetup(%v): %v", args, err)
	}
	return buf.String()
}

func TestSetupDetectClaudeOnly(t *testing.T) {
	home := setupHome(t)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	runSetup(t)

	if !fileExists(filepath.Join(home, ".claude.json")) {
		t.Fatal("claude configured: ~/.claude.json missing")
	}
	if dirExists(filepath.Join(home, ".codex")) || dirExists(filepath.Join(home, ".cursor")) {
		t.Fatal("undetected agents should not be created")
	}
}

func TestSetupApplyClaude(t *testing.T) {
	home := setupHome(t)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	runSetup(t)

	// MCP into ~/.claude.json
	claudeJSON := readJSON(t, filepath.Join(home, ".claude.json"))
	mcp, _ := claudeJSON["mcpServers"].(map[string]any)
	if mcp == nil {
		t.Fatalf("mcpServers missing: %v", claudeJSON)
	}
	wantMuster := map[string]any{
		"type": "stdio", "command": "muster",
		"args": []any{"mcp"}, "env": map[string]any{},
	}
	if got := mcp["muster"]; !reflect.DeepEqual(got, wantMuster) {
		t.Errorf("mcpServers.muster = %#v, want %#v", got, wantMuster)
	}
	if got, ok := mcp["muster-channel"]; ok {
		t.Errorf("mcpServers.muster-channel = %#v, want absent (the plugin delivers mail)", got)
	}

	// Hooks + permission into ~/.claude/settings.json
	settings := readJSON(t, filepath.Join(home, ".claude", "settings.json"))
	perms, _ := settings["permissions"].(map[string]any)
	if perms == nil || !reflect.DeepEqual(perms["allow"], []any{"mcp__muster"}) {
		t.Errorf("permissions.allow = %#v", settings["permissions"])
	}
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		t.Fatalf("hooks missing: %v", settings)
	}
	for _, ev := range []string{"SessionStart", "Stop", "SessionEnd"} {
		if _, ok := hooks[ev].([]any); !ok {
			t.Errorf("hooks.%s missing/not-array: %#v", ev, hooks[ev])
		}
	}
	// Command uses the ~/.local/bin/muster tilde form for the tilde-expanding
	// settings.json.
	ss := hooks["SessionStart"].([]any)[0].(map[string]any)
	inner := ss["hooks"].([]any)[0].(map[string]any)
	if cmd, _ := inner["command"].(string); !strings.HasPrefix(cmd, "~/.local/bin/muster hook SessionStart claude") {
		t.Errorf("SessionStart command = %q", cmd)
	}
}

// TestSetupClaudeInstallsUserPromptSubmit: Claude Code gets the hook that
// marks a session working, and a second run adds nothing.
func TestSetupClaudeInstallsUserPromptSubmit(t *testing.T) {
	home := setupHome(t)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	runSetup(t)
	path := filepath.Join(home, ".claude", "settings.json")
	hooks, _ := readJSON(t, path)["hooks"].(map[string]any)
	want := []any{map[string]any{"hooks": []any{map[string]any{
		"type": "command", "command": "~/.local/bin/muster hook UserPromptSubmit claude",
	}}}}
	if got := hooks["UserPromptSubmit"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("hooks.UserPromptSubmit = %#v, want %#v", got, want)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	runSetup(t)
	if again, _ := os.ReadFile(path); !bytes.Equal(again, first) {
		t.Fatalf("re-run changed settings.json:\n%s\nvs\n%s", first, again)
	}
}

func TestSetupIdempotent(t *testing.T) {
	home := setupHome(t)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".cursor"), 0o755); err != nil {
		t.Fatal(err)
	}
	runSetup(t)

	files := []string{
		filepath.Join(home, ".claude.json"),
		filepath.Join(home, ".claude", "settings.json"),
		filepath.Join(home, ".codex", "config.toml"),
		filepath.Join(home, ".codex", "hooks.json"),
		filepath.Join(home, ".cursor", "mcp.json"),
		filepath.Join(home, ".cursor", "hooks.json"),
	}
	first := map[string][]byte{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		first[f] = b
	}

	runSetup(t) // second run must not change anything

	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if !bytes.Equal(b, first[f]) {
			t.Errorf("%s changed on re-run:\n--- first ---\n%s\n--- second ---\n%s", f, first[f], b)
		}
	}
}

func TestSetupNoHooks(t *testing.T) {
	home := setupHome(t)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	runSetup(t, "--no-hooks")

	if !fileExists(filepath.Join(home, ".claude.json")) {
		t.Fatal("MCP should still be written with --no-hooks")
	}
	settings := readJSON(t, filepath.Join(home, ".claude", "settings.json"))
	if _, ok := settings["hooks"]; ok {
		t.Fatalf("settings.json hooks should NOT be written with --no-hooks: %#v", settings)
	}
}

func TestSetupCodexTOML(t *testing.T) {
	home := setupHome(t)
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	runSetup(t)

	path := filepath.Join(home, ".codex", "config.toml")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := toml.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	srv, _ := m["mcp_servers"].(map[string]any)
	muster, _ := srv["muster"].(map[string]any)
	if muster == nil || muster["command"] != "muster" {
		t.Fatalf("mcp_servers.muster = %#v", srv)
	}
	if !reflect.DeepEqual(muster["args"], []any{"mcp"}) {
		t.Errorf("args = %#v", muster["args"])
	}

	// Re-run idempotent.
	runSetup(t)
	b2, _ := os.ReadFile(path)
	if !bytes.Equal(b, b2) {
		t.Errorf("codex config.toml changed on re-run:\n%s\nvs\n%s", b, b2)
	}
}

func TestSetupPrintKempt(t *testing.T) {
	setupHome(t)
	out := runSetup(t, "--print", "kempt")
	if out != setupKemptTOML {
		t.Errorf("--print kempt output does not equal the embedded template.\ngot:\n%s", out)
	}
}

func TestSetupPrintUnknown(t *testing.T) {
	setupHome(t)
	var buf bytes.Buffer
	if err := cmdSetup([]string{"--print", "bogus"}, &buf); err == nil {
		t.Fatal("expected error for unknown --print value")
	}
}

func TestSetupKemptPresent(t *testing.T) {
	home := setupHome(t)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Put a fake `kempt` executable on PATH.
	bindir := t.TempDir()
	kempt := filepath.Join(bindir, "kempt")
	if err := os.WriteFile(kempt, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bindir)

	out := runSetup(t)
	if !strings.Contains(out, "kempt apply") {
		t.Errorf("expected kempt guidance, got:\n%s", out)
	}
	if fileExists(filepath.Join(home, ".claude.json")) {
		t.Fatal("no files should be written when kempt is present without --force")
	}

	// --force overrides.
	runSetup(t, "--force")
	if !fileExists(filepath.Join(home, ".claude.json")) {
		t.Fatal("--force should configure directly even with kempt present")
	}
}

func TestSetupTmux(t *testing.T) {
	home := setupHome(t)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	runSetup(t, "--tmux")

	path := filepath.Join(home, ".tmux.conf")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range tmuxLines() {
		if !strings.Contains(string(b), l) {
			t.Errorf("~/.tmux.conf missing line %q", l)
		}
	}

	runSetup(t, "--tmux") // re-run: no duplicates
	b2, _ := os.ReadFile(path)
	for _, l := range tmuxLines() {
		if n := strings.Count(string(b2), l); n != 1 {
			t.Errorf("line %q appears %d times, want 1", l, n)
		}
	}
}

func TestSetupNoTmuxByDefault(t *testing.T) {
	home := setupHome(t)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	runSetup(t)
	if fileExists(filepath.Join(home, ".tmux.conf")) {
		t.Fatal("tmux config should not be touched without --tmux")
	}
}

func TestSetupPreserveExisting(t *testing.T) {
	home := setupHome(t)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	seed := map[string]any{
		"topLevelKey": "keepme",
		"mcpServers": map[string]any{
			"other": map[string]any{"command": "other-tool"},
		},
	}
	b, _ := json.MarshalIndent(seed, "", "  ")
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}

	runSetup(t)

	got := readJSON(t, filepath.Join(home, ".claude.json"))
	if got["topLevelKey"] != "keepme" {
		t.Errorf("top-level key not preserved: %#v", got)
	}
	mcp := got["mcpServers"].(map[string]any)
	if _, ok := mcp["other"]; !ok {
		t.Errorf("pre-existing mcpServers.other dropped: %#v", mcp)
	}
	if _, ok := mcp["muster"]; !ok {
		t.Errorf("mcpServers.muster not added: %#v", mcp)
	}
}

func TestSetupNoAgents(t *testing.T) {
	setupHome(t)
	out := runSetup(t)
	if !strings.Contains(out, "No supported agents found") {
		t.Errorf("expected no-agents message, got:\n%s", out)
	}
}

func TestSetupDryRun(t *testing.T) {
	home := setupHome(t)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	out := runSetup(t, "--dry-run")
	if !strings.Contains(out, "would update") {
		t.Errorf("expected dry-run output, got:\n%s", out)
	}
	if fileExists(filepath.Join(home, ".claude.json")) {
		t.Fatal("--dry-run must not write files")
	}
}

func TestSetupAgentFilter(t *testing.T) {
	home := setupHome(t)
	for _, d := range []string{".claude", ".codex", ".cursor"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	runSetup(t, "--agent", "codex")
	if fileExists(filepath.Join(home, ".claude.json")) {
		t.Error("claude configured despite --agent codex")
	}
	if !fileExists(filepath.Join(home, ".codex", "config.toml")) {
		t.Error("codex not configured with --agent codex")
	}
}

// TestKemptTemplateInstallsUserPromptSubmit: the kempt template is the
// canonical setup, so it carries the Claude hook that marks a session working
// exactly as `muster setup` does.
func TestKemptTemplateInstallsUserPromptSubmit(t *testing.T) {
	want := `UserPromptSubmit = [ { hooks = [ { type = "command", command = "~/.local/bin/muster hook UserPromptSubmit claude" } ] } ]`
	if !strings.Contains(setupKemptTOML, want) {
		t.Fatalf("setup_kempt.toml lacks the Claude UserPromptSubmit hook: want %s", want)
	}
}

// TestSetupClaudeEnablesPlugin: Claude Code hears mail through the muster
// plugin, so setup adds the subaud-tools marketplace with auto-update on and
// enables muster@subaud-tools, drops the old muster marketplace and
// muster@muster, drops the old muster-channel server and keeps muster mcp for
// the bus tools.
func TestSetupClaudeEnablesPlugin(t *testing.T) {
	home := setupHome(t)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	seed := `{"mcpServers":{"muster-channel":{"type":"stdio","command":"muster","args":["channel"],"env":{}}}}`
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	old := `{"extraKnownMarketplaces":{"muster":{"source":{"source":"github","repo":"subaud-tools/muster"},"autoUpdate":true}},"enabledPlugins":{"muster@muster":true}}`
	if err := os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	runSetup(t)

	mcp, _ := readJSON(t, filepath.Join(home, ".claude.json"))["mcpServers"].(map[string]any)
	if got, ok := mcp["muster-channel"]; ok {
		t.Errorf("mcpServers.muster-channel = %#v, want removed", got)
	}
	if _, ok := mcp["muster"]; !ok {
		t.Errorf("mcpServers.muster missing: %#v", mcp)
	}

	settings := readJSON(t, filepath.Join(home, ".claude", "settings.json"))
	wantMarket := map[string]any{"source": map[string]any{"source": "github", "repo": "subaud-tools/plugins"}, "autoUpdate": true}
	markets, _ := settings["extraKnownMarketplaces"].(map[string]any)
	if got := markets["subaud-tools"]; !reflect.DeepEqual(got, wantMarket) {
		t.Errorf("extraKnownMarketplaces.subaud-tools = %#v, want %#v", got, wantMarket)
	}
	if got, ok := markets["muster"]; ok {
		t.Errorf("extraKnownMarketplaces.muster = %#v, want removed", got)
	}
	plugins, _ := settings["enabledPlugins"].(map[string]any)
	if got := plugins["muster@subaud-tools"]; got != true {
		t.Errorf("enabledPlugins[muster@subaud-tools] = %#v, want true", got)
	}
	if got, ok := plugins["muster@muster"]; ok {
		t.Errorf("enabledPlugins[muster@muster] = %#v, want removed", got)
	}
}

// TestSetupMatchesKemptTemplate: `muster setup` applies exactly the merges
// and removals the kempt block declares, file by file, so the two install
// paths can't drift.
func TestSetupMatchesKemptTemplate(t *testing.T) {
	var doc struct {
		Packages struct {
			Muster struct {
				JSONMerge []struct {
					File   string         `toml:"file"`
					Merge  map[string]any `toml:"merge"`
					Remove []string       `toml:"remove"`
				} `toml:"json-merge"`
				TOMLMerge []struct {
					File  string         `toml:"file"`
					Merge map[string]any `toml:"merge"`
				} `toml:"toml-merge"`
			} `toml:"muster"`
		} `toml:"packages"`
	}
	if err := toml.Unmarshal([]byte(setupKemptTOML), &doc); err != nil {
		t.Fatalf("parse setup_kempt.toml: %v", err)
	}
	entry := func(file string, merge map[string]any, remove []string) string {
		b, err := json.Marshal(map[string]any{"file": file, "merge": merge, "remove": remove})
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	var kempt []string
	for _, e := range doc.Packages.Muster.JSONMerge {
		kempt = append(kempt, entry(e.File, e.Merge, e.Remove))
	}
	for _, e := range doc.Packages.Muster.TOMLMerge {
		kempt = append(kempt, entry(e.File, e.Merge, nil))
	}

	var setup []string
	for _, agent := range supportedAgents {
		// kempt expands ${HOME} where setup writes the absolute binary path.
		for _, op := range agentOps("/home", agent, "${HOME}/.local/bin/muster", false) {
			setup = append(setup, entry(op.disp, op.desired, op.remove))
		}
	}

	sort.Strings(kempt)
	sort.Strings(setup)
	if !reflect.DeepEqual(kempt, setup) {
		t.Errorf("setup and setup_kempt.toml differ:\nkempt:\n%s\nsetup:\n%s", strings.Join(kempt, "\n"), strings.Join(setup, "\n"))
	}
}

func TestSetupConsumesCanonicalSource(t *testing.T) {
	original := setupKemptTOML
	t.Cleanup(func() { setupKemptTOML = original })
	setupKemptTOML = strings.ReplaceAll(original, `repo = "subaud-tools/plugins"`, `repo = "fixture/plugins"`)
	for _, op := range agentOps(t.TempDir(), "claude", "/fixture/muster", false) {
		if marketplace, ok := op.desired["extraKnownMarketplaces"]; ok {
			b, _ := json.Marshal(marketplace)
			if !strings.Contains(string(b), "fixture/plugins") {
				t.Fatalf("direct setup ignored canonical source: %s", b)
			}
			return
		}
	}
	t.Fatal("missing marketplace operation")
}

func TestSetupReconcilesLegacyNestedHooks(t *testing.T) {
	home := setupHome(t)
	path := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	fixture := `{"private":{"account":"keep"},"hooks":{"Stop":[{"extra":"keep","hooks":[{"type":"command","command":"muster hook Stop claude"},{"type":"command","command":"echo sibling"}]},{"hooks":[{"type":"command","command":"~/.local/bin/muster hook Stop claude"}]},{"matcher":"deliberate","hooks":[{"type":"command","command":"muster hook Stop claude"}]}]}}`
	if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := cmdSetup([]string{"--force", "--agent", "claude"}, &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
	}
	doc := readJSON(t, path)
	groups := doc["hooks"].(map[string]any)["Stop"].([]any)
	if len(groups) != 3 {
		t.Fatalf("groups = %#v", groups)
	}
	first := groups[0].(map[string]any)
	if len(first["hooks"].([]any)) != 1 || first["extra"] != "keep" {
		t.Fatalf("legacy nested hook survived or sibling lost: %#v", first)
	}
	if doc["private"].(map[string]any)["account"] != "keep" {
		t.Fatal("private config lost")
	}
}

func TestSetupBundleCommand(t *testing.T) {
	path := filepath.Join(t.TempDir(), "muster_setup.tar.gz")
	var out bytes.Buffer
	if err := Dispatch([]string{"setup-bundle", "--version", "1.2.3", "--out", path}, &out); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := setupbundle.Read(data)
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Tool != "muster" || bundle.Version != "1.2.3" || len(bundle.Files) != 0 {
		t.Fatalf("unexpected bundle: %#v", bundle)
	}
	var doc map[string]any
	if err := toml.Unmarshal(bundle.Manifest, &doc); err != nil {
		t.Fatal(err)
	}
	packages := doc["packages"].(map[string]any)
	pkg, ok := packages["integration"].(map[string]any)
	if !ok || len(packages) != 1 {
		t.Fatal("bundle must declare only packages.integration for kempt binding")
	}
	if doc["kempt"].(map[string]any)["spec"] != int64(2) || pkg["download"] != nil || strings.Contains(string(bundle.Manifest), "~/.cursor/") {
		t.Fatal("bundle includes own download/Cursor or lacks schema 2")
	}
	if pkg["service"] == nil || pkg["line-in-file"] == nil || pkg["verify"] == nil || !strings.Contains(string(bundle.Manifest), "subaud-tools/plugins") {
		t.Fatal("bundle lost shared setup")
	}
}

func TestContribHooksFromCanonicalSource(t *testing.T) {
	for _, agent := range supportedAgents {
		t.Run(agent, func(t *testing.T) {
			var out bytes.Buffer
			if err := cmdSetup([]string{"--print", agent + "-hooks"}, &out); err != nil {
				t.Fatal(err)
			}
			name := agent + "-hooks.json"
			if agent == "claude" {
				name = "claude-settings-hooks.json"
			}
			data, err := os.ReadFile(filepath.Join("..", "..", "contrib", name))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(mustJSON(string(data)), mustJSON(out.String())) {
				t.Fatalf("regenerate contrib/%s from setup --print %s-hooks", name, agent)
			}
		})
	}
}

// Opt-in integration with the public kempt release, never the service-bearing
// bundle: only the canonical JSON/TOML configuration steps enter this fixture.
func TestSetupMixedKempt(t *testing.T) {
	bin := os.Getenv("KEMPT_TEST_BIN")
	if bin == "" {
		t.Skip("set KEMPT_TEST_BIN to released kempt >= 0.13.0")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local/state"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	path := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	contrib, err := os.ReadFile("../../contrib/claude-settings-hooks.json")
	if err != nil {
		t.Fatal(err)
	}
	legacy := strings.ReplaceAll(string(contrib), "~/.local/bin/muster", "muster")
	seed := mustJSON(legacy)
	deepMerge(seed, mustJSON(string(contrib)))
	deepMerge(seed, mustJSON(`{"private":{"account":"keep"},"hooks":{"Stop":[{"extra":"keep","hooks":[{"type":"command","command":"echo sibling"}]},{"matcher":"deliberate","hooks":[{"type":"command","command":"muster hook Stop claude"}]}]}}`))
	data, _ := json.Marshal(seed)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	pkg := setupPackage(setupDeclaration())
	fixture := map[string]any{"kempt": map[string]any{"spec": 2}, "packages": map[string]any{"muster": map[string]any{"json-merge": pkg["json-merge"], "toml-merge": pkg["toml-merge"]}}}
	manifest, err := toml.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(home, "config-only.toml")
	if err := os.WriteFile(manifestPath, manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		output, err := exec.Command(bin, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("kempt %v: %v\n%s", args, err, output)
		}
		return string(output)
	}
	before := run("doctor", "--json", "--manifest", manifestPath)
	if !strings.Contains(before, "duplicate owned hook registrations") {
		t.Fatalf("doctor missed duplicate: %s", before)
	}
	stub := filepath.Join(home, ".local/bin/muster")
	if err := os.MkdirAll(filepath.Dir(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$HOME/invocations\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	var previous []byte
	for i := 0; i < 3; i++ {
		run("apply", "--yes", "--manifest", manifestPath)
		if err := cmdSetup([]string{"--force", "--agent", "claude,codex,cursor"}, &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
		current, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if i > 0 && !bytes.Equal(previous, current) {
			t.Fatal("mixed repeat drifted")
		}
		previous = current
	}
	after := run("doctor", "--json", "--manifest", manifestPath)
	if strings.Contains(after, "duplicate owned hook registrations") {
		t.Fatalf("duplicate remains: %s", after)
	}
	doc := readJSON(t, path)
	rule := hookconfig.Removal{Event: "Stop", Commands: []string{"muster hook Stop claude", "~/.local/bin/muster hook Stop claude"}}
	if hookconfig.Count(doc, rule) != 1 {
		t.Fatal("expected one owned hook")
	}
	if doc["private"].(map[string]any)["account"] != "keep" {
		t.Fatal("private config lost")
	}
	// Deliver a Stop payload to only its no-matcher command hooks, as a harness
	// does. The intended muster executable is a harmless stub, never the bus.
	for _, value := range doc["hooks"].(map[string]any)["Stop"].([]any) {
		group := value.(map[string]any)
		if matcher, _ := group["matcher"].(string); matcher != "" {
			continue
		}
		for _, value := range group["hooks"].([]any) {
			hook := value.(map[string]any)
			cmd := exec.Command("/bin/sh", "-c", hook["command"].(string))
			cmd.Stdin = strings.NewReader(`{"hook_event_name":"Stop"}`)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("event: %v %s", err, output)
			}
		}
	}
	calls, err := os.ReadFile(filepath.Join(home, "invocations"))
	if err != nil || string(calls) != "hook Stop claude\n" {
		t.Fatalf("invocations %q: %v", calls, err)
	}
}
