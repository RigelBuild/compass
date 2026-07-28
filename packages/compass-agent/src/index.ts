// @compass/agent — the first-party Compass agent (design compass-0.6 §T5).
//
// Built on the OMP SDK (@oh-my-pi/pi-agent-core): it subscribes the SDK event
// stream, maps it to compass.v1 `AgentFrame`s in-process (its own testable
// surface — no Runner-side translator), and speaks a newline-framed compass.v1
// stdio channel the Runner drives over ExecStreaming. The wire envelopes
// (`AgentFrame` out, `AgentControl` in) are isolated behind FrameSink /
// ControlSource so the pending gen of those internal proto messages touches
// only the sink/source impls.

export { CompassAgent, type CompassAgentOptions } from "./agent";
export { CommsBroker, type CommsTransport, createCommsTools } from "./comms";
export type { AgentControl, ControlSource } from "./control";
export { type FrameSink, type OutboundFrame, ProtojsonLineSink } from "./frame";
export { EventMapper, type MapOutput, type UnmappedEvent } from "./mapping";
