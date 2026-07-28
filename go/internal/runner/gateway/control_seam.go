//go:build unix

package gateway

// control_seam.go is the boundary between telemetry ingest and the control
// lane. Two AgentFrame variants that arrive on the Publish stream are NOT
// telemetry to relay upstream — they are control-plane acks the agent sends
// back (transport-consolidation record, ack routing):
//
//   - ReplayCompleteAck — the agent's replay-barrier ack (OQ-4(i)): on receipt
//     the Runner releases the live control ops held behind the restart replay
//     barrier.
//   - ControlAck — the agent's selective apply-ack (amended OQ-6): a contiguous
//     cursor (highest contiguously-applied control_seq) plus a bounded set of
//     seqs applied out of order above it. The Runner retires retained ops up to
//     the cursor and drops the individually-acked ones.
//
// The telemetry-ingest path routes these to the control lane instead of
// forwarding them; the control lane owns the retention/barrier state behind this
// interface. Keeping the seam an interface lets telemetry ingest build and test
// independently of the control lane (a fake router records the routed acks) and
// lets the control-lane owner inject the real implementation without changing
// the ingest path.

// ControlRouter is the control lane as the ack-routing path sees it. The control
// lane's ControlSender implementation satisfies it; until the control lane
// lands, the Gateway uses a no-op default so a stray ack on the Publish stream
// is handled (dropped, never relayed upstream), not mishandled.
type ControlRouter interface {
	// AckControl retires the session's retained control ops with control_seq <=
	// ackedSeq, and drops any op whose seq is named in appliedAbove even though it
	// sits past the cursor (an out-of-order apply the agent has confirmed). Routed
	// from a ControlAck AgentFrame on Publish.
	AckControl(sessionID string, ackedSeq uint64, appliedAbove []uint64)
	// ReleaseReplayBarrier releases the live control ops held behind the session's
	// restart replay barrier. Routed from a ReplayCompleteAck AgentFrame on
	// Publish.
	ReleaseReplayBarrier(sessionID string)
}

// noopControlRouter is the default ControlRouter used until the control lane is
// wired in with the real one. It drops acks (they are still consumed off the
// Publish stream and never relayed upstream — the telemetry-ingest contract —
// just not applied to any retention state, because none exists yet). The control
// lane replaces it with the real sender.
type noopControlRouter struct{}

func (noopControlRouter) AckControl(string, uint64, []uint64) {}
func (noopControlRouter) ReleaseReplayBarrier(string)         {}
