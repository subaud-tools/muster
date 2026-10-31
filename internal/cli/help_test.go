package cli

import (
	"strings"
	"testing"
)

// `muster help` lists every command with its one-line summary under muster's
// four groups, in order, below muster's overview (tools.Config.About).
func TestHelpGroupedSnapshot(t *testing.T) {
	out, _, code := musterCLI(t, "help")
	if code != 0 {
		t.Fatalf("help exited %d", code)
	}
	last := -1
	for _, h := range []string{"\nTalk\n", "\nWatch\n", "\nIdentity\n", "\nPlumbing\n"} {
		i := strings.Index(out, h)
		if i < 0 || i < last {
			t.Fatalf("heading %q missing or out of order:\n%s", strings.TrimSpace(h), out)
		}
		last = i
	}
	for _, c := range Registry {
		if !strings.Contains(out, "  "+c.Name+" ") || !strings.Contains(out, c.Summary) {
			t.Errorf("help does not list %q with its summary", c.Name)
		}
	}
	for _, want := range []string{"muster help <command>", "https://muster.tools", "three modes"} {
		if !strings.Contains(out, want) {
			t.Errorf("help lacks %q", want)
		}
	}
}

// -h and --help at the top level are `help`.
func TestTopLevelHelpFlags(t *testing.T) {
	want, _, _ := musterCLI(t, "help")
	for _, arg := range []string{"-h", "--help"} {
		if got, _, code := musterCLI(t, arg); code != 0 || got != want {
			t.Fatalf("%s differs from help (exit %d)", arg, code)
		}
	}
}

func TestHelpForUnknownCommand(t *testing.T) {
	_, errw, code := musterCLI(t, "help", "nope")
	if code != 2 || !strings.Contains(errw, `unknown command "nope"`) {
		t.Fatalf("help nope: exit %d, stderr %q", code, errw)
	}
}

// The man page keeps what muster's own renderer carried: the modes and the
// files, now in the DESCRIPTION, and a quoted synopsis stays whole.
func TestManPage(t *testing.T) {
	out, _, code := musterCLI(t, "man")
	if code != 0 || !strings.HasPrefix(out, ".TH MUSTER 1") {
		t.Fatalf("man: exit %d, %.80q", code, out)
	}
	for _, want := range []string{".SH DESCRIPTION", "three modes", "MUSTER_HOME", ".SH COMMANDS", `\(dqbody\(dq`} {
		if !strings.Contains(out, want) {
			t.Errorf("man lacks %q", want)
		}
	}
	for _, c := range Registry {
		if !strings.Contains(out, ".B "+strings.ReplaceAll(c.Name, "-", `\-`)) {
			t.Errorf("man has no entry for %q", c.Name)
		}
	}
}
