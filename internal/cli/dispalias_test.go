package cli

import (
	"testing"
	"time"

	"github.com/subaud-tools/muster/internal/mustertest"
)

// TestAliasDisplayStripsTheLocalPrefix is the everyday case: this machine's
// own rows read short.
func TestAliasDisplayStripsTheLocalPrefix(t *testing.T) {
	t.Setenv("MUSTER_HOME", t.TempDir())
	t.Setenv("MUSTER_DEVICE_NAME", "personal")
	d := aliasDisplay([]string{"personal-dotfiles/main", "work-dotfiles/main"})
	if got, want := d["personal-dotfiles/main"], "dotfiles/main"; got != want {
		t.Fatalf("local alias rendered %q, want %q", got, want)
	}
	if got, want := d["work-dotfiles/main"], "work-dotfiles/main"; got != want {
		t.Fatalf("foreign alias rendered %q, want %q", got, want)
	}
}

// TestAliasDisplayKeepsCollidingRowsInFull covers both the migration case (a
// legacy bare row beside a seeded one) and the permanent one (a foreign bare
// alias matching one of ours). Two rows must never render the same string.
func TestAliasDisplayKeepsCollidingRowsInFull(t *testing.T) {
	t.Setenv("MUSTER_HOME", t.TempDir())
	t.Setenv("MUSTER_DEVICE_NAME", "personal")
	d := aliasDisplay([]string{"personal-dotfiles/main", "dotfiles/main"})
	if got, want := d["personal-dotfiles/main"], "personal-dotfiles/main"; got != want {
		t.Fatalf("colliding local alias rendered %q, want %q", got, want)
	}
	if got, want := d["dotfiles/main"], "dotfiles/main"; got != want {
		t.Fatalf("colliding bare alias rendered %q, want %q", got, want)
	}
}

// TestAliasDisplayDuplicateAliasStillStrips guards against counting
// OCCURRENCES instead of DISTINCT aliases: the same alias legitimately
// appears twice in one view (a thread's FROM and LAST-FROM commonly match),
// and that must not be treated as a collision with itself.
func TestAliasDisplayDuplicateAliasStillStrips(t *testing.T) {
	t.Setenv("MUSTER_HOME", t.TempDir())
	t.Setenv("MUSTER_DEVICE_NAME", "personal")
	d := aliasDisplay([]string{"personal-dotfiles/main", "personal-dotfiles/main"})
	if got, want := d["personal-dotfiles/main"], "dotfiles/main"; got != want {
		t.Fatalf("duplicate (non-colliding) alias rendered %q, want %q", got, want)
	}
}

// TestDispAliasStripsWithoutAView covers the single-alias confirmations
// ("registered X"), where no other row is on screen to collide with.
func TestDispAliasStripsWithoutAView(t *testing.T) {
	t.Setenv("MUSTER_HOME", t.TempDir())
	t.Setenv("MUSTER_DEVICE_NAME", "personal")
	if got, want := dispAlias("personal-dotfiles/main"), "dotfiles/main"; got != want {
		t.Fatalf("dispAlias = %q, want %q", got, want)
	}
	if got, want := dispAlias("work-dotfiles/main"), "work-dotfiles/main"; got != want {
		t.Fatalf("dispAlias of a foreign alias = %q, want %q", got, want)
	}
}

// TestExpandAliasPrefersTheLocalRow is the invariant the design exists for: on
// this machine, a short name is mine. Exact-first would have made that
// conditional on what some other machine happens to have registered — and when
// one does, you would silently get theirs.
func TestExpandAliasPrefersTheLocalRow(t *testing.T) {
	t.Setenv("MUSTER_HOME", t.TempDir())
	t.Setenv("MUSTER_DEVICE_NAME", "personal")
	roster := map[string]bool{"personal-dotfiles/main": true, "dotfiles/main": true}
	exists := func(a string) bool { return roster[a] }
	if got, want := expandAlias("dotfiles/main", exists), "personal-dotfiles/main"; got != want {
		t.Fatalf("expandAlias = %q, want %q", got, want)
	}
}

// TestExpandAliasFallsBackToTheLiteral: a full foreign alias, and a bare alias
// with no local counterpart, both still resolve exactly.
func TestExpandAliasFallsBackToTheLiteral(t *testing.T) {
	t.Setenv("MUSTER_HOME", t.TempDir())
	t.Setenv("MUSTER_DEVICE_NAME", "personal")
	roster := map[string]bool{"work-dotfiles/main": true, "legacy": true}
	exists := func(a string) bool { return roster[a] }
	if got, want := expandAlias("work-dotfiles/main", exists), "work-dotfiles/main"; got != want {
		t.Fatalf("expandAlias = %q, want %q", got, want)
	}
	if got, want := expandAlias("legacy", exists), "legacy"; got != want {
		t.Fatalf("expandAlias = %q, want %q", got, want)
	}
}

// TestExpandAliasLeavesUnknownNamesAlone so the caller's own error message
// names what the operator actually typed.
func TestExpandAliasLeavesUnknownNamesAlone(t *testing.T) {
	t.Setenv("MUSTER_HOME", t.TempDir())
	t.Setenv("MUSTER_DEVICE_NAME", "personal")
	exists := func(string) bool { return false }
	if got, want := expandAlias("typo", exists), "typo"; got != want {
		t.Fatalf("expandAlias = %q, want %q", got, want)
	}
}

// TestRosterAliasExistsConstructionIsLazy is the eager-fetch regression this
// task's review caught: every call site passes rosterAliasExists() as an
// argument expression to expandAlias — events.go, tail.go, thread.go,
// humancli.go, identity.go — even when the operator typed no name to expand
// at all (an empty --agent, --from, or deregister arg). expandAlias itself
// already short-circuits on an empty `given` before ever calling `exists`,
// but Go evaluates a call's arguments before the call runs, so MERELY
// CONSTRUCTING rosterAliasExists() paid a full list_agents round trip
// regardless of whether anything downstream ever asked it a question. Plain
// `muster events` (no --agent) must not pay that cost.
//
// Proven the same way TestHookSessionEndUnresolvableIdentityNeverDialsDaemon
// proves its own "never dials" invariant: point at a dead, isolated socket
// path with auto-spawn left ACTIVE (MUSTER_NO_AUTOSPAWN unset — see
// client.dialOrSpawn) and bound with a timeout well under dialOrSpawn's
// ~5-second spawn-and-retry loop. A regression that dials eagerly would spawn
// the daemon (re-exec'ing this very test binary as "serve" — the same fork
// hazard that test guards against) and only return once that loop times out,
// well past the bound; a construction that touches nothing returns
// immediately.
func TestRosterAliasExistsConstructionIsLazy(t *testing.T) {
	dir, cleanup, err := mustertest.ShortHome()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	t.Setenv("MUSTER_HOME", dir) // isolated, dead socket path — nothing listens here
	t.Setenv("MUSTER_NO_AUTOSPAWN", "")

	done := make(chan struct{})
	go func() {
		rosterAliasExists()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("rosterAliasExists() did not return promptly — construction dialed the daemon eagerly")
	}
}
