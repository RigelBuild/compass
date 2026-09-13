// @compass/agent — the first-party Compass agent (design: architecture-lineage). Built on
// the OMP SDK: subscribes the SDK event stream, maps it to compass.v1 `AgentFrame`s, and
// speaks compass.v1 over the AgentGateway socket the Runner bind-mounts. Wire envelopes
// stay behind FrameSink / ControlSource, so a change of carrier touches only those impls.

export { CompassAgent, type CompassAgentOptions } from "./agent";
export { CommsBroker, type CommsTransport, createCommsTools } from "./comms";
export type { AgentControl, ControlSource } from "./control";
export {
	createForgeTools,
	ForgeBroker,
	type ForgeTransport,
} from "./forge";
export { type FrameSink, type OutboundFrame, ProtojsonLineSink } from "./frame";
export {
	createLifecycleTools,
	LifecycleBroker,
	type LifecycleTransport,
} from "./lifecycle";
export { EventMapper, type MapOutput, type UnmappedEvent } from "./mapping";
export { createSocketControlSource } from "./transport/control-source";
export { createSocketFrameSink } from "./transport/frame-sink";
export {
	createUnixSocketTransport,
	type RunnerTransport,
} from "./transport/index";
