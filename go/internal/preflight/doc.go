// Package preflight is the native app's embedded-mode host preflight: the set of
// checks that must pass BEFORE spawning the embedded stack (compass-stack up)
// per the compass-native-app design (§Plan T4). Each check produces actionable
// failure copy so an operator on an unsupported host sees the precise
// precondition to fix, rather than a deep failure inside the stack.
//
// The checker core is inverted over injected effect functions (see Deps),
// mirroring the go/internal/stack idiom: every genuine external effect —
// probing rootless podman, the local image store, and Postgres reachability —
// is a func the caller supplies. The core imports none of those subsystems, so
// unit tests supply stubs and no test shells out. The runner's required uid is
// likewise injected (not hard-coded here), keeping compass-runner's constant the
// single source of truth.
package preflight
