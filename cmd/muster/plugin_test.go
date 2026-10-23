package main

import (
	"path/filepath"
	"testing"

	"github.com/subaud-tools/tools-common/claudeplugin"
)

// The muster plugin is rendered by tools-common, not written by hand: its
// manifest, the .mcp.json that runs `muster channel` as muster-channel for
// muster_channel_status, and the mod that runs `muster watch` and starts a
// turn with each event. `muster mcp`, the bus's own tools, is registered
// separately and is not part of the plugin. Regenerate with
// CLAUDEPLUGIN_UPDATE=1 go test -run '^TestPluginIsCurrent$' ./cmd/muster
func TestPluginIsCurrent(t *testing.T) {
	claudeplugin.AssertCurrent(t, filepath.Join("..", "..", "plugin", "muster"), claudeplugin.Spec{
		Name:        "muster",
		DisplayName: "muster",
		Bin:         "muster",
		ServerName:  "muster-channel",
		Description: "Mail on the muster bus starts a turn in the session it was sent to.",
		Author:      "Court Schuett",
		AuthorURL:   "https://muster.tools",
		Homepage:    "https://muster.tools",
		Repository:  "https://github.com/subaud-tools/muster",
		License:     "BUSL-1.1",
		Keywords:    []string{"agents", "coordination", "tmux", "messaging"},
	})
}
