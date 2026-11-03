// Package device resolves this machine's stable muster device identity.
package device

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/subaud-tools/muster/internal/paths"
)

// FileName is the device id's filename within paths.Home().
const FileName = "device-id"

// NameFileName is the device NAME's filename within paths.Home(), and
// NameEnv the variable that overrides it.
//
// The file is the durable half on purpose. A shell export reaches only shells
// that source the profile defining it, and muster's daemon inherits whatever
// environment happened to spawn it — so an export is exactly as reliable as
// the operator's memory of which terminal started the daemon. A file under
// MUSTER_HOME survives reboots with no shell configuration at all and reads
// the same however the process was launched. The variable stays as an
// override for the cases where per-shell control is genuinely wanted.
const (
	NameFileName = "device-name"
	NameEnv      = "MUSTER_DEVICE_NAME"
)

// maxNameLen bounds a device name. It is a prefix on derived aliases and a
// column in the roster, so an essay here makes both unreadable.
const maxNameLen = 32

// Name is this device's human-meaningful name: $MUSTER_DEVICE_NAME, else the
// persisted device-name file, else the machine's hostname reduced to
// alias-safe form. Never returns an error — a display name that could fail
// would have to be handled at every call site for no benefit — and returns ""
// only when there is no hostname to fall back on.
//
// This is what an operator SAYS ("the ci-cd session on my work laptop") and
// what a model matches against in the roster. The device ID is the identifier
// the code compares; this is the string a human recognises. Keeping them
// separate is deliberate: renaming a device must not re-key anything.
func Name() string {
	if n := NameConfigured(); n != "" {
		return n
	}
	host, err := os.Hostname()
	if err != nil {
		return ""
	}
	return SanitizeName(host)
}

// NameConfigured returns this device's PERSISTED name — the environment
// override, or the device-name file — and "" only when neither exists yet.
//
// A non-empty return no longer proves the operator chose the name: Adopt()
// (called by seedAlias ahead of every mint) writes the sanitized hostname
// into the same file on a machine's first use, exactly as an operator's own
// `muster device <name>` would. Once either has run, this returns the same
// thing either way — that is deliberate, since Adopt()'s whole point is to
// guarantee a name is pinned (never merely a live, re-derivable hostname
// lookup) by the time an alias is minted, so seeding can be unconditional.
//
// What still depends on this: Name() falls back to a live hostname lookup
// only while this is still "" (nothing has adopted or set a name yet).
// `muster device`'s source=hostname|configured display (devicename.go) also
// reads this — once a name is pinned, by an operator or by Adopt(), it
// reports "configured", since from that point nothing here distinguishes the
// two.
func NameConfigured() string {
	if v := SanitizeName(os.Getenv(NameEnv)); v != "" {
		return v
	}
	b, err := os.ReadFile(filepath.Join(paths.Home(), NameFileName))
	if err != nil {
		return ""
	}
	return SanitizeName(string(b))
}

// SanitizeName reduces s to the character set a device name may use:
// lowercase letters, digits, and single dashes, trimmed at both ends and
// capped at maxNameLen. It is deliberately lossy rather than validating,
// because the inputs it has to survive are hostnames — "Alex's MacBook
// Pro.local" has to become something usable, not an error.
//
// The character set is not cosmetic. A device name is a PREFIX on derived
// aliases, and an alias is an address, so anything that would need quoting on
// a command line or would collide with muster's own "project:label"
// addressing has to be gone before it reaches a roster.
func SanitizeName(s string) string {
	if i := strings.IndexByte(s, '.'); i > 0 {
		s = s[:i] // "host.local" / an FQDN: the first label is the machine
	}
	var b strings.Builder
	lastDash := true // leading dashes are dropped, same as repeated ones
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		case !lastDash:
			b.WriteByte('-')
			lastDash = true
		}
		if b.Len() >= maxNameLen {
			break
		}
	}
	return strings.Trim(b.String(), "-")
}

// SetName persists name as this device's name, replacing any previous one.
// The name is sanitized first, and an input that sanitizes to nothing is an
// error rather than a silent no-op — an operator who typed something expects
// either that name or a complaint.
func SetName(name string) (string, error) {
	clean := SanitizeName(name)
	if clean == "" {
		return "", fmt.Errorf("device: %q is not a usable device name (letters, digits and dashes)", name)
	}
	if err := paths.EnsureHome(); err != nil {
		return "", err
	}
	p := filepath.Join(paths.Home(), NameFileName)
	if err := os.WriteFile(p, []byte(clean+"\n"), 0o644); err != nil {
		return "", err
	}
	return clean, nil
}

// Adopt guarantees this machine has a configured device name, taking the
// sanitized hostname if the operator has not chosen one. It reports the name
// in force and whether this call is what established it, so a caller can
// announce the adoption exactly once.
//
// Pinning matters more than the name itself. A hostname is not stable — macOS
// renames a machine when another on the network already answers to that name,
// and a domain join or a restore from backup can change it too. The device
// name is stamped into every alias at registration, so a seed that followed a
// live hostname would silently start registering a machine under a new prefix
// and orphan every alias and inbox behind the old one. Writing it once makes
// the seed immutable regardless of what the OS later does.
//
// An operator's own choice — the file, or $MUSTER_DEVICE_NAME — is never
// overwritten. The env override is honoured without being persisted: it is a
// single-shell override by contract, and writing it would make a throwaway
// value permanent.
func Adopt() (string, bool, error) {
	if n := NameConfigured(); n != "" {
		return n, false, nil
	}
	host, err := os.Hostname()
	if err != nil {
		return "", false, fmt.Errorf("device: no configured name and no hostname to adopt: %w", err)
	}
	name, err := SetName(host)
	if err != nil {
		return "", false, err
	}
	return name, true, nil
}

// Existing returns this device's identifier if one is ALREADY established —
// $MUSTER_DEVICE_ID, else a persisted device-id file — and "" otherwise. It
// never generates and never writes, which is the whole point: read-only
// commands (muster agents renders a device column) must not mint this
// machine's identity as a side effect of displaying something. ID is for
// callers that are registering and genuinely need an identity to exist;
// everything else wants this. Every error reads as "" — a display column
// cannot fail a command.
func Existing() string {
	if v := strings.TrimSpace(os.Getenv("MUSTER_DEVICE_ID")); v != "" {
		return v
	}
	b, err := os.ReadFile(filepath.Join(paths.Home(), FileName))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// ID returns this device's stable identifier: $MUSTER_DEVICE_ID if set,
// otherwise a UUID generated once and persisted at <MUSTER_HOME>/device-id.
//
// SocketPath cannot serve this purpose — two machines can both have
// /tmp/tmux-501/default — so cross-device wake needs an identifier that is
// unique per machine rather than per tmux server.
func ID() (string, error) {
	if v := strings.TrimSpace(os.Getenv("MUSTER_DEVICE_ID")); v != "" {
		return v, nil
	}
	p := filepath.Join(paths.Home(), FileName)
	b, err := os.ReadFile(p)
	// Only "no such file" means "not generated yet". Any OTHER read error —
	// EACCES on a file whose directory is still writable, EIO, a directory
	// where the file should be — must NOT fall through to generation, because
	// generation ROTATES this device's identity, and the device id is the
	// wake-routing key: every agent already registered from this machine would
	// suddenly look like another device's, so ReconcileLocalSessions and
	// DevicePoll would filter all of them out and every badge on the machine
	// would go dark until each agent re-registered. Failing loudly on a
	// transient read error costs one command; rotating costs the machine's
	// wake path with no error anywhere.
	if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("device: read %s: %w", p, err)
	}
	if v := strings.TrimSpace(string(b)); v != "" {
		return v, nil
	}
	if err := paths.EnsureHome(); err != nil {
		return "", err
	}
	v := uuid.NewString()
	if err := os.WriteFile(p, []byte(v+"\n"), 0o644); err != nil {
		return "", err
	}
	return v, nil
}
