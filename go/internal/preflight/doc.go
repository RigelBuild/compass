// Package preflight is the native app's embedded-mode host preflight: the set of
// checks that must pass BEFORE spawning the embedded stack (compass-stack up)
// per the compass-native-embedded-revival design (§A3). Each check produces
// actionable failure copy so an operator on an unsupported host sees the precise
// precondition to fix, rather than a deep failure inside the stack.
//
// The checker core is inverted over injected effect functions (see Deps),
// mirroring the go/internal/stack idiom: every genuine external effect —
// probing rootless podman, the podman version floor, the darwin podman machine,
// and the local image store — is a func the caller supplies. The core imports
// none of those subsystems, so unit tests supply stubs and no test shells out.
//
// The checks are cross-OS: the OS check accepts linux or darwin, and a
// darwin-only machine check joins the set when its adapter is wired. The host
// uid is deliberately NOT checked — the runner is uid-agnostic now via the
// --userns=keep-id:uid= remap, so a uid gate would break launch on every host
// with uid != 1000; the importable single source of truth for the agent uid is
// go/internal/agentuid.AgentUID.
package preflight
