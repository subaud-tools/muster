// Package serve is muster's page: the agents on the bus with their inbox
// counts, served by pageserve beside the daemon's unix socket in the same
// `muster serve` process. It reads the bus through the daemon's own ops
// (list_agents, status), so it shows the same thing whichever backend the
// daemon fronts, and the socket and its clients are untouched.
package serve

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"reflect"
	"time"

	"github.com/subaud-tools/muster/internal/proto"
	"github.com/subaud-tools/tools-common/localweb/page"
	"github.com/subaud-tools/tools-common/pageserve"
)

//go:embed assets
var assets embed.FS

// Live is where muster serve's page entry is: StateDir/live/serve.json.
func Live() pageserve.Live { return pageserve.LiveFor("muster") }

// Dispatcher answers one daemon op in this process (*daemon.Daemon).
type Dispatcher interface {
	Dispatch(proto.Request) proto.Response
}

// Agent is one row of the page: a list_agents row joined with its status
// counts.
type Agent struct {
	Alias          string `json:"alias"`
	Project        string `json:"project"`
	Label          string `json:"label"`
	Activity       string `json:"activity"`
	LastSeen       int64  `json:"last_seen"`
	Departed       bool   `json:"departed"`
	Unread         int    `json:"unread"`
	ActionRequired int    `json:"action_required"`
}

// Options configures Run.
type Options struct {
	Port    int // 0: remembered, else free
	Version string
	Log     io.Writer     // one line per start/stop
	Every   time.Duration // how often the rows are checked for a change; default 2s
}

// Run serves the page until ctx ends or POST /api/stop. pageserve writes the
// entry while running and removes it on the way out.
func Run(ctx context.Context, d Dispatcher, o Options) error {
	if o.Every <= 0 {
		o.Every = 2 * time.Second
	}
	assetsFS, err := fs.Sub(assets, "assets")
	if err != nil {
		return err
	}
	events := pageserve.NewEvents()
	return pageserve.Run(ctx, pageserve.Options{
		Live: Live(), Assets: page.With(assetsFS), API: Handler(d), Events: events,
		Port: o.Port, Version: o.Version, Log: o.Log,
		OnStart: func(ctx context.Context, _ string) { go watch(ctx, d, events, o.Every) },
	})
}

// Handler is the page's API: GET /api/agents.
func Handler(d Dispatcher) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/agents", func(w http.ResponseWriter, _ *http.Request) {
		rows, err := Agents(d)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"agents": rows})
	})
	return mux
}

// Agents reads list_agents and status and joins them by alias.
func Agents(d Dispatcher) ([]Agent, error) {
	var rows []Agent
	if err := call(d, "list_agents", &rows); err != nil {
		return nil, err
	}
	var status struct {
		Agents []struct {
			Alias          string `json:"alias"`
			Unread         int    `json:"unread"`
			ActionRequired int    `json:"action_required"`
		} `json:"agents"`
	}
	if err := call(d, "status", &status); err != nil {
		return nil, err
	}
	for _, s := range status.Agents {
		for i := range rows {
			if rows[i].Alias == s.Alias {
				rows[i].Unread, rows[i].ActionRequired = s.Unread, s.ActionRequired
			}
		}
	}
	if rows == nil {
		rows = []Agent{}
	}
	return rows, nil
}

// call runs op and decodes its data into out. Data is Go values in local mode
// and decoded JSON in remote mode, so it goes through JSON either way.
func call(d Dispatcher, op string, out any) error {
	resp := d.Dispatch(proto.Request{Op: op})
	if !resp.OK {
		return fmt.Errorf("%s: %s", op, resp.Error)
	}
	b, err := json.Marshal(resp.Data)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

// watch emits an "agents" event whenever the rows change, so an open page
// reloads them; the first read only records.
func watch(ctx context.Context, d Dispatcher, events *pageserve.Events, every time.Duration) {
	last, _ := Agents(d)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		now, err := Agents(d)
		if err != nil || reflect.DeepEqual(now, last) {
			continue
		}
		last = now
		events.Emit("agents", nil)
	}
}
