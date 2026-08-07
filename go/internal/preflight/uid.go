package preflight

// DefaultAgentUID is the uid the embedded agent runs as inside its container,
// matching compass-runner's defaultAgentUID (cmd/compass-runner/main.go). It is
// duplicated here deliberately because that const lives in package main and is
// not importable; consolidating both onto one shared const is a tracked
// follow-up (SEA-1685 Open Question).
const DefaultAgentUID = 1000
