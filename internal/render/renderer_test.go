package render

import "testing"

// dispTarget must show broadcast targets as-is — never label-resolve them
// through the bare-alias fallthrough. The label planted under the literal
// target string proves the fallthrough is not taken.
func TestDispTargetScopedBroadcastShownAsIs(t *testing.T) {
	r := NewRenderer(nil, map[string]string{"broadcast:web": "WRONG"}, false, false, 120)
	if got := r.dispTarget("broadcast:web"); got != "broadcast:web" {
		t.Fatalf("scoped broadcast target rendered %q, want broadcast:web", got)
	}
	if got := r.dispTarget("broadcast"); got != "broadcast" {
		t.Fatalf("global broadcast target rendered %q, want broadcast", got)
	}
}

// TestDispStripsTheLocalPrefix covers events/tail/station's alias display
// (--aliases mode, or any agent with no label yet): this machine's own
// prefix comes off, a foreign one is left alone. Same human-surface rule as
// internal/cli's dispAlias.
func TestDispStripsTheLocalPrefix(t *testing.T) {
	t.Setenv("MUSTER_HOME", t.TempDir())
	t.Setenv("MUSTER_DEVICE_NAME", "personal")
	r := NewRenderer(nil, nil, true, false, 120)
	if got, want := r.disp("personal-dotfiles/main"), "dotfiles/main"; got != want {
		t.Fatalf("disp = %q, want %q", got, want)
	}
	if got, want := r.disp("work-dotfiles/main"), "work-dotfiles/main"; got != want {
		t.Fatalf("disp of foreign alias = %q, want %q", got, want)
	}
}

// TestDispTargetStripsAgentTargetsOnly covers dispTarget's three shapes: an
// agent:-prefixed target is an alias and must strip; role: and broadcast
// targets are role/project names, never aliases, and must render untouched
// even when they happen to carry this machine's prefix string.
func TestDispTargetStripsAgentTargetsOnly(t *testing.T) {
	t.Setenv("MUSTER_HOME", t.TempDir())
	t.Setenv("MUSTER_DEVICE_NAME", "personal")
	r := NewRenderer(nil, nil, true, false, 120)
	if got, want := r.dispTarget("agent:personal-dotfiles/main"), "dotfiles/main"; got != want {
		t.Fatalf("agent target = %q, want %q", got, want)
	}
	if got, want := r.dispTarget("role:personal-reviewer"), "role:personal-reviewer"; got != want {
		t.Fatalf("role target must not be stripped, got %q, want %q", got, want)
	}
	if got, want := r.dispTarget("broadcast:personal-proj"), "broadcast:personal-proj"; got != want {
		t.Fatalf("scoped broadcast target must not be stripped, got %q, want %q", got, want)
	}
}

// TestDispRendersBothSidesOfAStripCollisionInFull is spec §5's fallback, the
// one the other two human surfaces (cli.aliasDisplay,
// station.computeAliasStripCollisions) already implement. A legacy bare
// "relay" beside a locally seeded "personal-relay" both strip to "relay", and
// a feed showing one name for two agents is worse than one showing a prefix.
func TestDispRendersBothSidesOfAStripCollisionInFull(t *testing.T) {
	t.Setenv("MUSTER_HOME", t.TempDir())
	t.Setenv("MUSTER_DEVICE_NAME", "personal")
	labels := map[string]string{"personal-relay": "", "relay": ""}
	r := NewRenderer(nil, labels, true, false, 120)
	if got, want := r.disp("personal-relay"), "personal-relay"; got != want {
		t.Fatalf("disp = %q, want %q (a collision must render full)", got, want)
	}
	if got, want := r.disp("relay"), "relay"; got != want {
		t.Fatalf("disp = %q, want %q", got, want)
	}
	// A non-colliding row in the same roster is unaffected.
	if got, want := r.disp("personal-dotfiles/main"), "dotfiles/main"; got != want {
		t.Fatalf("disp = %q, want %q", got, want)
	}
}

// TestSetLabelsRefreshesTheStripCollisionSet: tail and station both replace
// the label map when a newly-registered agent appears, and that is exactly
// when a collision can come into existence — the set has to be recomputed
// with it, not frozen at construction.
func TestSetLabelsRefreshesTheStripCollisionSet(t *testing.T) {
	t.Setenv("MUSTER_HOME", t.TempDir())
	t.Setenv("MUSTER_DEVICE_NAME", "personal")
	r := NewRenderer(nil, map[string]string{"personal-relay": ""}, true, false, 120)
	if got, want := r.disp("personal-relay"), "relay"; got != want {
		t.Fatalf("disp before the collision = %q, want %q", got, want)
	}
	r.SetLabels(map[string]string{"personal-relay": "", "relay": ""})
	if got, want := r.disp("personal-relay"), "personal-relay"; got != want {
		t.Fatalf("disp after the collision appeared = %q, want %q", got, want)
	}
}

// TestDispTargetRendersCollisionsInFullToo: dispTarget delegates to disp for
// both alias-shaped targets, so the guard has to cover the 'agent:' form and
// the bare-alias (nudge) form as well as the plain WHO column.
func TestDispTargetRendersCollisionsInFullToo(t *testing.T) {
	t.Setenv("MUSTER_HOME", t.TempDir())
	t.Setenv("MUSTER_DEVICE_NAME", "personal")
	r := NewRenderer(nil, map[string]string{"personal-relay": "", "relay": ""}, true, false, 120)
	if got, want := r.dispTarget("agent:personal-relay"), "personal-relay"; got != want {
		t.Fatalf("agent target = %q, want %q", got, want)
	}
	if got, want := r.dispTarget("personal-relay"), "personal-relay"; got != want {
		t.Fatalf("bare-alias target = %q, want %q", got, want)
	}
}

// TestLabelStillWinsOverAStripCollision: a collision is about two aliases
// rendering the same string. A label already replaces the alias entirely, and
// station's own computeLabelCollisions guards THAT ambiguity separately — so
// a labelled row keeps its label rather than falling back to a full alias
// nobody asked to see.
func TestLabelStillWinsOverAStripCollision(t *testing.T) {
	t.Setenv("MUSTER_HOME", t.TempDir())
	t.Setenv("MUSTER_DEVICE_NAME", "personal")
	r := NewRenderer(nil, map[string]string{"personal-relay": "standard 2000", "relay": ""}, false, false, 120)
	if got, want := r.disp("personal-relay"), "standard 2000"; got != want {
		t.Fatalf("disp = %q, want the label %q", got, want)
	}
}
