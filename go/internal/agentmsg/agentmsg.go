// Package agentmsg holds the size bound on one agent message, shared by the
// agent→Runner hop (the AgentGateway socket) and the Runner→Server hop (the
// RunnerService door) so the two caps cannot drift apart.
package agentmsg

// MaxBytes bounds one AgentGateway message the Runner reads from an agent.
// connect-go imposes no read limit unless WithReadMaxBytes is set, so without
// it a compromised in-container agent could stream one arbitrarily large
// message. It is a small multiple of the retired 4 MiB stdout line cap.
const MaxBytes = 16 * 1024 * 1024

// MaxSessionBlobBytes leaves room for the RunnerService envelope within its read limit.
const MaxSessionBlobBytes = MaxBytes - 1<<20
