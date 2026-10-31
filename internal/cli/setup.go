package cli

import (
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
	"github.com/subaud-tools/tools-common/hookconfig"
	"github.com/subaud-tools/tools-common/setupbundle"
)

// setupKemptTOML is the canonical declarative setup — the exact kempt package
// block `muster setup --print kempt` emits and the imperative equivalent
// `muster setup` applies for people who don't use kempt, so the two paths
// never drift. Co-located and embedded the same way internal/store embeds
// schema.sql.
//
//go:embed setup_kempt.toml
var setupKemptTOML string

// setupFlagVals holds cmdSetup's parsed flag pointers.
type setupFlagVals struct {
	print   *string
	force   *bool
	dryRun  *bool
	noHooks *bool
	tmux    *bool
	agents  *stringList
}

// stringList is a repeatable / comma-separated string flag (--agent claude
// --agent codex, or --agent claude,codex).
type stringList []string

func (s *stringList) String() string {
	if s == nil {
		return ""
	}
	return strings.Join(*s, ",")
}

func (s *stringList) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			*s = append(*s, part)
		}
	}
	return nil
}

// newSetupFlagsWithVals declares setup's flags and returns both the FlagSet
// and typed access to its values — the one declaration cmdSetup (real
// parsing) and newSetupFlags (registry help/man rendering) both build on, so
// the two can't drift apart.
func newSetupFlagsWithVals() (*flag.FlagSet, setupFlagVals) {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var v setupFlagVals
	v.print = fs.String("print", "", "print a configuration instead of applying it (kempt|claude-hooks|codex-hooks|cursor-hooks)")
	v.force = fs.Bool("force", false, "apply directly even when kempt is installed")
	v.dryRun = fs.Bool("dry-run", false, "compute and print every change but write nothing")
	v.noHooks = fs.Bool("no-hooks", false, "register the MCP server (and the Claude Code plugin) only; skip session hooks")
	v.tmux = fs.Bool("tmux", false, "also add the tmux mailbox lines to ~/.tmux.conf")
	v.agents = &stringList{}
	fs.Var(v.agents, "agent", "limit to these agents (claude|codex|cursor); repeatable or comma-separated")
	return fs, v
}

// newSetupFlags builds setup's flag.FlagSet for registry-driven help/man
// rendering (Command.NewFlags).
func newSetupFlags() *flag.FlagSet {
	fs, _ := newSetupFlagsWithVals()
	return fs
}

// supportedAgents is setup's v1 scope, in display order.
var supportedAgents = []string{"claude", "codex", "cursor"}

// cmdSetup configures the user's coding agents to use muster: it registers
// the MCP server with each detected agent, installs the session hooks, and
// (opt-in) renders the tmux mailbox — the imperative fallback for people not
// using kempt, applying the exact same configuration the embedded kempt
// package declares.
func cmdSetup(args []string, out io.Writer) error {
	if helpRequested(args) {
		return HelpFor("setup", out)
	}
	fs, v := newSetupFlagsWithVals()
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return HelpFor("setup", out)
		}
		return err
	}

	// --print kempt: emit the canonical template, touch nothing.
	if *v.print != "" {
		if agent, ok := strings.CutSuffix(*v.print, "-hooks"); ok && contains(supportedAgents, agent) {
			desired := map[string]any{}
			for _, op := range agentOps("", agent, "${HOME}/.local/bin/muster", false) {
				if op.desired["hooks"] != nil {
					deepMerge(desired, op.desired)
				}
			}
			enc := json.NewEncoder(out)
			enc.SetIndent("", "  ")
			return enc.Encode(desired)
		}
		if *v.print != "kempt" {
			return fmt.Errorf("unknown --print value %q (kempt|claude-hooks|codex-hooks|cursor-hooks)", *v.print)
		}
		_, err := fmt.Fprint(out, setupKemptTOML)
		return err
	}

	// Validate any explicitly requested agents up front.
	for _, a := range *v.agents {
		if !contains(supportedAgents, a) {
			return fmt.Errorf("unknown --agent %q (supported: claude, codex, cursor)", a)
		}
	}

	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		if h := os.Getenv("HOME"); h != "" {
			home = h
		} else {
			return fmt.Errorf("cannot determine home directory: %w", err)
		}
	}

	// kempt manages machine config declaratively; when it's installed we
	// don't fight it — we hand back the exact block to add, unless --force.
	if !*v.force {
		if _, err := exec.LookPath("kempt"); err == nil {
			_, _ = fmt.Fprintln(out, "kempt is installed — it manages machine config declaratively, so muster setup will not edit files directly.")
			_, _ = fmt.Fprintln(out)
			_, _ = fmt.Fprint(out, setupKemptTOML)
			_, _ = fmt.Fprintln(out)
			_, err := fmt.Fprintln(out, "Add this to your kempt.toml and run `kempt apply`, or re-run with `muster setup --force` to configure directly.")
			return err
		}
	}

	return applySetup(out, home, v)
}

// applySetup performs (or, under --dry-run, describes) the direct-merge
// configuration for every detected, requested agent.
func applySetup(out io.Writer, home string, v setupFlagVals) error {
	// The muster binary path, resolved once: prefer an installed
	// ~/.local/bin/muster, else the running executable's absolute path. Files
	// that expand ~ get the literal ~/.local/bin/muster; files that do NOT
	// expand ~ (codex/cursor hooks.json) get the absolute path.
	mbAbs := resolveMusterBin(home)

	requested := *v.agents
	var ops []fileOp
	var configured []string

	for _, agent := range supportedAgents {
		if len(requested) > 0 && !contains(requested, agent) {
			continue
		}
		if !agentDetected(home, agent) {
			continue
		}
		configured = append(configured, agent)
		ops = append(ops, agentOps(home, agent, mbAbs, *v.noHooks)...)
	}

	if len(configured) == 0 {
		_, err := fmt.Fprintln(out, "No supported agents found; install Claude Code, Codex, or Cursor first.")
		return err
	}

	for _, op := range ops {
		changed, err := op.apply(*v.dryRun)
		if err != nil {
			return fmt.Errorf("%s: %w", op.disp, err)
		}
		reportOp(out, op, changed, *v.dryRun)
	}

	if *v.tmux {
		if err := applyTmux(out, home, *v.dryRun); err != nil {
			return err
		}
	}

	return nil
}

// resolveMusterBin returns the absolute muster binary path: the installed
// ~/.local/bin/muster if it exists, else the running executable.
func resolveMusterBin(home string) string {
	local := filepath.Join(home, ".local", "bin", "muster")
	if _, err := os.Stat(local); err == nil {
		return local
	}
	if exe, err := os.Executable(); err == nil {
		if abs, err := filepath.Abs(exe); err == nil {
			return abs
		}
		return exe
	}
	return local
}

// agentDetected reports whether the given agent looks installed on this
// machine, by the presence of its config directory (or, for Claude, its
// top-level config file).
func agentDetected(home, agent string) bool {
	switch agent {
	case "claude":
		return dirExists(filepath.Join(home, ".claude")) || fileExists(filepath.Join(home, ".claude.json"))
	case "codex":
		return dirExists(filepath.Join(home, ".codex"))
	case "cursor":
		return dirExists(filepath.Join(home, ".cursor"))
	}
	return false
}

// setupDeclaration decodes only the configuration operations direct setup supports.
// Download, service and verification remain exclusively kempt's responsibility.
func setupDeclaration() map[string]any {
	var doc map[string]any
	if err := toml.Unmarshal([]byte(setupKemptTOML), &doc); err != nil {
		panic(err)
	}
	return doc
}

func setupPackage(doc map[string]any) map[string]any {
	return doc["packages"].(map[string]any)["muster"].(map[string]any)
}

type declaredMerge struct {
	File        string               `json:"file"`
	Merge       map[string]any       `json:"merge"`
	Remove      []string             `json:"remove"`
	RemoveHooks []hookconfig.Removal `json:"remove-hooks"`
}

// agentOps consumes the declaration, not an imperative copy of its values.
func agentOps(home, agent, mbAbs string, noHooks bool) []fileOp {
	pkg := setupPackage(setupDeclaration())
	var ops []fileOp
	for _, kind := range []string{"json-merge", "toml-merge"} {
		data, err := json.Marshal(pkg[kind])
		if err != nil {
			panic(err)
		}
		var entries []declaredMerge
		if err := json.Unmarshal(data, &entries); err != nil {
			panic(err)
		}
		for _, entry := range entries {
			if !strings.HasPrefix(entry.File, "~/."+agent+"/") && (agent != "claude" || entry.File != "~/.claude.json") {
				continue
			}
			if noHooks && (entry.Merge["hooks"] != nil || entry.Merge["permissions"] != nil) {
				continue
			}
			// Codex/Cursor require an absolute executable. Keep the installed-path
			// fallback of direct setup; Claude deliberately retains its literal ~.
			encoded, err := json.Marshal(entry.Merge)
			if err != nil {
				panic(err)
			}
			replacement, err := json.Marshal(mbAbs)
			if err != nil {
				panic(err)
			}
			desired := mustJSON(strings.ReplaceAll(string(encoded), "${HOME}/.local/bin/muster", string(replacement[1:len(replacement)-1])))
			ops = append(ops, fileOp{path: filepath.Join(home, strings.TrimPrefix(entry.File, "~/")), disp: entry.File, desc: "declared " + kind, toml: kind == "toml-merge", desired: desired, remove: entry.Remove, removeHooks: entry.RemoveHooks})
		}
	}
	return ops
}

// setupSource contributes shared onboarding only: its binary is resolved by the
// owning download, and standalone Cursor support is intentionally not selected.
func setupSource() setupbundle.Source {
	doc := setupDeclaration()
	pkg := setupPackage(doc)
	delete(pkg, "download")
	entries, _ := pkg["json-merge"].([]any)
	var shared []any
	for _, value := range entries {
		entry := value.(map[string]any)
		if !strings.HasPrefix(entry["file"].(string), "~/.cursor/") {
			shared = append(shared, entry)
		}
	}
	pkg["json-merge"] = shared
	doc["packages"] = map[string]any{"integration": pkg}
	data, err := toml.Marshal(doc)
	if err != nil {
		panic(err)
	}
	return setupbundle.Source{Manifest: data}
}

// fileOp is one deep-merge into one config file.
type fileOp struct {
	path        string         // absolute filesystem path
	disp        string         // display path (~ form) for summaries
	desc        string         // what this merge adds, for the summary line
	toml        bool           // true → TOML file, else JSON
	desired     map[string]any // the map to deep-merge in
	removeHooks []hookconfig.Removal
	remove      []string // dotted key paths to delete after the merge
}

// apply reads the target file, deep-merges the desired map, and (unless
// dryRun) writes it back. It reports whether the merge changed the file's
// serialized content — the idempotency signal the summary keys off.
func (op fileOp) apply(dryRun bool) (bool, error) {
	before, err := os.ReadFile(op.path)
	if err != nil {
		if !os.IsNotExist(err) {
			return false, err
		}
		before = nil
	}

	current := map[string]any{}
	if len(strings.TrimSpace(string(before))) > 0 {
		if op.toml {
			if err := toml.Unmarshal(before, &current); err != nil {
				return false, fmt.Errorf("parse: %w", err)
			}
		} else {
			if err := json.Unmarshal(before, &current); err != nil {
				return false, fmt.Errorf("parse: %w", err)
			}
		}
	}

	hookconfig.Remove(current, op.removeHooks)
	deepMerge(current, op.desired)
	for _, p := range op.remove {
		removePath(current, strings.Split(p, "."))
	}

	var after []byte
	if op.toml {
		after, err = toml.Marshal(current)
	} else {
		after, err = json.MarshalIndent(current, "", "  ")
		if err == nil {
			after = append(after, '\n')
		}
	}
	if err != nil {
		return false, fmt.Errorf("encode: %w", err)
	}

	if string(after) == string(before) {
		return false, nil
	}
	if dryRun {
		return true, nil
	}
	if err := os.MkdirAll(filepath.Dir(op.path), 0o755); err != nil {
		return false, err
	}
	if err := os.WriteFile(op.path, after, 0o644); err != nil {
		return false, err
	}
	return true, nil
}

// reportOp prints one summary line for a completed (or dry-run) file op.
func reportOp(out io.Writer, op fileOp, changed, dryRun bool) {
	switch {
	case changed && dryRun:
		_, _ = fmt.Fprintf(out, "would update %s: add %s\n", op.disp, op.desc)
	case changed:
		_, _ = fmt.Fprintf(out, "updated %s: %s\n", op.disp, op.desc)
	default:
		_, _ = fmt.Fprintf(out, "no change %s (%s already present)\n", op.disp, op.desc)
	}
}

// tmuxLines reads the optional mailbox lines from the same declaration.
func tmuxLines() []string {
	entries, _ := setupPackage(setupDeclaration())["line-in-file"].([]any)
	var lines []string
	for _, value := range entries {
		entry := value.(map[string]any)
		if entry["file"] == "~/.tmux.conf" {
			lines = append(lines, entry["line"].(string))
		}
	}
	return lines
}

// applyTmux ensures each mailbox line exists in ~/.tmux.conf (exact-line
// match, so re-runs add no duplicates).
func applyTmux(out io.Writer, home string, dryRun bool) error {
	path := filepath.Join(home, ".tmux.conf")
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	existing := map[string]bool{}
	for _, l := range strings.Split(string(data), "\n") {
		existing[l] = true
	}

	var missing []string
	for _, l := range tmuxLines() {
		if !existing[l] {
			missing = append(missing, l)
		}
	}

	if len(missing) == 0 {
		_, _ = fmt.Fprintln(out, "no change ~/.tmux.conf (mailbox lines already present)")
		return nil
	}
	if dryRun {
		_, _ = fmt.Fprintf(out, "would update ~/.tmux.conf: add %d mailbox line(s)\n", len(missing))
		return nil
	}

	buf := string(data)
	if len(buf) > 0 && !strings.HasSuffix(buf, "\n") {
		buf += "\n"
	}
	buf += strings.Join(missing, "\n") + "\n"
	if err := os.WriteFile(path, []byte(buf), 0o644); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "updated ~/.tmux.conf: added %d mailbox line(s)\n", len(missing))
	return nil
}

// deepMerge merges src into dst in place: nested maps merge recursively;
// arrays union by deep equality (a desired element is appended only when no
// existing element is reflect.DeepEqual to it, so re-runs add no duplicates);
// scalars overwrite. Every pre-existing key not named in src is preserved.
func deepMerge(dst, src map[string]any) {
	for k, sv := range src {
		switch sval := sv.(type) {
		case map[string]any:
			if dval, ok := dst[k].(map[string]any); ok {
				deepMerge(dval, sval)
			} else {
				nm := map[string]any{}
				deepMerge(nm, sval)
				dst[k] = nm
			}
		case []any:
			dst[k] = mergeArray(asArray(dst[k]), sval)
		default:
			dst[k] = sv
		}
	}
}

// removePath deletes the key at a dotted path, leaving its parents; a path
// that isn't there is left alone.
func removePath(m map[string]any, keys []string) {
	for len(keys) > 1 {
		next, ok := m[keys[0]].(map[string]any)
		if !ok {
			return
		}
		m, keys = next, keys[1:]
	}
	delete(m, keys[0])
}

// mergeArray returns dst with each element of src appended only if no element
// already deep-equals it.
func mergeArray(dst, src []any) []any {
	for _, s := range src {
		dup := false
		for _, d := range dst {
			if reflect.DeepEqual(d, s) {
				dup = true
				break
			}
		}
		if !dup {
			dst = append(dst, s)
		}
	}
	return dst
}

// asArray coerces an existing value to []any (nil for anything that isn't
// already an array — a scalar under a key we merge an array into is replaced,
// matching "scalars: desired overwrites").
func asArray(v any) []any {
	if a, ok := v.([]any); ok {
		return a
	}
	return nil
}

// mustJSON parses a compile-time-constant JSON literal into a map[string]any.
// Building the desired maps by unmarshaling JSON (rather than as Go literals)
// normalizes their types to exactly what re-reading a written file produces
// (numbers as float64, etc.), so the array union-by-deep-equality stays exact
// across runs and never appends a "different-typed" duplicate.
func mustJSON(s string) map[string]any {
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		panic(fmt.Sprintf("setup: bad desired JSON literal: %v", err))
	}
	return m
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}
