package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	tools "github.com/subaud-tools/tools-common"

	"github.com/subaud-tools/muster/internal/paths"
	"github.com/subaud-tools/muster/internal/version"
)

// about is muster's overview: `muster help` prints it above the grouped
// command list, and the man page uses it as its DESCRIPTION (paragraphs
// become .PP breaks). It carries what muster's own man renderer used to add:
// the three modes, the files, and where the docs live.
func about() string {
	home := "~/" + paths.HomeSuffix()
	return fmt.Sprintf(`muster — local multi-agent coordination bus: independent coding-agent
sessions (each in its own tmux tab) hand tasks and messages to each other
without copy/paste. It never calls a model itself; it only routes between
agents already running on their own plans.

The binary has three modes: 'muster serve' runs the daemon, a lazy
unix-socket API server every other mode talks to; 'muster mcp' runs an MCP
stdio server exposing the daemon's operations as tools; every other command
is the operator CLI, a plain client of the daemon, exactly as capable as the
MCP tools.

Files: %s is the data directory (override with $MUSTER_HOME), holding
%s (the SQLite store) and %s (the daemon's socket).

Run 'muster help <command>' for details on a command. Docs: https://muster.tools`,
		home, filepath.Base(paths.DBPath()), filepath.Base(paths.SocketPath()))
}

// NewApp builds muster's command-line app on the family's tools.App: every
// Registry row, muster's groups and overview, and the built-in help, version,
// man, commands, update, login and logout. cmd/muster's main() dispatches
// through it for every command it does not route itself.
func NewApp() *tools.App {
	app := tools.New(tools.Config{
		Name:   "muster",
		Domain: "muster.tools",
		Version: tools.Version{
			Number: version.Version(),
			Commit: version.Commit(),
			Date:   version.Date(),
		},
		Groups: groups,
		About:  about(),
	})
	for _, c := range Registry {
		if c.Run != nil {
			c.Run = withoutSelfPrefix(c.Name, c.Run)
		}
		app.Register(c)
	}
	return app
}

// withoutSelfPrefix drops a leading "<name>: " from a command's error, because
// tools.App already prints every error as "muster <name>: <msg>". The daemon
// client labels each error with its op ("become: ..."), and for ops named
// like their command that doubled into "muster become: become: ...". Usage
// and exit errors keep their types (tools.App reads their codes).
func withoutSelfPrefix(name string, run func(args []string, out, errw io.Writer) error) func(args []string, out, errw io.Writer) error {
	return func(args []string, out, errw io.Writer) error {
		err := run(args, out, errw)
		if err == nil {
			return nil
		}
		var ue tools.UsageError
		var ee *tools.ExitError
		if errors.As(err, &ue) || errors.As(err, &ee) || errors.Is(err, flag.ErrHelp) {
			return err
		}
		if msg := err.Error(); strings.HasPrefix(msg, name+": ") {
			return errors.New(strings.TrimPrefix(msg, name+": "))
		}
		return err
	}
}

// adapt lets a command written against one writer run as a tools.Command. A
// command reports failure by returning an error, which tools.App prints to
// errw with the family prefix and exit code, so the command never needs errw.
func adapt(run func(args []string, out io.Writer) error) func(args []string, out, errw io.Writer) error {
	return func(args []string, out, _ io.Writer) error { return run(args, out) }
}
