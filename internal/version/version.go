// Package version holds muster's build-time version stamp. It exists so
// exactly one pair of variables gets the -ldflags -X treatment — cmd/muster,
// the justfile, and .github/workflows/release.yml all target
// github.com/subaud-tools/muster/internal/version.{version,commit} rather than
// three copies scattered across main packages.
package version

// version, commit, and date are overwritten at build time via:
//
//	-ldflags "-X github.com/subaud-tools/muster/internal/version.version=$(cat VERSION) \
//	          -X github.com/subaud-tools/muster/internal/version.commit=$(git rev-parse --short HEAD) \
//	          -X github.com/subaud-tools/muster/internal/version.date=$(date -u +%Y-%m-%d)"
//
// A plain `go build` / `go run` (no ldflags — local dev, `go test`, an
// unstamped checkout) leaves them at these defaults, so `muster version`
// still prints something sane: "muster dev (none)".
var (
	version = "dev"
	commit  = "none"
	date    = ""
)

// Version returns the stamped version ("dev" if the binary wasn't built with
// the ldflags above).
func Version() string { return version }

// Commit returns the stamped short commit hash ("none" if unstamped).
func Commit() string { return commit }

// Date returns the stamped build date ("" if unstamped).
func Date() string { return date }

// Gated says muster.tools/dl is closed to anyone not signed in, so every
// download goes through a signed link from subaud.tools. It is the one switch
// for muster: `muster update` (tools.Config.Gated in internal/cli/app.go) and
// muster-deploy's Lambda zip fetch (internal/deploy) both read it, so flipping
// it when muster.tools is gated switches update and deploy together.
const Gated = false
