package render

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/subaud-tools/muster/internal/device"
	"github.com/subaud-tools/muster/internal/display"
)

// rawFieldWidth bounds display.Sanitize's width cap for row fields (subject,
// detail, who) before their final column truncation in Line() — large enough
// that it never bites ahead of the real budget, while still running every
// field through the one canonical sanitizer (control-char stripping +
// whitespace-run collapsing) rather than a bespoke oneLine/truncate pair.
const rawFieldWidth = 4096

const (
	whoMaxWidth      = 34  // WHO column cap (display columns)
	whoSenderWidth   = 15  // sender half of a 'from → to' pair (display columns)
	defaultLineWidth = 120 // line budget when $COLUMNS is unset; --width overrides
)

// Renderer holds the display state shared by events, tail, and the station
// feed pane: the alias→label map, column widths, and time format. Widths are
// seeded from the rows about to be printed (events) or the backlog
// (tail/station) and only grow, so columns stay aligned to real content
// instead of guessed fixed widths while the feed streams.
type Renderer struct {
	labels map[string]string // alias → current label ("" or missing = show the alias)
	// stripCollide is alias → "render this one in full": stripping this
	// machine's device prefix off it would produce the same string as
	// stripping some OTHER alias in the same roster. Derived from labels'
	// KEYS, which are the whole roster (both producers — render.LoadLabels
	// and station's applyAgents — key one entry per registered agent,
	// label or not), so it is recomputed wherever labels is.
	stripCollide map[string]bool
	aliases      bool // true = raw aliases, ignore labels
	fullTime     bool // true = date + time; false = time only
	whoW         int
	threadW      int
	width        int // total line budget (terminal columns)
}

// NewRenderer sizes columns from rows. labels may be nil (aliases render
// as-is). width <= 0 falls back to $COLUMNS, then defaultLineWidth.
func NewRenderer(rows []EventRow, labels map[string]string, aliases, fullTime bool, width int) *Renderer {
	r := &Renderer{labels: labels, stripCollide: aliasStripCollisions(labels), aliases: aliases, fullTime: fullTime, whoW: 6, threadW: 3, width: width}
	if r.width <= 0 {
		if c, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil && c > 40 {
			r.width = c
		} else {
			r.width = defaultLineWidth
		}
	}
	for _, e := range rows {
		r.fit(e)
	}
	return r
}

// HasLabel reports whether alias currently has a non-empty entry in the
// renderer's label map — used by streaming callers (tail, station's feed)
// to decide whether the map needs a refresh for a newly-seen agent.
func (r *Renderer) HasLabel(alias string) bool {
	return r.labels[alias] != ""
}

// SetLabels replaces the renderer's label map (e.g. after a streaming
// caller refreshes it for a newly-registered agent). The strip-collision set
// is recomputed with it: a newly-registered agent is exactly what brings a
// collision into existence, so a set frozen at construction would miss the
// case it exists for.
func (r *Renderer) SetLabels(labels map[string]string) {
	r.labels = labels
	r.stripCollide = aliasStripCollisions(labels)
}

// aliasStripCollisions reports, for each alias in the roster-derived label
// map, whether stripping this machine's device prefix would render it
// identically to some OTHER alias's post-strip form — a legacy bare "relay"
// beside a locally seeded "personal-relay", or a foreign machine's bare alias
// matching one of ours. Both sides then render in full (spec §5): a feed
// showing one name for two agents is worse than one showing a prefix.
//
// This is the third implementation of that rule, matching
// cli.aliasDisplay and station.computeAliasStripCollisions. It differs
// from them only in what it counts over: they see one view's rows, this sees
// the whole roster, which is what the renderer has at render time — a
// streamed feed prints one event at a time and never holds "the set of rows
// on screen". Counting over the roster is the more conservative direction: it
// can render an alias in full because of a colliding agent that has no event
// in the visible window, which is legible, where the reverse would not be.
//
// An alias with no roster row at all — an event from an agent since purged —
// is absent from the map and strips unconditionally, exactly as before.
func aliasStripCollisions(labels map[string]string) map[string]bool {
	name := device.Name()
	if name == "" || len(labels) == 0 {
		return nil
	}
	count := make(map[string]int, len(labels))
	for alias := range labels {
		count[device.Strip(name, alias)]++ // map keys are already distinct
	}
	var out map[string]bool
	for alias := range labels {
		if count[device.Strip(name, alias)] > 1 {
			if out == nil {
				out = map[string]bool{}
			}
			out[alias] = true
		}
	}
	return out
}

// SetAliases toggles raw-alias mode (true = ignore labels, show aliases
// as-is) — station's `a` key flips this at runtime; every other caller sets
// it once at construction via NewRenderer.
func (r *Renderer) SetAliases(aliases bool) {
	r.aliases = aliases
}

// SetWidth updates the renderer's total line-width budget — station's
// bordered feed pane calls this on every render so Line()'s WHAT-column cap
// tracks the pane's actual INNER width rather than the whole terminal's (a
// pane narrower than the terminal must still never let a line run past its
// own box). Mirrors SetLabels/SetAliases' role as a runtime knob on an
// otherwise construction-time Renderer. width <= 0 falls back exactly like
// NewRenderer's own construction-time default ($COLUMNS, then
// defaultLineWidth) — callers that don't yet know a real pane width (no
// tea.WindowSizeMsg has landed) get the same behavior NewRenderer always had.
func (r *Renderer) SetWidth(width int) {
	if width <= 0 {
		if c, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil && c > 40 {
			width = c
		} else {
			width = defaultLineWidth
		}
	}
	r.width = width
}

// fit grows column widths to accommodate e; it never shrinks them, keeping
// alignment stable across a streamed tail.
func (r *Renderer) fit(e EventRow) {
	if n := display.Width(r.who(e)); n > r.whoW {
		r.whoW = min(n, whoMaxWidth)
	}
	if n := len(r.thread(e)); n > r.threadW {
		r.threadW = n
	}
}

// disp resolves an alias for display: the agent's current label when one is
// known (pinned or auto topic), the alias otherwise — with this machine's
// device-name prefix stripped, exactly like the rest of the CLI's human
// surfaces (internal/cli's dispAlias/aliasDisplay). Labels resolve at
// render time, so old events show whoever the agent is *today* — use
// --aliases for the stable raw view (still stripped: --aliases only turns
// off label resolution, it does not re-expose the wire-format alias).
//
// A label wins over the strip-collision fallback: it has already replaced the
// alias entirely, and station guards label ambiguity separately
// (computeLabelCollisions). The fallback is about two ALIASES rendering the
// same string.
func (r *Renderer) disp(alias string) string {
	if !r.aliases {
		if l := r.labels[alias]; l != "" {
			return l
		}
	}
	if r.stripCollide[alias] {
		return alias
	}
	return device.Strip(device.Name(), alias)
}

// dispTarget renders a journal target ('agent:x' / 'role:r' / 'broadcast' /
// 'broadcast:<project>' / bare alias) for display. Only the 'agent:' and
// bare-alias forms carry an alias — role names and broadcast/project scopes
// are never run through disp/device.Strip, since they are not aliases and a
// role or project name that happened to share this device's prefix must not
// be mangled.
func (r *Renderer) dispTarget(target string) string {
	if a, ok := strings.CutPrefix(target, "agent:"); ok {
		return r.disp(a)
	}
	if strings.HasPrefix(target, "role:") || target == "broadcast" || strings.HasPrefix(target, "broadcast:") || target == "" {
		return target
	}
	return r.disp(target) // nudge's bare alias
}

// who renders the direction column. Sends and tasks are directed at an
// agent or role and show 'from → to'; notifies and nudges are deliveries TO
// the shown agent ('→ x'); replies, claims, and transitions are directed at
// the THREAD — the paired notify rows below show who that fanned out to —
// so they show the actor alone.
func (r *Renderer) who(e EventRow) string {
	switch e.Kind {
	case "send", "task":
		return display.Sanitize(r.disp(e.Agent), whoSenderWidth) + " → " + r.dispTarget(e.Target)
	case "notify":
		return "→ " + r.disp(e.Agent)
	case "nudge":
		return "→ " + r.dispTarget(e.Target)
	default:
		return r.disp(e.Agent)
	}
}

// thread renders '#<id>' or nothing for thread-less events.
func (r *Renderer) thread(e EventRow) string {
	if e.ThreadID == 0 {
		return ""
	}
	return "#" + strconv.FormatInt(e.ThreadID, 10)
}

// intentTag maps a thread's effective intent to the journal suffix what()
// appends on send/task rows — "" (unspecified) renders no tag. The three
// non-empty values mirror internal/store's IntentFYI/IntentReply/IntentAction
// vocabulary (duplicated here deliberately: render is a peer client of the
// daemon over the wire, not a store-internal package).
func intentTag(intent string) string {
	switch intent {
	case "fyi":
		return " [fyi]"
	case "reply-requested":
		return " [reply?]"
	case "action-requested":
		return " [action]"
	default:
		return ""
	}
}

// what renders the payload column without repeating itself: a send/task
// detail duplicates the subject by construction, so only the subject prints,
// with an intent tag appended when the thread's effective intent is set;
// notify folds its unread count into the outcome ('lit(2)') and appends the
// subject for context; a reply with a non-empty body preview (Detail) shows
// '↳ <preview>' instead of the thread subject — showing both makes an
// announcement and its reply look like a duplicate send.
func (r *Renderer) what(e EventRow) string {
	subject := display.Sanitize(e.Subject, rawFieldWidth)
	detail := display.Sanitize(e.Detail, rawFieldWidth)
	switch e.Kind {
	case "send", "task":
		if subject != "" {
			return subject + intentTag(e.Intent)
		}
		return detail + intentTag(e.Intent)
	case "reply":
		if detail != "" {
			return "↳ " + detail
		}
		if subject != "" {
			return subject
		}
		return detail
	case "claim":
		if subject != "" {
			return subject
		}
		return detail
	case "notify":
		out := detail
		if e.Count > 0 {
			out = fmt.Sprintf("%s(%d)", detail, e.Count)
		}
		if subject != "" {
			out += " — " + subject
		}
		return out
	case "transition":
		if subject != "" {
			return detail + " — " + subject
		}
		return detail
	default: // read, nudge
		return detail
	}
}

func (r *Renderer) timeFormat() string {
	if r.fullTime {
		return "2006-01-02 15:04:05"
	}
	return "15:04:05"
}

// PadDisplay right-pads s with spaces to w DISPLAY COLUMNS (not rune count),
// matching the units display.Width and display.Sanitize already use. fmt's
// "%-*s" pads by rune count, which misaligns columns and blows the line
// budget once a field holds wide (CJK/fullwidth) runes — a rune-counted pad
// adds more spaces than the display actually needs, since each wide rune
// already occupies 2 columns worth of the target width. Every column whose
// content can legitimately hold non-ASCII (WHO, and — for symmetry — TIME/
// THREAD, which are ASCII in practice but should never silently rely on
// that) goes through this helper instead.
func PadDisplay(s string, w int) string {
	if n := w - display.Width(s); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}

// Header writes the column header line shared by events, tail, and station.
func (r *Renderer) Header(w io.Writer) {
	_, _ = fmt.Fprintf(w, "%s  %-6s  %s  %s  %s\n",
		PadDisplay("TIME", len(r.timeFormat())), "KIND", PadDisplay("WHO", r.whoW), PadDisplay("THREAD", r.threadW), "WHAT")
}

// Line writes exactly one line for e — safe for line-by-line streaming (no
// long-lived tabwriter that only aligns at Flush). The WHAT column is capped
// to the remaining terminal width so a row can never wrap. Column padding is
// display-width based throughout (PadDisplay), matching the `used` budget
// math below, which sums display-width column widths — the two must stay in
// the same unit or the WHAT truncation cap drifts from the line's real
// rendered width.
func (r *Renderer) Line(w io.Writer, e EventRow) {
	r.fit(e)
	ts := time.UnixMilli(e.TS).Format(r.timeFormat())
	used := len(r.timeFormat()) + 2 + 6 + 2 + r.whoW + 2 + r.threadW + 2
	budget := r.width - used
	if budget < 10 {
		budget = 10
	}
	_, _ = fmt.Fprintf(w, "%s  %-6s  %s  %s  %s\n",
		PadDisplay(ts, len(r.timeFormat())), e.Kind, PadDisplay(display.Sanitize(r.who(e), whoMaxWidth), r.whoW), PadDisplay(r.thread(e), r.threadW), display.Sanitize(r.what(e), budget))
}
