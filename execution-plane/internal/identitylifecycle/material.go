package identitylifecycle

import (
	"fmt"
	"path"
	"strings"
)

// The provider is tmpfs-only today: /tmp and /run, no volume, no host bind. A
// persistent home is a real later requirement (CLI session and tool state for
// R4/R5), so the boundary has to be settled before anything is mounted rather
// than after.
//
// The rule is one-directional: identity material must stay non-persistent
// forever, whatever home becomes. If the key, the machine ID or the installed
// certificate could survive a container, then a restart would no longer force a
// new epoch and generation, and the "never reuse a binding tuple" invariant in
// this package would be unenforceable from outside.
//
// Nothing here mounts anything or changes the provider. It exists so that a
// future persistent-home change has to state its paths and be checked first.

// PersistentHomeRoot is the only subtree a persistent store may ever back.
//
// This is an allowlist on purpose. A denylist of identity paths cannot be made
// safe by string comparison alone: on the Debian base /var/run is a symlink to
// /run, so a lexical "not under /run/execution/identity" test would happily
// approve /var/run/execution/identity. Naming the one permitted subtree removes
// that whole class instead of chasing aliases.
const PersistentHomeRoot = "/home"

// NonPersistentPaths are container paths that must never be backed by a store
// that outlives the container. They are checked in addition to the allowlist as
// defence in depth, and are asserted against the provider constants in the
// package tests so the two cannot drift apart.
var NonPersistentPaths = []string{
	"/run/execution/identity",
	"/run/execution/runtime-ca.pem",
}

// ErrPersistence reports a proposed persistent path that is outside the
// permitted home subtree or would outlive the instance's identity.
var ErrPersistence = fmt.Errorf("%w: persistent path rejected", ErrTransition)

// ValidatePersistentPaths checks a proposed set of persistent container paths.
//
// It deliberately does not approve a persistence design. Passing here only
// means the proposal is confined to the home subtree and captures no identity
// material; per-instance ownership, per-account separation and erasure on
// account change are separate requirements recorded in the P3b design, and the
// real enforcement belongs at mount time against resolved paths.
func ValidatePersistentPaths(paths []string) error {
	for _, proposed := range paths {
		if proposed == "" || len(proposed) > 4096 || !strings.HasPrefix(proposed, "/") ||
			path.Clean(proposed) != proposed || strings.ContainsFunc(proposed, isControl) {
			return fmt.Errorf("%w: %q is not an absolute clean path", ErrPersistence, proposed)
		}
		// Checked before the allowlist so that naming identity material gets the
		// specific reason rather than a generic "outside /home", and so this
		// rule keeps its meaning if the allowlist root is ever widened.
		for _, reserved := range NonPersistentPaths {
			if withinPath(proposed, reserved) || withinPath(reserved, proposed) {
				return fmt.Errorf("%w: %q overlaps identity material at %q", ErrPersistence, proposed, reserved)
			}
		}
		if !withinPath(proposed, PersistentHomeRoot) || proposed == PersistentHomeRoot {
			return fmt.Errorf("%w: %q is outside %q", ErrPersistence, proposed, PersistentHomeRoot)
		}
	}
	return nil
}

func isControl(r rune) bool { return r < 0x20 || r == 0x7f }

// withinPath reports whether candidate is at or below parent, comparing whole
// path segments so that /run/execution/identity-backup is not treated as being
// inside /run/execution/identity.
func withinPath(candidate, parent string) bool {
	if candidate == parent {
		return true
	}
	return strings.HasPrefix(candidate, strings.TrimSuffix(parent, "/")+"/")
}
