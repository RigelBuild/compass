// @compass/agent — the first-party Compass agent (design compass-0.6 §T5).
//
// Built on the OMP SDK (@oh-my-pi/pi-agent-core): it subscribes the SDK event
// stream, maps it to compass.v1 `AgentFrame`s in-process (its own testable
// surface — no Runner-side translator), and speaks compass.v1 over the
// AgentGateway socket the Runner bind-mounts into the container (the
// transport-consolidation record supersedes the former newline-framed stdio
// channel). The wire envelopes (`AgentFrame` out, `AgentControl` in) are
// isolated behind FrameSink / ControlSource, so a change of carrier touches
// only the sink/source impls.

export { CompassAgent, type CompassAgentOptions } from "./agent";
export type { AgentControl, ControlSource } from "./control";
export { type FrameSink, type OutboundFrame, ProtojsonLineSink } from "./frame";
export { EventMapper, type MapOutput, type UnmappedEvent } from "./mapping";
export { createSocketControlSource } from "./transport/control-source";
export { createSocketFrameSink } from "./transport/frame-sink";
export {
	createUnixSocketTransport,
	type RunnerTransport,
} from "./transport/index";
