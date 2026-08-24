package agentuid

// AgentUID is the unprivileged uid the Compass agent user runs as inside every
// agent container. The agent image bakes gid==uid==AgentUID, /nix and $HOME are
// owned by it (docs/designs/platform/compass-runner-arbitrary-uid/design.md),
// and the runner launches agent work under it so it runs unprivileged.
//
// The const is untyped on purpose: it flows into the uint32 runtime.AgentSpec.UID
// field and into int comparisons (e.g. os.Getuid()) alike, with no conversions at
// the call sites. Keeping it in one importable package is the single source of
// truth for the agent-uid invariant across the runner command and the runtime
// package's proofs.
const AgentUID = 1000
