package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// tailOpts carries the loop's injectable seams. Zero value = production:
// wait sleeps interval or returns early on signal; maxPolls 0 = forever.
type tailOpts struct {
	wait     func(d time.Duration) bool // false = shutdown requested
	maxPolls int
	errw     io.Writer // stderr for retry/reset notes; nil = os.Stderr
}

// tailFlagVals holds cmdTail's parsed flag pointers.
type tailFlagVals struct {
	agent, kind       *string
	thread            *int64
	interval          *time.Duration
	backlog, width    *int
	aliases, fullTime *bool
}

// newTailFlagsWithVals declares tail's flags and returns typed access to
// their values — shared by cmdTail (real parsing) and newTailFlags
// (registry help/man rendering).
func newTailFlagsWithVals() (*flag.FlagSet, tailFlagVals) {
	fs := flag.NewFlagSet("tail", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var v tailFlagVals
	v.agent = fs.String("agent", "", "only events concerning this alias")
	v.kind = fs.String("kind", "", "only this event kind")
	v.thread = fs.Int64("thread", 0, "only this thread")
	v.interval = fs.Duration("interval", time.Second, "poll interval")
	v.backlog = fs.Int("backlog", 10, "history lines to print before following (0 = none)")
	v.aliases = fs.Bool("aliases", false, "show raw aliases instead of current labels")
	v.fullTime = fs.Bool("full-time", false, "show dates, not just times")
	v.width = fs.Int("width", 0, "line budget in columns (default $COLUMNS or 120)")
	return fs, v
}

// newTailFlags builds tail's flag.FlagSet for registry-driven help/man
// rendering.
func newTailFlags() *flag.FlagSet {
	fs, _ := newTailFlagsWithVals()
	return fs
}

// cmdTail tails the bus journal: prints the last --backlog matching events,
// then polls list_events with the max_id cursor every --interval until
// interrupted. Side-effect-free: never marks anything read.
func cmdTail(args []string, out io.Writer, o tailOpts) error {
	fs, v := newTailFlagsWithVals()
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return HelpFor("tail", out)
		}
		return fmt.Errorf("usage: muster tail [--agent <alias>] [--thread <id>] [--kind <k>] [--interval <dur>] [--backlog <n>] [--aliases] [--full-time] [--width <cols>]")
	}
	agent, kind, thread, interval, backlog, aliases, fullTime, width := v.agent, v.kind, v.thread, v.interval, v.backlog, v.aliases, v.fullTime, v.width
	if o.errw == nil {
		o.errw = os.Stderr
	}
	if o.wait == nil {
		o.wait = signalWait()
	}

	// A short local alias must expand before it reaches the daemon's exact
	// list_events filter, exactly like every other input site. Expanded once
	// up front — the roster a device name maps against does not change over
	// one tail invocation's life.
	agentArg := expandAlias(*agent, rosterAliasExists())

	page, err := fetchEvents(agentArg, *kind, *thread, -1, *backlog) // backlog mode
	if err != nil {
		return err
	}
	r := newRenderer(page.Events, loadLabels(), *aliases, *fullTime, *width)
	r.Header(out)
	for i := len(page.Events) - 1; i >= 0; i-- { // newest-first → print oldest-first
		r.Line(out, page.Events[i])
	}
	cursor := page.MaxID

	for polls := 0; o.maxPolls == 0 || polls < o.maxPolls; polls++ {
		if !o.wait(*interval) {
			return nil // interrupted
		}
		page, err := fetchEvents(agentArg, *kind, *thread, cursor, 0) // follow mode
		if err != nil {
			_, _ = fmt.Fprintln(o.errw, "tail: poll failed, retrying:", err)
			continue
		}
		if page.MaxID < cursor {
			_, _ = fmt.Fprintf(o.errw, "tail: journal reset (max id %d < cursor %d) — DB replaced? following from the new tail\n", page.MaxID, cursor)
			cursor = page.MaxID
			continue
		}
		for _, e := range page.Events { // follow mode is oldest-first
			if !*aliases && !r.HasLabel(e.Agent) && e.Agent != "" {
				// A newly-seen agent may have registered since the map was
				// loaded — refresh once so its label renders. Best-effort.
				if m := loadLabels(); m != nil {
					r.SetLabels(m)
				}
			}
			r.Line(out, e)
		}
		cursor = page.MaxID
	}
	return nil
}

// signalWait returns the production wait: sleep d, but return false
// immediately on SIGINT/SIGTERM so Ctrl-C never waits out an interval.
func signalWait() func(time.Duration) bool {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	return func(d time.Duration) bool {
		select {
		case <-sig:
			return false
		case <-time.After(d):
			return true
		}
	}
}
