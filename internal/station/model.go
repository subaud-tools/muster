// Package station implements `muster station`, the operator TUI. Spec
// §5-LOCK (2026-07-17) is the operator-approved, locked design: pure STACK
// navigation (Enter pushes, Esc pops exactly one frame, g pops home), a
// mailbox as its own page, one canonical header-badge renderer, and an
// agent page with a header band (+ a marked-but-empty vitals slot, 0.6.1)
// above its full-width threads table. Like `muster tail`, station never
// streams — it polls the daemon on a tea.Tick, but the poll loop is owned by
// the Bubble Tea MODEL instead of a bare for-loop, so the event journal
// cursor advances only when the model actually applies an events page (see
// Update's eventsMsg branch) rather than whenever a fetch happens to
// complete.
package station

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/subaud-tools/muster/internal/device"
	"github.com/subaud-tools/muster/internal/nudge"
	"github.com/subaud-tools/muster/internal/render"
)

// keyMap is station's canonical key vocabulary (spec §5-LOCK: "enter/esc/g/
// j-k/m/s/r/n/slash/a/?/q"). Enter drills/opens (pushes a nav frame); Esc
// pops exactly one frame everywhere; g pops all the way home (projects); j/k
// move the current list's own cursor (or scroll, while reading); s send
// (global, roster-filtered picker); r reply (the currently selected/open
// thread, wherever one is in view); n nudge (the agents list / an agent's
// own page); m jump to the mailbox (an overlay page); / filter the current
// left list; a aliases toggle; ? help overlay; q quit. Tab is retired
// entirely (spec §5-LOCK decision B: "Tab retires except where a genuine
// second focus target exists" — nowhere in the locked design has one any
// more: the right pane is always preview-only, and mail is its own page
// rather than an L0 toggle).
type keyMap struct {
	Down, Up, Quit, Enter, Esc, Home, End                                          key.Binding
	Send, Reply, Nudge, Deregister, Transition, Filter, Aliases, CycleIntent, Help key.Binding
	MailJump, ClearInbox, RetractStanding                                          key.Binding
}

var keys = keyMap{
	Down:  key.NewBinding(key.WithKeys("j", "down"), key.WithHelp("j/↓", "move")),
	Up:    key.NewBinding(key.WithKeys("k", "up"), key.WithHelp("k/↑", "move")),
	Quit:  key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
	Enter: key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "drill")),
	Esc:   key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "back")),
	// Home implements 'g' (spec §5-LOCK decision B: "g jumps home").
	Home: key.NewBinding(key.WithKeys("g"), key.WithHelp("g", "home")),
	// End snaps the focused thread reader to its live tail — a no-op
	// everywhere else.
	End:        key.NewBinding(key.WithKeys("end", "G"), key.WithHelp("end/G", "newest")),
	Send:       key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "send")),
	Reply:      key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "reply")),
	Nudge:      key.NewBinding(key.WithKeys("n"), key.WithHelp("n", "nudge")),
	Deregister: key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "deregister")),
	ClearInbox: key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "clear inbox")),
	// RetractStanding is capital R: retracting a standing broadcast is
	// deliberate and confirmed, and must not sit one shift away from nothing
	// on the everyday 'r' reply.
	RetractStanding: key.NewBinding(key.WithKeys("R"), key.WithHelp("R", "retract standing")),
	Transition:      key.NewBinding(key.WithKeys("t"), key.WithHelp("t", "task transition")),
	Filter:          key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "filter")),
	Aliases:         key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "aliases")),
	CycleIntent:     key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "intent")), // composer-local only; the base nav vocabulary has no Tab binding
	Help:            key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
	MailJump:        key.NewBinding(key.WithKeys("m"), key.WithHelp("m", "mail")),
}

// Layout knobs. defaultRows/eventBacklog bound how much of the global events
// journal the model keeps — no longer rendered as its own pane, but still
// the source buffer an agent page's activity list filters client-side (see
// agentActivity in views.go), and still the same cursor-disciplined buffer
// the data loop requires.
const (
	defaultRows  = 20
	eventBacklog = 500
)

// initialEventBacklog bounds the cold-start (and bootstrap-retry) backlog
// fetch.
const initialEventBacklog = defaultRows

// conversationReaderRows bounds the right pane's visible WRAPPED-line budget
// when a thread is being read.
const conversationReaderRows = defaultRows

// hasVitals gates the agent-page header band's vitals slot (spec §5-LOCK
// decision C): context/token usage and a "working on…" status are DESIGNED
// IN NOW — the container below is built and wired end to end — but the
// transcript-reading HOOK that fills them ships in 0.6.1, so this stays
// false for the whole 0.6.0 branch. Flipping it to true (once 0.6.1 lands
// real agentVitals data on agentEnriched) is a data wire-up, not a
// redesign — renderVitalsLines already renders correctly when hasVitals is
// true, see its own test.
const hasVitals = false

// agentVitals is the 0.6.1 vitals payload's shape (spec §5-LOCK screen 4):
// "working on …", "ctx ~Nk / Wk P%", "out Nk last turn · ended <t>". No wire
// data populates this in 0.6.0 — every field is its zero value, and
// renderVitalsLines never runs anyway (see hasVitals) — but the type exists
// now so 0.6.1 only has to populate it, not design it.
type agentVitals struct {
	WorkingOn       string
	CtxUsedK        int
	CtxWindowK      int
	CtxPercent      int
	OutTokensK      int
	LastTurnEndedAt int64 // ms epoch
}

// agentEnriched is one roster row: the wire agent row plus tmux-live state
// and its session's unread count.
type agentEnriched struct {
	Alias          string
	Project        string
	ModelType      string
	Role           string
	DeviceName     string
	DeviceID       string
	Departed       bool
	Label          string
	LabelManual    bool
	SocketPath     string
	PaneID         string
	SessionID      string
	SessionCreated int64
	Live           bool
	Unread         int
	Action         bool // true when the session's unread includes an action-requested thread
	ActionCount    int  // the session's action-requested unread count
}

// listThreadRow mirrors store.Thread's wire JSON for the thread lists.
type listThreadRow struct {
	ID        int64  `json:"id"`
	Kind      string `json:"kind"`
	FromAgent string `json:"from_agent"`
	ToKind    string `json:"to_kind"`
	ToTarget  string `json:"to_target"`
	Subject   string `json:"subject"`
	Ref       string `json:"ref"`
	Status    string `json:"status"`
	Intent    string `json:"intent"`
	// Standing/StandingRetracted: a LIVE standing broadcast (standing and not
	// retracted) greets every new session until read — station marks it and
	// 'R' retracts it.
	Standing          bool  `json:"standing"`
	StandingRetracted bool  `json:"standing_retracted"`
	CreatedAt         int64 `json:"created_at"`
	UpdatedAt         int64 `json:"updated_at"`
	// OriginProject is the sender's registered project stamped at thread
	// creation time — "" when unstamped (a pre-migration row whose sender no
	// longer resolves, or a genuinely unregistered sender). nav.go's
	// threadProjects unions this with the roster-derived participant
	// projects so a thread whose participants have ALL since deregistered
	// still has a project home.
	OriginProject      string `json:"origin_project"`
	LastFrom           string `json:"last_from"`
	LastAt             int64  `json:"last_at"`
	EntryCount         int    `json:"entry_count"`
	RetainedActiveTask bool   `json:"-"`
}

// Options configures a Model — station.Run's flags, or whatever a test wants
// to fix directly.
type Options struct {
	Interval time.Duration // poll interval (--interval, default 1s)
	Aliases  bool          // show raw aliases instead of labels (--aliases)
	Width    int           // total line budget (--width; 0 = default)
	Alias    string        // this station's own registered alias
	Nudger   nudger        // test seam for the 'n' nudge action; nil (default) uses nudge.TmuxNudger{} (real tmux)

	// SocketPath/SessionID/SessionCreated are this station's own registered
	// tmux tuple (from registerStation's Capture) — get_inbox's ownership
	// check (spec 2026-08-21 §3.2, task 7) needs this proof so station's own
	// open-to-acknowledge read (fetchInboxAckCmd) actually marks read
	// instead of becoming a no-op peek.
	SocketPath     string
	SessionID      string
	SessionCreated int64
}

var taskTransitionChoices = []struct {
	label  string
	action string
	status string
}{
	{label: "claim", action: "claim", status: "claimed"},
	{label: "needs info", status: "needs_info"},
	{label: "blocked", status: "blocked"},
	{label: "complete", status: "completed"},
	{label: "decline", status: "declined"},
	{label: "cancel", status: "cancelled"},
	{label: "reopen", status: "open"},
}

func taskTransitionChoiceDisabled(choice int, currentStatus string) bool {
	item := taskTransitionChoices[choice]
	return item.status == currentStatus || (item.action == "claim" && currentStatus != "open")
}

type taskTransitionState struct {
	open          bool
	editingNote   bool
	threadID      int64
	currentStatus string
	choice        int
	note          textinput.Model
}

// Model is the station Bubble Tea model. It owns the event journal cursor —
// Update's eventsMsg branch is the ONLY place it advances, and only after a
// page is actually applied.
type Model struct {
	caller render.Caller
	opts   Options

	cursor       int64 // event journal read cursor; advances only on applied event pages
	bootstrapped bool  // false until the cold-start BACKLOG fetch has applied; gates pollCmd's mode
	pollGen      int64 // current poll generation; bumped per tick, stamped onto each fetch's msg so a stale in-flight fetch from an older tick is discarded rather than applied (Update's eventsMsg case)
	events       []render.EventRow
	activity     *render.Renderer // renders agent-activity lines

	agents            []agentEnriched
	labels            map[string]string // alias → current label, shared by the activity view and every row renderer
	labelCollide      map[string]bool   // alias → true when its current label needs its alias appended to stay unambiguous (spec §5-LOCK item 7)
	aliasStripCollide map[string]bool   // alias → true when stripping this machine's device prefix would render it identically to another agent's post-strip alias; forces full form on BOTH sides (see computeAliasStripCollisions)

	threads              []listThreadRow
	activeTasksTruncated bool

	// Navigation (spec §5-LOCK decision B: pure stack). stack[0] is always
	// {screen: screenProjects}; screen mirrors stack's top for the many call
	// sites that only care "what screen am I rendering/routing keys for"
	// without walking the stack themselves. project/agent/conversation are
	// the CURRENT LIST CURSOR at whichever level is active — set by j/k
	// movement, carried unmodified by push/pop (a frame's own project/agent
	// are a snapshot for BREADCRUMB purposes only, see navFrame's doc; popping
	// never needs to "restore" these fields because a deeper screen never
	// mutates a shallower one's cursor). mailboxSel is the mailbox list's own,
	// independent cursor (a different ID space/list from conversation, so an
	// 'm' excursion mid-browse can never disturb the browse cursor it
	// interrupted).
	stack         []navFrame
	screen        screen
	singleProject bool // recomputed every applyAgents: gates L0 auto-skip and Esc-from-L1 no-op
	everNavigated bool // gates the ONE-TIME auto-skip so projects appearing/disappearing later never yanks the operator back to L0 mid-session
	project       string
	agent         string
	conversation  int64
	mailboxSel    int64

	ackedThreads map[int64]bool // thread IDs already open-to-acknowledged THIS run — opening the same thread twice must not re-fire get_inbox

	// lastActive is alias -> the newest journal event's TS (ms epoch) where
	// that alias was the ACTOR ("last active: <relative>") — populated
	// lazily by fetchLastActiveCmd, scoped each poll tick to the current
	// project's agent-list membership (see pollCmd), never fetched from
	// View(). A missing entry (nil map, or no key yet) simply renders no
	// last-active text rather than "unknown".
	lastActive map[string]int64

	// Thread reader (screenAgent/screenProject's right-pane preview, or
	// screenRead's full-width focused view): the currently loaded thread's
	// page.
	viewThreadID int64
	viewEntries  []threadEntryRow
	viewOffset   int64
	viewTotal    int64
	viewCursor   int
	viewLoading  bool
	viewGen      int64

	// composer implements the bottom-line composer ('s' send via a
	// roster-filtered target picker anywhere; 'r' reply to the currently
	// selected/open thread; intent cycled F/R/A, Enter submits, Esc cancels).
	composer composerState

	// nudgeConfirmAlias is "" (no pending confirmation) or the alias 'n' is
	// asking "nudge <label>? y/n" about — handleNudgeConfirmKey owns every
	// key while it's set. nudger is the send-keys seam (DI'd via
	// Options.Nudger; tests inject a fake so a model test never shells to
	// real tmux).
	nudgeConfirmAlias      string
	deregisterConfirmAlias string
	// retractConfirmThread is 0 (no pending confirmation) or the standing
	// broadcast 'R' is asking "retract …? y/n" about.
	retractConfirmThread int64
	taskTransition       taskTransitionState
	nudger               nudger

	// filter implements '/': a substring filter over the CURRENT left list's
	// rendered row text, selection-aware exactly like nav.go's generic
	// selectionVisible/snapSelection/moveSelection.
	filter filterState

	helpOpen bool // '?' overlay

	status   string
	quitting bool

	// termWidth/termHeight are the last tea.WindowSizeMsg the program has
	// seen (0,0 before the first one arrives) — the ONLY inputs layout()
	// needs to size the two columns, and the sole input to the narrow-mode
	// (< ~110 cols) single-column collapse.
	termWidth  int
	termHeight int
}

// composerPhase is the composer's own little state machine: closed (no
// composer on screen), picking a target (the 's' roster-filtered picker,
// before the body input), or editing the body (both 's' after a target is
// picked, and 'r' — which skips straight here, its target is already known).
type composerPhase int

const (
	composerClosed composerPhase = iota
	composerPickingTarget
	composerEditingBody
)

// composerKind distinguishes the composer's two submit ops: send_message
// ('s') vs reply ('r').
type composerKind int

const (
	composerKindSend composerKind = iota
	composerKindReply
)

// composerState is the Model's composer sub-state (see Model.composer).
type composerState struct {
	phase     composerPhase
	kind      composerKind
	intent    intentState
	target    string          // resolved send target alias (composerKindSend)
	threadID  int64           // reply target thread (composerKindReply)
	filter    textinput.Model // target-picker's filter field (composerKindSend only)
	pickerIdx int
	input     textinput.Model // body input
	err       string          // op error, shown alongside the composer rather than crashing
}

// intentState is the composer's F/R/A cycle.
type intentState int

const (
	intentFYI intentState = iota
	intentReplyRequested
	intentActionRequested
)

// next cycles F → R → A → F.
func (i intentState) next() intentState { return (i + 1) % 3 }

// tag renders the composer's F/R/A indicator letter.
func (i intentState) tag() string {
	switch i {
	case intentReplyRequested:
		return "R"
	case intentActionRequested:
		return "A"
	default:
		return "F"
	}
}

// wire renders the intent's send_message wire value.
func (i intentState) wire() string {
	switch i {
	case intentReplyRequested:
		return "reply-requested"
	case intentActionRequested:
		return "action-requested"
	default:
		return "fyi"
	}
}

// filterState is the Model's '/' filter sub-state (see Model.filter): list
// names which left list the active query applies to (set when '/' is
// pressed); query is live-updated as the operator types; editing gates
// whether handleFilterKey currently owns the keys (Enter stops editing but
// leaves query applied; Esc clears the filter entirely).
type filterState struct {
	list    llList
	query   string
	editing bool
	input   textinput.Model
}

// NewModel constructs a station Model against caller (the daemon-transport
// seam — production wraps internal/client.Call via daemonCaller, tests hand
// in a fake).
func NewModel(caller render.Caller, opts Options) Model {
	if opts.Interval <= 0 {
		opts.Interval = time.Second
	}
	n := opts.Nudger
	if n == nil {
		n = nudge.TmuxNudger{} // production default: real tmux send-keys
	}
	return Model{
		caller:   caller,
		opts:     opts,
		nudger:   n,
		activity: render.NewRenderer(nil, nil, opts.Aliases, false, opts.Width),
		stack:    []navFrame{{screen: screenProjects}},
		screen:   screenProjects,
		status:   "connecting…",
	}
}

// Init issues the first poll immediately (so the screen isn't blank for a
// full --interval) and schedules the first tick. Since a fresh Model is not
// yet bootstrapped, that first poll's events fetch is pollCmd's backlog
// branch, not follow — see pollCmd and applyEvents.
func (m Model) Init() tea.Cmd {
	return tea.Batch(m.pollCmd(), tickCmd(m.opts.Interval))
}

// pollCmd issues the tick's three independent fetches: events, list_agents,
// list_threads. Each yields its own message; none may be combined into a
// shared snapshot before Update sees them, so a failure in one can never
// block or corrupt the others, and no decision is ever derived from a mixed
// tick bundle.
//
// The events fetch's MODE depends on m.bootstrapped: until the cold-start
// backlog fetch has actually applied, every attempt — including retries
// after a failed backlog fetch — stays in BACKLOG mode.
func (m Model) pollCmd() tea.Cmd {
	eventsCmd := fetchEventsCmd(m.caller, m.cursor, m.pollGen)
	if !m.bootstrapped {
		eventsCmd = fetchBacklogEventsCmd(m.caller, initialEventBacklog, m.pollGen)
	}
	cmds := []tea.Cmd{
		eventsCmd,
		fetchAgentsCmd(m.caller),
		fetchThreadsCmd(m.caller),
	}
	// Last-active enrichment: one small list_events(agent=alias) lookup per
	// CURRENT project's agent-list member, tagged with this tick's pollGen so
	// a slow in-flight fetch from an older tick is discarded exactly like
	// eventsMsg (see applyLastActive) — issued from the poll Cmd only, never
	// from a keypress or View().
	for _, a := range m.agentStripRows() {
		cmds = append(cmds, fetchLastActiveCmd(m.caller, a.Alias, m.pollGen))
	}
	return tea.Batch(cmds...)
}

// Update implements tea.Model. See eventsMsg/agentsMsg/threadsMsg handling
// below for the cursor discipline (identical to tail: max_id, regression
// reset, errors → status line + retry, never exit).
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.termWidth = msg.Width
		m.termHeight = msg.Height
		return m, nil
	case tea.KeyMsg:
		return m.handleKey(msg)
	case tickMsg:
		m.pollGen++ // a new tick supersedes any still-in-flight fetch from the previous one
		return m, tea.Batch(m.pollCmd(), tickCmd(m.opts.Interval))
	case eventsMsg:
		if msg.gen != m.pollGen {
			return m, nil // stale: a newer tick has already superseded this in-flight fetch
		}
		return m.applyEvents(msg), nil
	case agentsMsg:
		return m.applyAgents(msg), nil
	case threadsMsg:
		return m.applyThreads(msg)
	case threadPageMsg:
		return m.applyThreadPage(msg)
	case inboxAckMsg:
		return m.applyInboxAck(msg), nil
	case composerSentMsg:
		return m.applyComposerSent(msg), nil
	case nudgeResultMsg:
		return m.applyNudgeResult(msg), nil
	case deregisterResultMsg:
		return m.applyDeregisterResult(msg)
	case retractStandingResultMsg:
		return m.applyRetractStandingResult(msg)
	case markReadResultMsg:
		return m.applyMarkReadResult(msg)
	case taskTransitionResultMsg:
		return m.applyTaskTransitionResult(msg)
	case lastActiveMsg:
		return m.applyLastActive(msg), nil
	}
	return m, nil
}

// handleKey routes a keypress to whichever modal owns the keyboard right
// now, innermost first: the composer (the bottom-line send/reply), then the
// nudge y/n confirmation, then the '/' filter's own edit box, then the '?'
// help overlay (any key closes it) — each of these owns EVERY key while
// active. Only once none of them are active does a keypress reach the base
// navigation vocabulary.
func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch {
	case m.composer.phase != composerClosed:
		return m.handleComposerKey(msg)
	case m.taskTransition.open:
		return m.handleTaskTransitionKey(msg)
	case m.deregisterConfirmAlias != "":
		return m.handleDeregisterConfirmKey(msg)
	case m.retractConfirmThread != 0:
		return m.handleRetractStandingConfirmKey(msg)
	case m.nudgeConfirmAlias != "":
		return m.handleNudgeConfirmKey(msg)
	case m.filter.editing:
		return m.handleFilterKey(msg)
	case m.helpOpen:
		m.helpOpen = false
		return m, nil
	}
	switch {
	case key.Matches(msg, keys.Quit):
		m.quitting = true
		return m, tea.Quit
	case key.Matches(msg, keys.Help):
		m.helpOpen = true
		return m, nil
	case key.Matches(msg, keys.Down):
		return m.moveCurrentList(1)
	case key.Matches(msg, keys.Up):
		return m.moveCurrentList(-1)
	case key.Matches(msg, keys.End):
		return m.handleEndKey()
	case key.Matches(msg, keys.Enter):
		return m.handleEnterKey()
	case key.Matches(msg, keys.Esc):
		return m.popFrame(), nil
	case key.Matches(msg, keys.Home):
		return m.popToHome(), nil
	case key.Matches(msg, keys.Send):
		return m.openComposerSend(), nil
	case key.Matches(msg, keys.Reply):
		return m.handleReplyKey()
	case key.Matches(msg, keys.Nudge):
		return m.handleNudgeKey(), nil
	case key.Matches(msg, keys.Deregister):
		return m.handleDeregisterKey(), nil
	case key.Matches(msg, keys.ClearInbox):
		return m.handleClearInboxKey()
	case key.Matches(msg, keys.RetractStanding):
		return m.handleRetractStandingKey(), nil
	case key.Matches(msg, keys.Transition):
		return m.handleTaskTransitionKey(msg)
	case key.Matches(msg, keys.MailJump):
		return m.handleMailJumpKey()
	case key.Matches(msg, keys.Filter):
		if m.screen == screenRead {
			return m, nil // nothing meaningful to filter while reading
		}
		return m.openFilter(), nil
	case key.Matches(msg, keys.Aliases):
		m.opts.Aliases = !m.opts.Aliases
		m.activity.SetAliases(m.opts.Aliases)
		return m, nil
	}
	return m, nil
}

// pushFrame drills/opens: appends f to the stack and updates m.screen to
// match its top (spec §5-LOCK decision B: "Enter pushes").
func (m Model) pushFrame(f navFrame) Model {
	m.stack = append(m.stack, f)
	m.screen = f.screen
	return m
}

// canPop reports whether Esc has anywhere to go: false at the true root
// (stack length 1), and false at L1 on a single-project bus (stack length 2,
// singleProject) — there is no L0 to climb back to there, mirroring the
// auto-skip that put the operator at L1 in the first place.
func (m Model) canPop() bool {
	if len(m.stack) <= 1 {
		return false
	}
	if len(m.stack) == 2 && m.singleProject && m.stack[1].screen == screenProject {
		return false
	}
	return true
}

// popFrame implements Esc: pops exactly one frame everywhere (spec §5-LOCK
// decision B) — a no-op when canPop is false. Restoring m.project/agent/
// conversation is never necessary: a deeper screen never mutates a
// shallower one's own list cursor (mailboxSel is its own independent field
// for exactly this reason), so whatever those fields held before the push
// is still correct once it's popped back off.
func (m Model) popFrame() Model {
	if !m.canPop() {
		return m
	}
	m.stack = m.stack[:len(m.stack)-1]
	m.screen = m.stack[len(m.stack)-1].screen
	return m
}

// popToHome implements 'g': pops all the way back to the root (spec §5-LOCK
// decision B: "g jumps home") — to the auto-skipped L1 instead, on a
// single-project bus, since that IS effectively home there (mirrors
// canPop's own single-project floor).
func (m Model) popToHome() Model {
	if m.singleProject && len(m.stack) >= 2 {
		m.stack = m.stack[:2]
	} else {
		m.stack = m.stack[:1]
	}
	m.screen = m.stack[len(m.stack)-1].screen
	return m
}

// currentLeftList identifies which left list '/' filters and j/k address
// right now.
func (m Model) currentLeftList() llList {
	switch m.screen {
	case screenAgent:
		return llAgentThreads
	case screenProject:
		return llProjectItems
	case screenMailbox:
		return llMailbox
	default:
		return llProjects
	}
}

// filterQueryFor reports list's active '/' query, if any.
func (m Model) filterQueryFor(list llList) (query string, filtering bool) {
	if m.filter.list != list || m.filter.query == "" {
		return "", false
	}
	return m.filter.query, true
}

// containsFold reports whether s contains substr, case-insensitively.
func containsFold(s, substr string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(substr))
}

// projectRows returns L0's rows.
func (m Model) projectRows() []projectSummary {
	return computeProjectSummaries(m.agents, m.threads)
}

// agentStripRows returns the active project's agent list (spec §5-LOCK
// screen 3).
func (m Model) agentStripRows() []agentEnriched {
	return agentsForProject(m.agents, m.project)
}

// conversationRows returns the currently-showing thread list — the active
// agent's threads (cross-project-annotated), or the active project's threads
// (meaningful ONLY for the "(unassigned)" bucket's ORPHANED THREADS
// exception, see l1IsOrphaned — every OTHER project's screenProject shows
// agents, not this list, though the selection this still maintains is
// harmless dead bookkeeping until the operator descends into an agent).
// Always grouped action-requested-first. Never consulted while m.screen ==
// screenRead (applyThreads/moveCurrentList both gate on that first) — the
// thread being read is tracked by viewThreadID, independent of whichever
// list was showing before Enter opened it, so a background poll's regroup
// can never disturb the reader.
func (m Model) conversationRows() []conversationRow {
	aliasProj := aliasProjectMap(m.agents)
	if m.screen == screenAgent {
		return groupConversationRows(conversationsForAgentAnnotated(m.threads, aliasProj, m.agent))
	}
	return groupConversationRows(conversationsForProject(m.threads, aliasProj, m.project))
}

// plainProjectRow/plainConvRow/plainMailboxRow are the FILTER/SELECTION
// predicate's plain (unpadded) row text — plainAgentRow is renderRosterRow
// itself (views.go), reused verbatim for the agent list.
func (m Model) plainProjectRow(p projectSummary) string {
	return fmt.Sprintf("%s %d/%d live (%d unread, %d action)", projectDisplayName(p.Name), p.Live, p.Total, p.Unread, p.ActionUnread)
}

func (m Model) plainConvRow(c conversationRow) string {
	return m.renderThreadRow(c.listThreadRow)
}

func (m Model) plainMailboxRow(row listThreadRow) string {
	return fmt.Sprintf("%s %s %s %s", intentWord(row.Intent), row.Subject, m.dispLabel(row.FromAgent), relativeAge(time.Now(), row.LastAt))
}

// refreshProjectItemsSelection defaults screenProject's L1 selection on a
// drill-IN: for a normal project, that's the agents-list default
// (refreshAgentStripSelection); the "(unassigned)" bucket's ORPHANED THREADS
// exception (l1IsOrphaned) has no agents list to default, so it's left alone
// here — its own thread selection instead defaults via
// refreshConversationSelection.
func (m Model) refreshProjectItemsSelection() Model {
	if m.l1IsOrphaned() {
		return m
	}
	return m.refreshAgentStripSelection()
}

// mailboxRows returns EVERY thread addressed to station's own alias
// (to_kind=agent AND to_target=station's alias), newest first — list_threads
// already returns rows in updated_at DESC order, so no re-sort is needed
// (spec §5-LOCK screen 2: "ALL threads addressed to station... newest
// first"). Unlike the pre-lock "FOR YOU" section, this is the FULL mailbox:
// read rows are kept, never filtered out.
func (m Model) mailboxRows() []listThreadRow {
	var out []listThreadRow
	for _, row := range m.threads {
		if row.ToKind != "agent" || row.ToTarget != m.opts.Alias {
			continue
		}
		out = append(out, row)
	}
	return out
}

// isMailUnread reports whether row counts as unread in the mailbox: the last
// entry wasn't written by station itself, and station hasn't already
// acknowledged it this run (ackedThreads) — the SAME predicate that gates
// operatorInboxCount, so the header badge and the mailbox's own unread rows
// can never disagree (spec §5-LOCK item 3).
func (m Model) isMailUnread(row listThreadRow) bool {
	if row.LastFrom == "" || row.LastFrom == m.opts.Alias {
		return false
	}
	return !m.ackedThreads[row.ID]
}

// operatorInboxCount is station's OWN canonical unread-mail count (spec
// §5-LOCK item 3): the single source behind the header badge AND the
// mailbox's own bright/dim row rendering — literally the same isMailUnread
// predicate counted here and checked there, so they can never disagree.
func (m Model) operatorInboxCount() int {
	n := 0
	for _, row := range m.mailboxRows() {
		if m.isMailUnread(row) {
			n++
		}
	}
	return n
}

// refreshMailboxSelection defaults the mailbox list's own cursor.
func (m Model) refreshMailboxSelection() Model {
	rows := m.mailboxRows()
	switch {
	case len(rows) == 0:
		m.mailboxSel = 0
	case keyIndex(rows, mailKey, m.mailboxSel) < 0:
		m.mailboxSel = rows[0].ID
	}
	return m
}

// moveCurrentList applies a j/k (delta=+1/-1) move to whichever list (or the
// thread reader) the current screen owns.
func (m Model) moveCurrentList(delta int) (tea.Model, tea.Cmd) {
	switch m.screen {
	case screenProjects:
		rows := m.projectRows()
		q, f := m.filterQueryFor(llProjects)
		m.project = moveSelection(rows, projKey, m.project, delta, m.plainProjectRow, q, f, "")
		return m, nil
	case screenProject:
		if m.l1IsOrphaned() {
			rows := m.conversationRows()
			q, f := m.filterQueryFor(llProjectItems)
			newSel := moveSelection(rows, convKey, m.conversation, delta, m.plainConvRow, q, f, 0)
			if newSel == m.conversation {
				return m, nil
			}
			m.conversation = newSel
			var cmd tea.Cmd
			m, cmd = m.conversationPreviewCmd(m.conversation, false)
			return m, cmd
		}
		rows := m.agentStripRows()
		q, f := m.filterQueryFor(llProjectItems)
		m.agent = moveSelection(rows, agentKey, m.agent, delta, m.renderRosterRow, q, f, "")
		return m, nil
	case screenAgent:
		rows := m.conversationRows()
		q, f := m.filterQueryFor(llAgentThreads)
		newSel := moveSelection(rows, convKey, m.conversation, delta, m.plainConvRow, q, f, 0)
		if newSel == m.conversation {
			return m, nil
		}
		m.conversation = newSel
		var cmd tea.Cmd
		m, cmd = m.conversationPreviewCmd(m.conversation, false)
		return m, cmd
	case screenMailbox:
		rows := m.mailboxRows()
		q, f := m.filterQueryFor(llMailbox)
		m.mailboxSel = moveSelection(rows, mailKey, m.mailboxSel, delta, m.plainMailboxRow, q, f, 0)
		return m, nil
	case screenRead:
		return m.scrollConversation(delta)
	}
	return m, nil
}

// scrollConversation applies a j/k move within the thread reader: k/up
// scrolls toward older entries, lazily fetching the next older get_thread
// page when the loaded window's top is reached; j/down scrolls toward newer
// entries within what's already loaded.
func (m Model) scrollConversation(delta int) (Model, tea.Cmd) {
	if delta < 0 {
		if m.viewCursor > 0 {
			m.viewCursor--
			return m, nil
		}
		if m.viewOffset > 0 && !m.viewLoading {
			m.viewLoading = true
			return m, fetchThreadPageCmd(m.caller, m.viewThreadID, 0, m.viewOffset, true, false, m.viewGen)
		}
		return m, nil
	}
	if m.viewCursor < len(m.viewEntries)-1 {
		m.viewCursor++
	}
	return m, nil
}

// handleEndKey implements End/G: fetches the tail when newer entries have
// arrived since this window loaded (viewNewerCount). Everywhere else it's a
// no-op.
func (m Model) handleEndKey() (tea.Model, tea.Cmd) {
	if m.screen != screenRead {
		return m, nil
	}
	if n := m.viewNewerCount(); n > 0 && !m.viewLoading {
		limit := int64(threadViewPageSize)
		offset := m.viewTotal + n - limit
		if offset < 0 {
			offset = 0
		}
		m.viewLoading = true
		return m, fetchThreadPageCmd(m.caller, m.viewThreadID, offset, limit, false, false, m.viewGen)
	}
	return m, nil
}

// viewNewerCount returns how many entries exist beyond the currently loaded
// tail, per the freshest list_threads poll's entry_count for this thread —
// 0 while nothing has loaded yet, or once nothing is known to be missing.
func (m Model) viewNewerCount() int64 {
	if m.viewLoading || len(m.viewEntries) == 0 {
		return 0
	}
	idx := indexOfThread(m.threads, m.viewThreadID)
	if idx < 0 {
		return 0
	}
	n := int64(m.threads[idx].EntryCount) - m.viewTotal
	if n < 0 {
		return 0
	}
	return n
}

// handleEnterKey implements Enter (spec §5-LOCK decision B: "Enter pushes
// (drill/open)"). At each list target, a selection currently hidden by an
// active '/' filter is only SNAPPED to the nearest visible row rather than
// acted on — the next Enter (now landing on a visible row) drills normally.
func (m Model) handleEnterKey() (tea.Model, tea.Cmd) {
	switch m.screen {
	case screenProjects:
		rows := m.projectRows()
		q, f := m.filterQueryFor(llProjects)
		if !anyRowVisible(rows, m.plainProjectRow, q, f) {
			return m, nil
		}
		if !selectionVisible(rows, projKey, m.project, m.plainProjectRow, q, f) {
			m.project = snapSelection(rows, projKey, m.project, m.plainProjectRow, q, f, "")
			return m, nil
		}
		m = m.pushFrame(navFrame{screen: screenProject, project: m.project})
		m.everNavigated = true
		m = m.refreshProjectItemsSelection()
		return m.refreshConversationSelection()

	case screenProject:
		if m.l1IsOrphaned() {
			// The "(unassigned)" bucket's ORPHANED THREADS exception: L1 is a
			// thread list directly, with no agent to descend through first, so
			// Enter reads it full-width right here.
			rows := m.conversationRows()
			q, f := m.filterQueryFor(llProjectItems)
			if !anyRowVisible(rows, m.plainConvRow, q, f) {
				return m, nil
			}
			if !selectionVisible(rows, convKey, m.conversation, m.plainConvRow, q, f) {
				m.conversation = snapSelection(rows, convKey, m.conversation, m.plainConvRow, q, f, 0)
				return m, nil
			}
			return m.openReadFromList()
		}
		// Every other project's L1 is agents ONLY — Enter always descends
		// into the selected agent's own thread list.
		rows := m.agentStripRows()
		q, f := m.filterQueryFor(llProjectItems)
		if !anyRowVisible(rows, m.renderRosterRow, q, f) {
			return m, nil
		}
		if !selectionVisible(rows, agentKey, m.agent, m.renderRosterRow, q, f) {
			m.agent = snapSelection(rows, agentKey, m.agent, m.renderRosterRow, q, f, "")
			return m, nil
		}
		m = m.pushFrame(navFrame{screen: screenAgent, project: m.project, agent: m.agent})
		return m.refreshConversationSelection()

	case screenAgent:
		rows := m.conversationRows()
		q, f := m.filterQueryFor(llAgentThreads)
		if !anyRowVisible(rows, m.plainConvRow, q, f) {
			return m, nil
		}
		if !selectionVisible(rows, convKey, m.conversation, m.plainConvRow, q, f) {
			m.conversation = snapSelection(rows, convKey, m.conversation, m.plainConvRow, q, f, 0)
			return m, nil
		}
		return m.openReadFromList()

	case screenMailbox:
		rows := m.mailboxRows()
		q, f := m.filterQueryFor(llMailbox)
		if !anyRowVisible(rows, m.plainMailboxRow, q, f) {
			return m, nil
		}
		if !selectionVisible(rows, mailKey, m.mailboxSel, m.plainMailboxRow, q, f) {
			m.mailboxSel = snapSelection(rows, mailKey, m.mailboxSel, m.plainMailboxRow, q, f, 0)
			return m, nil
		}
		return m.openReadFromMailbox()
	}
	return m, nil // screenRead: already the deepest level
}

// refreshAgentStripSelection re-derives the active project's agent-list
// selection, defaulting to its first row when the previous selection doesn't
// belong to THIS project.
func (m Model) refreshAgentStripSelection() Model {
	strip := m.agentStripRows()
	switch {
	case len(strip) == 0:
		m.agent = ""
	case keyIndex(strip, agentKey, m.agent) < 0:
		m.agent = strip[0].Alias
	}
	return m
}

// refreshConversationSelection re-derives the current screen's thread list
// and defaults the selection to its first row when the previous selection no
// longer applies — then kicks off that thread's preview fetch.
func (m Model) refreshConversationSelection() (Model, tea.Cmd) {
	rows := m.conversationRows()
	if len(rows) == 0 {
		m.conversation = 0
		return m, nil
	}
	if keyIndex(rows, convKey, m.conversation) < 0 {
		m.conversation = rows[0].ID
	}
	return m.conversationPreviewCmd(m.conversation, false)
}

// maybeAckThread fires the ONE side-effecting read anywhere in station:
// open-to-acknowledge. If threadID is addressed to station's OWN registered
// alias, this issues exactly one get_inbox for that alias — never on
// selection, preview, or a later poll — and never twice for the same thread
// within this run (ackedThreads dedupes a repeated open of the same thread).
func (m Model) maybeAckThread(threadID int64) (Model, tea.Cmd) {
	idx := indexOfThread(m.threads, threadID)
	if idx < 0 {
		return m, nil
	}
	row := m.threads[idx]
	if row.ToKind != "agent" || row.ToTarget != m.opts.Alias || m.ackedThreads[row.ID] {
		return m, nil
	}
	if m.ackedThreads == nil {
		m.ackedThreads = map[int64]bool{}
	}
	m.ackedThreads[row.ID] = true
	return m, fetchInboxAckCmd(m.caller, m.opts.Alias, m.opts.SocketPath, m.opts.SessionID, m.opts.SessionCreated)
}

// openReadFromList pushes the Read frame from the currently selected
// thread-list row (screenAgent's own threads, or the "(unassigned)" bucket's
// ORPHANED THREADS exception at screenProject) — the preview is already
// warm (moveCurrentList keeps it live on every cursor move), so this only
// needs to fire the open-to-acknowledge check.
func (m Model) openReadFromList() (tea.Model, tea.Cmd) {
	m = m.pushFrame(navFrame{screen: screenRead})
	return m.maybeAckThread(m.conversation)
}

// openReadFromMailbox pushes the Read frame from the mailbox's own selected
// row — the mailbox has no live preview pane (it renders full-width, single
// column), so this loads the thread explicitly rather than relying on a
// preview fetch that never happened.
func (m Model) openReadFromMailbox() (tea.Model, tea.Cmd) {
	threadID := m.mailboxSel
	m = m.pushFrame(navFrame{screen: screenRead})
	var previewCmd, ackCmd tea.Cmd
	m, previewCmd = m.conversationPreviewCmd(threadID, false)
	m, ackCmd = m.maybeAckThread(threadID)
	return m, tea.Batch(previewCmd, ackCmd)
}

// handleMailJumpKey implements 'm': pushes the mailbox page (spec §5-LOCK
// screen 2), or — a second press while it's already the top frame — POPS it
// back to the exact origin, exactly like Esc, so 'm' toggles rather than
// stacking duplicate mailbox frames.
func (m Model) handleMailJumpKey() (tea.Model, tea.Cmd) {
	if m.screen == screenMailbox {
		return m.popFrame(), nil
	}
	m = m.pushFrame(navFrame{screen: screenMailbox})
	m = m.refreshMailboxSelection()
	return m, nil
}

// handleReplyKey implements 'r': replies to whichever thread is currently
// selected/open, wherever that is (spec §5-LOCK: reply is available directly
// from a thread LIST row — the agents-page threads table, the mailbox — not
// only while actually reading one).
func (m Model) handleReplyKey() (tea.Model, tea.Cmd) {
	id := m.replyTargetThreadID()
	if id == 0 {
		return m, nil
	}
	return m.openComposerReply(id), nil
}

// replyTargetThreadID resolves 'r”s target thread ID for the current
// screen, or 0 if there is none.
func (m Model) replyTargetThreadID() int64 {
	switch m.screen {
	case screenRead:
		return m.viewThreadID
	case screenAgent:
		return m.conversation
	case screenProject:
		if m.l1IsOrphaned() {
			return m.conversation
		}
	case screenMailbox:
		return m.mailboxSel
	}
	return 0
}

// applyLastActive applies one fetchLastActiveCmd result. A stale generation
// (an in-flight fetch from an older tick, superseded by a newer one) or a
// fetch error is discarded without touching the cache; ts==0 (no actor event
// found within the fetch's small window) also leaves any previously-cached
// value alone rather than blanking it, since "not found in this window"
// isn't the same claim as "never active".
func (m Model) applyLastActive(msg lastActiveMsg) Model {
	if msg.gen != m.pollGen || msg.err != nil || msg.ts <= 0 {
		return m
	}
	if m.lastActive == nil {
		m.lastActive = map[string]int64{}
	}
	m.lastActive[msg.alias] = msg.ts
	return m
}

// conversationPreviewCmd loads threadID's most recent page into the right
// pane, NEVER acknowledging it (only maybeAckThread does that). forceRefresh
// re-fetches even if threadID is already loaded (used for the "keep the
// live preview fresh" tick refresh in applyThreads); otherwise a call for the
// already-loaded thread is a no-op. A fresh thread (switching away from
// whatever was previously loaded) clears the old entries immediately so the
// pane never shows a stale thread's content while the new one loads.
func (m Model) conversationPreviewCmd(threadID int64, forceRefresh bool) (Model, tea.Cmd) {
	if threadID == 0 {
		return m, nil
	}
	if !forceRefresh && m.viewThreadID == threadID {
		return m, nil
	}
	idx := indexOfThread(m.threads, threadID)
	var entryCount int
	if idx >= 0 {
		entryCount = m.threads[idx].EntryCount
	}
	limit := int64(threadViewPageSize)
	offset := int64(entryCount) - limit
	if offset < 0 {
		offset = 0
	}
	freshThread := m.viewThreadID != threadID
	m.viewThreadID = threadID
	m.viewGen++
	m.viewLoading = true
	if freshThread {
		m.viewEntries = nil
		m.viewOffset = offset
		m.viewTotal = 0
		m.viewCursor = 0
	}
	return m, fetchThreadPageCmd(m.caller, threadID, offset, limit, false, false, m.viewGen)
}

// clearThreadView blanks the thread reader entirely — used when the current
// screen's thread list becomes empty (spec §5-LOCK screen 4: "an empty list
// draws an empty preview, never stale content from the last thing you
// looked at").
func (m Model) clearThreadView() Model {
	m.viewThreadID = 0
	m.viewEntries = nil
	m.viewOffset = 0
	m.viewTotal = 0
	m.viewCursor = 0
	return m
}

func (m Model) selectedTask() (listThreadRow, bool) {
	var id int64
	switch m.screen {
	case screenRead:
		id = m.viewThreadID
	case screenMailbox:
		id = m.mailboxSel
	case screenAgent:
		id = m.conversation
	case screenProject:
		if m.l1IsOrphaned() {
			id = m.conversation
		}
	}
	idx := indexOfThread(m.threads, id)
	if idx < 0 || m.threads[idx].Kind != "task" {
		return listThreadRow{}, false
	}
	return m.threads[idx], true
}

func (m Model) handleTaskTransitionKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if !m.taskTransition.open {
		thread, ok := m.selectedTask()
		if !ok {
			return m, nil
		}
		note := textinput.New()
		note.Placeholder = "optional note"
		choice := 0
		for choice < len(taskTransitionChoices) && taskTransitionChoiceDisabled(choice, thread.Status) {
			choice++
		}
		if choice == len(taskTransitionChoices) {
			choice = 0
		}
		m.taskTransition = taskTransitionState{open: true, threadID: thread.ID, currentStatus: thread.Status, choice: choice, note: note}
		return m, nil
	}
	if m.taskTransition.editingNote {
		switch msg.String() {
		case "esc":
			m.taskTransition = taskTransitionState{}
			return m, nil
		case "enter":
			choice := taskTransitionChoices[m.taskTransition.choice]
			threadID, note := m.taskTransition.threadID, m.taskTransition.note.Value()
			m.taskTransition = taskTransitionState{}
			m.status = fmt.Sprintf("updating task #%d…", threadID)
			return m, taskTransitionCmd(m.caller, m.opts.Alias, threadID, choice.action, choice.status, note)
		default:
			var cmd tea.Cmd
			m.taskTransition.note, cmd = m.taskTransition.note.Update(msg)
			return m, cmd
		}
	}
	switch msg.String() {
	case "esc":
		m.taskTransition = taskTransitionState{}
	case "j", "down":
		m.moveTaskTransitionChoice(1)
	case "k", "up":
		m.moveTaskTransitionChoice(-1)
	case "enter":
		if !taskTransitionChoiceDisabled(m.taskTransition.choice, m.taskTransition.currentStatus) {
			m.taskTransition.editingNote = true
			m.taskTransition.note.Focus()
			return m, textinput.Blink
		}
	}
	return m, nil
}

func (m *Model) moveTaskTransitionChoice(delta int) {
	for range taskTransitionChoices {
		m.taskTransition.choice = (m.taskTransition.choice + delta + len(taskTransitionChoices)) % len(taskTransitionChoices)
		if !taskTransitionChoiceDisabled(m.taskTransition.choice, m.taskTransition.currentStatus) {
			return
		}
	}
}

func (m Model) applyTaskTransitionResult(msg taskTransitionResultMsg) (Model, tea.Cmd) {
	if msg.err != nil {
		m.status = fmt.Sprintf("task #%d update failed: %v", msg.threadID, msg.err)
		return m, nil
	}
	m.status = fmt.Sprintf("task #%d → %s", msg.threadID, strings.ReplaceAll(msg.status, "_", " "))
	cmds := []tea.Cmd{fetchThreadsCmd(m.caller)}
	if m.screen == screenRead && m.viewThreadID == msg.threadID {
		cmds = append(cmds, fetchThreadPageCmd(m.caller, m.viewThreadID, m.viewOffset, int64(threadViewPageSize), false, false, m.viewGen))
	}
	return m, tea.Batch(cmds...)
}

func (m Model) handleDeregisterKey() Model {
	switch {
	case m.screen == screenProject && !m.l1IsOrphaned():
		rows := m.agentStripRows()
		q, f := m.filterQueryFor(llProjectItems)
		if !selectionVisible(rows, agentKey, m.agent, m.renderRosterRow, q, f) {
			m.agent = snapSelection(rows, agentKey, m.agent, m.renderRosterRow, q, f, "")
			if m.agent == "" {
				m.status = "no agent visible — adjust or clear the filter"
			}
			return m
		}
	case m.screen == screenAgent:
		if m.agent == "" {
			return m
		}
	default:
		return m
	}
	if m.agent == m.opts.Alias {
		m.status = "that's you — Station cannot deregister itself"
		return m
	}
	agent, found := m.agentByAlias(m.agent)
	if !found || agent.Departed {
		m.status = "agent is already departed"
		return m
	}
	m.deregisterConfirmAlias = agent.Alias
	return m
}

func (m Model) handleDeregisterConfirmKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	alias := m.deregisterConfirmAlias
	m.deregisterConfirmAlias = ""
	if msg.String() != "y" {
		return m, nil
	}
	m.status = fmt.Sprintf("deregistering %s…", m.dispLabel(alias))
	return m, deregisterCmd(m.caller, alias)
}

func (m Model) applyDeregisterResult(msg deregisterResultMsg) (Model, tea.Cmd) {
	if msg.err != nil {
		m.status = fmt.Sprintf("deregister %s failed: %v", m.dispLabel(msg.alias), msg.err)
		return m, nil
	}
	m.status = fmt.Sprintf("deregistered %s — history and read state preserved", m.dispLabel(msg.alias))
	return m, fetchAgentsCmd(m.caller)
}

// isLiveStanding reports whether row is a standing broadcast still greeting new
// sessions — the only kind of thread 'R' acts on or station marks.
func isLiveStanding(row listThreadRow) bool {
	return row.ToKind == "broadcast" && row.Standing && !row.StandingRetracted
}

// handleRetractStandingKey implements 'R': ask to retract the selected/open
// thread (replyTargetThreadID — the same target 'r' uses) when it is a live
// standing broadcast. Anything else gets a status line, not a confirmation.
func (m Model) handleRetractStandingKey() Model {
	id := m.replyTargetThreadID()
	if id == 0 {
		return m
	}
	idx := indexOfThread(m.threads, id)
	if idx < 0 || !isLiveStanding(m.threads[idx]) {
		m.status = fmt.Sprintf("#%d is not a live standing broadcast", id)
		return m
	}
	m.retractConfirmThread = id
	return m
}

func (m Model) handleRetractStandingConfirmKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	id := m.retractConfirmThread
	m.retractConfirmThread = 0
	if msg.String() != "y" {
		return m, nil
	}
	m.status = fmt.Sprintf("retracting standing broadcast #%d…", id)
	return m, retractStandingCmd(m.caller, m.opts.Alias, id)
}

func (m Model) applyRetractStandingResult(msg retractStandingResultMsg) (Model, tea.Cmd) {
	switch {
	case msg.err != nil:
		m.status = fmt.Sprintf("retract #%d failed: %v", msg.threadID, msg.err)
		return m, nil
	case msg.changed:
		m.status = fmt.Sprintf("retracted standing broadcast #%d — it greets no new session; history stays", msg.threadID)
	default:
		m.status = fmt.Sprintf("#%d was already retracted", msg.threadID)
	}
	return m, fetchThreadsCmd(m.caller)
}

// handleClearInboxKey implements 'c': mark the selected agent's inbox read and
// clear its 📬 badge — the operator drain a stale mailbox needs, distinct from
// 'd' deregister (which tombstones the agent). Valid on the agents list or an
// agent page, a no-op elsewhere. Non-destructive (mail stays in history), so
// unlike deregister it fires immediately with no confirm gate. Station MAY
// clear its OWN badge, unlike nudge/deregister. Mirrors handleNudgeKey's
// filter/selection-desync guard.
func (m Model) handleClearInboxKey() (tea.Model, tea.Cmd) {
	switch {
	case m.screen == screenProject && !m.l1IsOrphaned():
		rows := m.agentStripRows()
		q, f := m.filterQueryFor(llProjectItems)
		if !selectionVisible(rows, agentKey, m.agent, m.renderRosterRow, q, f) {
			m.agent = snapSelection(rows, agentKey, m.agent, m.renderRosterRow, q, f, "")
			if m.agent == "" {
				m.status = "no agent visible — adjust or clear the filter"
			}
			return m, nil
		}
	case m.screen == screenAgent:
		if m.agent == "" {
			return m, nil
		}
	default:
		return m, nil
	}
	if m.agent == "" {
		return m, nil
	}
	m.status = fmt.Sprintf("clearing %s…", m.dispLabel(m.agent))
	return m, markReadCmd(m.caller, m.agent)
}

func (m Model) applyMarkReadResult(msg markReadResultMsg) (Model, tea.Cmd) {
	if msg.err != nil {
		m.status = fmt.Sprintf("clear %s failed: %v", m.dispLabel(msg.alias), msg.err)
		return m, nil
	}
	if msg.cleared == 0 {
		m.status = fmt.Sprintf("%s already clear", m.dispLabel(msg.alias))
	} else {
		m.status = fmt.Sprintf("cleared %d unread from %s", msg.cleared, m.dispLabel(msg.alias))
	}
	return m, fetchAgentsCmd(m.caller)
}

// handleNudgeKey implements 'n': valid when L1's agents list is showing
// (nudges the selected agent — never the "(unassigned)" bucket's ORPHANED
// THREADS exception, which has no agents to nudge) or when a whole agent
// page (screenAgent) is showing (nudges that page's own agent) — a no-op
// anywhere else. Two guards mirror carried-over fixes: filter/selection
// desync (a hidden selection is only snapped, never acted on) and self-nudge
// (station's own row never opens the confirm gate).
func (m Model) handleNudgeKey() Model {
	switch {
	case m.screen == screenProject && !m.l1IsOrphaned():
		rows := m.agentStripRows()
		q, f := m.filterQueryFor(llProjectItems)
		if !selectionVisible(rows, agentKey, m.agent, m.renderRosterRow, q, f) {
			m.agent = snapSelection(rows, agentKey, m.agent, m.renderRosterRow, q, f, "")
			if m.agent == "" {
				m.status = "no agent visible — adjust or clear the filter"
			}
			return m
		}
	case m.screen == screenAgent:
		if m.agent == "" {
			return m
		}
	default:
		return m
	}
	if m.agent == m.opts.Alias {
		m.status = "that's you — can't nudge yourself"
		return m
	}
	m.nudgeConfirmAlias = m.agent
	return m
}

// handleNudgeConfirmKey owns every key while nudgeConfirmAlias is pending:
// 'y' confirms and issues the nudge; anything else (including 'n' itself,
// and Esc) cancels without nudging.
func (m Model) handleNudgeConfirmKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	alias := m.nudgeConfirmAlias
	m.nudgeConfirmAlias = ""
	if msg.String() != "y" {
		return m, nil
	}
	m.status = fmt.Sprintf("nudging %s…", m.dispLabel(alias))
	return m, nudgeCmd(m.caller, m.nudger, alias)
}

// applyNudgeResult applies one nudgeCmd outcome to the status line — success
// and failure both land there.
func (m Model) applyNudgeResult(msg nudgeResultMsg) Model {
	label := m.dispLabel(msg.alias)
	if msg.err != nil {
		m.status = fmt.Sprintf("nudge %s failed: %v", label, msg.err)
		return m
	}
	word := "typed"
	if msg.submitted {
		word = "submitted"
	}
	m.status = fmt.Sprintf("nudged %s (%s)", label, word)
	return m
}

// openFilter opens '/' editing for the current left list: re-opening the
// SAME list's filter preserves its existing query for editing; switching to
// a different list starts blank.
func (m Model) openFilter() Model {
	list := m.currentLeftList()
	ti := textinput.New()
	ti.Placeholder = "filter…"
	ti.Focus()
	if m.filter.list == list {
		ti.SetValue(m.filter.query)
	}
	m.filter = filterState{list: list, query: ti.Value(), editing: true, input: ti}
	return m
}

// handleFilterKey owns every key while the '/' filter box is being edited:
// Esc clears the filter entirely (back to showing everything); Enter stops
// editing but leaves the last-typed query applied; anything else is handed
// to the textinput, and the live query is synced from it on every keystroke
// so filtering updates as the operator types.
func (m Model) handleFilterKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.filter = filterState{}
		return m, nil
	case tea.KeyEnter:
		m.filter.editing = false
		return m, nil
	}
	var cmd tea.Cmd
	m.filter.input, cmd = m.filter.input.Update(msg)
	m.filter.query = m.filter.input.Value()
	return m, cmd
}

// openComposerSend opens the 's' composer: the roster-filtered target picker
// phase, before the body input. Valid from anywhere.
func (m Model) openComposerSend() Model {
	ti := textinput.New()
	ti.Placeholder = "filter roster (label or alias)…"
	ti.Focus()
	m.composer = composerState{phase: composerPickingTarget, kind: composerKindSend, filter: ti}
	return m
}

// openComposerReply opens the 'r' composer straight into body-editing — no
// target picker, the target is threadID.
func (m Model) openComposerReply(threadID int64) Model {
	ti := textinput.New()
	ti.Placeholder = "reply…"
	ti.Focus()
	m.composer = composerState{phase: composerEditingBody, kind: composerKindReply, threadID: threadID, input: ti}
	return m
}

// composerCandidates returns the roster rows matching the target picker's
// current filter text (label or alias substring), excluding station's own
// alias (it can't message itself).
func (m Model) composerCandidates() []agentEnriched {
	q := strings.ToLower(strings.TrimSpace(m.composer.filter.Value()))
	var out []agentEnriched
	for _, a := range m.agents {
		if a.Alias == m.opts.Alias {
			continue
		}
		if q == "" || strings.Contains(strings.ToLower(a.Alias), q) || strings.Contains(strings.ToLower(m.dispLabel(a.Alias)), q) {
			out = append(out, a)
		}
	}
	return out
}

// handleComposerKey routes to whichever composer phase is active.
func (m Model) handleComposerKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.composer.phase {
	case composerPickingTarget:
		return m.handleComposerPickerKey(msg)
	case composerEditingBody:
		return m.handleComposerBodyKey(msg)
	}
	return m, nil
}

// handleComposerPickerKey owns the keys during the 's' target-picker phase:
// Esc cancels the whole composer; Up/Down move the highlighted candidate;
// Enter selects it and advances to body-editing; every other key is handed
// to the filter textinput, narrowing the candidate list live.
func (m Model) handleComposerPickerKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.composer = composerState{}
		return m, nil
	case tea.KeyEnter:
		cands := m.composerCandidates()
		if len(cands) == 0 {
			return m, nil
		}
		idx := m.composer.pickerIdx
		if idx < 0 || idx >= len(cands) {
			idx = 0
		}
		m.composer.target = cands[idx].Alias
		m.composer.phase = composerEditingBody
		ti := textinput.New()
		ti.Placeholder = "message…"
		ti.Focus()
		m.composer.input = ti
		return m, nil
	case tea.KeyUp:
		if m.composer.pickerIdx > 0 {
			m.composer.pickerIdx--
		}
		return m, nil
	case tea.KeyDown:
		if n := len(m.composerCandidates()); m.composer.pickerIdx < n-1 {
			m.composer.pickerIdx++
		}
		return m, nil
	}
	var cmd tea.Cmd
	m.composer.filter, cmd = m.composer.filter.Update(msg)
	if n := len(m.composerCandidates()); m.composer.pickerIdx >= n {
		m.composer.pickerIdx = max(n-1, 0)
	}
	return m, cmd
}

// handleComposerBodyKey owns the keys during body-editing (both composer
// kinds): Esc cancels; Enter submits (submitComposer); CycleIntent (tab)
// advances the F/R/A indicator; everything else is handed to the body
// textinput.
func (m Model) handleComposerBodyKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch {
	case msg.Type == tea.KeyEsc:
		m.composer = composerState{}
		return m, nil
	case msg.Type == tea.KeyEnter:
		return m.submitComposer()
	case key.Matches(msg, keys.CycleIntent):
		m.composer.intent = m.composer.intent.next()
		return m, nil
	}
	var cmd tea.Cmd
	m.composer.input, cmd = m.composer.input.Update(msg)
	return m, cmd
}

// submitComposer sends the composer's body via the same send_message/reply
// ops the CLI uses, closing the composer immediately (op errors —
// applyComposerSent — land on the status line, never leaving the operator
// stuck re-typing into a composer whose op already fired).
func (m Model) submitComposer() (Model, tea.Cmd) {
	body := strings.TrimSpace(m.composer.input.Value())
	if body == "" {
		m.composer.err = "cannot send an empty message"
		return m, nil
	}
	from := m.opts.Alias
	kind := m.composer.kind
	target := m.composer.target
	threadID := m.composer.threadID
	intentWire := m.composer.intent.wire()
	label := m.dispLabel(target)
	m.composer = composerState{}
	switch kind {
	case composerKindReply:
		m.status = fmt.Sprintf("replying on #%d…", threadID)
		return m, replyCmd(m.caller, from, threadID, body)
	default: // composerKindSend
		m.status = fmt.Sprintf("sending to %s…", label)
		return m, sendMessageCmd(m.caller, from, target, body, intentWire)
	}
}

// applyComposerSent applies one composer submit's outcome to the status
// line.
func (m Model) applyComposerSent(msg composerSentMsg) Model {
	verb := "sent"
	if msg.kind == composerKindReply {
		verb = "reply sent"
	}
	if msg.err != nil {
		m.status = fmt.Sprintf("%s failed: %v", verb, msg.err)
		return m
	}
	m.status = verb
	return m
}

// applyEvents is the ONLY place the cursor advances, and only once a page is
// actually applied: a failed fetch (err != nil) leaves the cursor and the
// buffered events untouched, so a dropped/failed poll can never skip events —
// the next successful poll retries in the same mode (backlog stays backlog
// until it succeeds; follow picks up from exactly the same after_id).
//
// msg.backlog is the cold-start (or bootstrap-retry) case: the daemon
// returns backlog rows NEWEST-first, so they're reversed into the
// oldest-first order the buffer and every follow-mode page use, the cursor
// is seeded straight from max_id (there is no prior cursor to regress
// against), and bootstrapped flips true so every subsequent pollCmd follows
// instead of re-fetching backlog.
//
// Otherwise this is a follow-mode page: a regression (max_id < cursor — the
// DB was replaced) resets the cursor to the new tail without applying the
// (stale) page, exactly like tail.
func (m Model) applyEvents(msg eventsMsg) Model {
	if msg.err != nil {
		what := "events"
		if msg.backlog {
			what = "events backlog"
		}
		m.status = fmt.Sprintf("%s: poll failed, retrying: %v", what, msg.err)
		return m
	}
	if msg.backlog {
		events := make([]render.EventRow, len(msg.page.Events))
		for i, e := range msg.page.Events {
			events[len(events)-1-i] = e
		}
		m.events = events
		m.cursor = msg.page.MaxID
		m.bootstrapped = true
		m.status = ""
		return m
	}
	if msg.page.MaxID < m.cursor {
		m.status = fmt.Sprintf("journal reset (max id %d < cursor %d) — following from the new tail", msg.page.MaxID, m.cursor)
		m.cursor = msg.page.MaxID
		return m
	}
	if len(msg.page.Events) > 0 {
		m.events = append(m.events, msg.page.Events...) // follow mode: oldest-first
		if n := len(m.events); n > eventBacklog {
			m.events = m.events[n-eventBacklog:]
		}
		m.status = ""
	}
	m.cursor = msg.page.MaxID
	return m
}

// applyAgents refreshes the roster and re-derives the L0/L1 selection
// defaults. A failed fetch only updates the status line — the roster keeps
// showing its last-known state rather than blanking.
func (m Model) applyAgents(msg agentsMsg) Model {
	if msg.err != nil {
		m.status = fmt.Sprintf("agents: poll failed, retrying: %v", msg.err)
		return m
	}
	m.agents = msg.rows
	labels := make(map[string]string, len(m.agents))
	for _, a := range m.agents {
		labels[a.Alias] = a.Label
	}
	m.labels = labels
	m.labelCollide = computeLabelCollisions(m.agents)
	m.aliasStripCollide = computeAliasStripCollisions(m.agents)
	m.activity.SetLabels(labels)

	projects := m.projectRows()
	m.singleProject = len(projects) == 1
	switch {
	case len(projects) == 0:
		m.project = ""
	case keyIndex(projects, projKey, m.project) < 0:
		m.project = projects[0].Name
	}
	// Single-project auto-skip, fired exactly once so a project appearing/
	// disappearing later never yanks the operator back to L0 mid-session —
	// same default as a manual L0 drill-in.
	if !m.everNavigated && len(m.stack) == 1 && len(projects) == 1 {
		m = m.pushFrame(navFrame{screen: screenProject, project: m.project})
		m = m.refreshProjectItemsSelection()
	}

	// Per-poll correction only (see refreshAgentStripSelection's doc).
	return m.refreshAgentStripSelection()
}

// applyThreads refreshes the thread-list data and re-derives the current
// screen's selection default (selection preserved BY THREAD ID across a
// poll's regroup, never an index). When the operator isn't currently reading
// (screenRead), it also re-fetches the selected thread's preview so the
// right pane's "last messages" stay live without disturbing a focused
// reader's scroll position (which instead uses End/G).
func (m Model) applyThreads(msg threadsMsg) (Model, tea.Cmd) {
	if msg.err != nil {
		m.status = fmt.Sprintf("threads: poll failed, retrying: %v", msg.err)
		return m, nil
	}
	m.threads = msg.threads
	m.activeTasksTruncated = msg.activeTasksTruncated

	// Never touched while actually reading (screenRead): the thread being
	// read is tracked by viewThreadID/m.conversation as they stood at the
	// moment Enter opened it, and must survive a background poll's regroup
	// untouched — only End/G explicitly refreshes a focused reader.
	var cmd tea.Cmd
	if m.screen != screenRead {
		rows := m.conversationRows()
		switch {
		case len(rows) == 0:
			m.conversation = 0
			m = m.clearThreadView() // spec §5-LOCK screen 4: empty list -> empty preview, never stale content
		case keyIndex(rows, convKey, m.conversation) < 0:
			m.conversation = rows[0].ID
		}
		if m.conversation != 0 && !m.viewLoading {
			m, cmd = m.conversationPreviewCmd(m.conversation, true)
		}
	}

	// Mailbox selection default (spec §5-LOCK screen 2): the SAME courtesy as
	// the thread list's own default above.
	switch mrows := m.mailboxRows(); {
	case len(mrows) == 0:
		m.mailboxSel = 0
	case keyIndex(mrows, mailKey, m.mailboxSel) < 0:
		m.mailboxSel = mrows[0].ID
	}
	return m, cmd
}

// applyThreadPage applies one get_thread page. A page that resolves after
// the reader moved on to a different thread, OR that belongs to a PREVIOUS
// load of the very same thread ID (msg.gen != m.viewGen), is discarded.
// Older pages are PREPENDED (the lazy "load older" fetch) rather than
// replacing the loaded window; viewCursor advances by the number of
// newly-prepended entries so the previously-topmost (still-visible) entry
// keeps its highlighted position instead of the view jumping.
//
// The newest-entries gap: a non-older page's live total can reveal that this
// fetch's offset guess (from a stale cached entry_count) undershot the true
// tail — total > offset+len(entries) means there are more entries beyond
// this window that should have been included. When that happens, issue
// exactly ONE corrective re-fetch at the corrected offset (msg.corrected on
// the correction's own response prevents this from ever chaining into a
// second correction).
func (m Model) applyThreadPage(msg threadPageMsg) (Model, tea.Cmd) {
	if msg.threadID != m.viewThreadID || msg.gen != m.viewGen {
		return m, nil
	}
	if msg.err != nil {
		m.status = fmt.Sprintf("thread: poll failed, retrying: %v", msg.err)
		m.viewLoading = false
		return m, nil
	}
	if msg.older {
		m.viewEntries = append(msg.entries, m.viewEntries...)
		m.viewCursor += len(msg.entries)
	} else {
		m.viewEntries = msg.entries
		if m.viewCursor > len(m.viewEntries)-1 {
			m.viewCursor = len(m.viewEntries) - 1
		}
		if m.viewCursor < 0 {
			m.viewCursor = 0
		}
	}
	m.viewOffset = msg.offset
	m.viewTotal = msg.total
	m.viewLoading = false
	m.status = ""

	if !msg.older && !msg.corrected {
		limit := int64(len(msg.entries))
		if limit > 0 && msg.total > msg.offset+limit {
			wantOffset := msg.total - limit
			if wantOffset < 0 {
				wantOffset = 0
			}
			m.viewLoading = true
			return m, fetchThreadPageCmd(m.caller, m.viewThreadID, wantOffset, limit, false, true, m.viewGen)
		}
	}
	return m, nil
}

// applyInboxAck applies the open-to-acknowledge get_inbox result: success is
// silent (the read already happened; there's nothing further to show), a
// failure lands on the status line like every other poll error.
func (m Model) applyInboxAck(msg inboxAckMsg) Model {
	if msg.err != nil {
		m.status = fmt.Sprintf("inbox ack: %v", msg.err)
	}
	return m
}

// dispLabel resolves an alias for display: the current label (or the alias
// itself when Aliases mode is on, or no label is known), with its OWN alias
// appended when computeLabelCollisions flagged this alias's label as
// ambiguous (spec §5-LOCK item 7) — the ONE shared helper every label-
// rendering call site in the package goes through, so "who → who" can never
// read as nonsense regardless of where it renders.
//
// alias is always the full, stored form (m.labels and m.labelCollide are
// keyed by it) — station is a human surface, so this machine's device
// prefix comes off wherever a raw alias would otherwise render: the
// no-label/Aliases-mode fallback, and the alias inside the "label (alias)"
// collision fallback. A label itself is never stripped; it isn't an alias.
// Both strip sites route through m.dispAlias, which forces the full,
// unstripped form when computeAliasStripCollisions flagged this alias as
// colliding with another agent's post-strip form — otherwise two different
// agents (a locally seeded "personal-relay" and a legacy bare "relay")
// could strip to the identical displayed string.
func (m Model) dispLabel(alias string) string {
	base := alias
	if !m.opts.Aliases {
		if l := m.labels[alias]; l != "" {
			base = l
		}
	}
	if base != alias && m.labelCollide[alias] {
		return base + " (" + m.dispAlias(alias) + ")"
	}
	if base == alias {
		return m.dispAlias(alias)
	}
	return base
}

// dispAlias strips this machine's device prefix off alias for display,
// UNLESS computeAliasStripCollisions flagged alias as colliding with
// another agent's post-strip form — in which case it returns alias
// untouched. This is the one place in the package that calls
// device.Strip on an alias bound for the screen; every display call site
// funnels through it via dispLabel (and renderQuitAgentLine, its one
// by-design bypass of dispLabel itself) so the collision guard can never be
// skipped by a second, parallel strip call.
func (m Model) dispAlias(alias string) string {
	if m.aliasStripCollide[alias] {
		return alias
	}
	return device.Strip(device.Name(), alias)
}

// dispToTarget renders a thread's (to_kind, to_target) pair for display:
// "agent" resolves through dispLabel, "role"/"broadcast" show as-is (they
// have no per-alias label to resolve).
func (m Model) dispToTarget(row listThreadRow) string {
	switch row.ToKind {
	case "agent":
		return m.dispLabel(row.ToTarget)
	case "role":
		return "role:" + row.ToTarget
	case "broadcast":
		if row.ToTarget != "" {
			return "broadcast:" + row.ToTarget
		}
		return "broadcast"
	default:
		return row.ToTarget
	}
}

// intentWord maps a thread's effective intent to its PLAIN-WORD rendering
// (spec §5-LOCK item 8/screen 4: "needs action"/"wants reply"/"fyi" — never
// the old "[action]"/"[reply?]"/"[fyi]" bracket shorthand, which stays only
// in the `muster events` CLI journal).
func intentWord(intent string) string {
	switch intent {
	case "action-requested":
		return "needs action"
	case "reply-requested":
		return "wants reply"
	case "fyi":
		return "fyi"
	default:
		return ""
	}
}

// groupThreads partitions rows into the three buckets — action-requested
// pinned first, then reply-requested, then everything else — via a STABLE
// three-way partition: each bucket keeps rows in the order list_threads
// already returned them (updated_at DESC, id DESC), so grouping never
// re-sorts within a bucket, only re-buckets.
func groupThreads(rows []listThreadRow) []listThreadRow {
	var action, reply, rest []listThreadRow
	for _, r := range rows {
		switch r.Intent {
		case "action-requested":
			action = append(action, r)
		case "reply-requested":
			reply = append(reply, r)
		default:
			rest = append(rest, r)
		}
	}
	out := make([]listThreadRow, 0, len(rows))
	out = append(out, action...)
	out = append(out, reply...)
	out = append(out, rest...)
	return out
}

// indexOfThread returns id's position in rows, or -1 if absent.
func indexOfThread(rows []listThreadRow, id int64) int {
	for i, r := range rows {
		if r.ID == id {
			return i
		}
	}
	return -1
}

// relativeAge renders the gap between now and atMillis (a ms-epoch
// timestamp) as a short duration ("Ns"/"Nm"/"Nh"/"Nd"). atMillis <= 0 (no
// last entry yet) renders "".
func relativeAge(now time.Time, atMillis int64) string {
	if atMillis <= 0 {
		return ""
	}
	d := now.Sub(time.UnixMilli(atMillis))
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// agentActivity filters the global events buffer down to rows concerning
// alias — a client-side filter over the SAME cursor-disciplined buffer the
// data loop already maintains, rather than a second fetch/cursor scheme.
func agentActivity(events []render.EventRow, alias string) []render.EventRow {
	var out []render.EventRow
	for _, e := range events {
		if e.Agent == alias || e.Target == "agent:"+alias {
			out = append(out, e)
		}
	}
	return out
}
