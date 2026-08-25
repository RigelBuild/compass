// The agent side of the agent->Runner call transport: a RunnerCallTransport over
// a Connect client that dials the Runner's per-container Unix socket
// (design docs/designs/product/compass-agent-runner-transport/design.md, SEA-1351
// T4). The in-container first-party agent reaches its Runner over a bind-mounted
// Unix socket — a local hop, no network path, so the egress seal is untouched.
//
// This module is the ONE place @connectrpc/connect-node is allowed: the biome
// `noRestrictedImports` fence blocks raw connect/connect-node everywhere else in
// the agent (reach the daemon through @compass/client, the owned door), and a
// single scoped override lets the transport dial the Runner socket directly. The
// AgentGateway service is internal-only gen (never the public client surface), so
// this is not a server-door client.

import { type CallOptions, createClient } from "@connectrpc/connect";
import {
	createGrpcTransport,
	Http2SessionManager,
} from "@connectrpc/connect-node";
import { Layer, Logger, ManagedRuntime } from "effect";

import type {
	CommsCallRequest,
	CommsCallResult,
	ControlSubscribeRequest,
	ForgeCallRequest,
	ForgeCallResult,
	LifecycleCallRequest,
	LifecycleCallResult,
	PostConversationFrameRequest,
	PostConversationFrameResponse,
} from "../gen/compass/v1/agent_gateway_pb";
import { AgentGateway } from "../gen/compass/v1/agent_gateway_pb";
import type { AgentControl } from "../gen/compass/v1/agent_pb";
import { makeOtelLayer } from "./otel-layer";
import { createPublishSpine, type PublishSpine } from "./publish-spine";
import { setTransportRuntime } from "./runtime-channel";

/**
 * The agent's handle on the AgentGateway RPCs over the Runner socket. T4
 * (SEA-1351) landed `comms`; the transport-consolidation C4 lane extends it with
 * the frame/control spine the socket sink + source ride:
 *
 *  - `comms` — the agent-initiated comms call, consumed by the comms-tools
 *    `CommsBroker` (comms.ts) that the two native comms tools call through.
 *  - `publishSpine()` — the single per-session Publish client-stream, memoized:
 *    the socket FrameSink pushes trace/session frames onto it and the
 *    ControlSource pushes control-plane ack frames onto the SAME spine, so the
 *    Runner sees one ordered publisher (hub gap-detection invariant). Exposed as
 *    the shared spine rather than the raw stream so the two producers cannot
 *    open two streams.
 *  - `postConversationFrame` — the durable conversation unary (delivered-or-
 *    erred); the sink awaits + retries it.
 *  - `control` — the agent-opened control server-stream; the ControlSource
 *    consumes it.
 *  - `close()` — release the underlying HTTP/2 session AND dispose the single
 *    transport-owned `ManagedRuntime` that backs the sink/spine/source lanes
 *    (design docs/designs/platform/compass-agent-effect-adoption/design.md §T5).
 *    The composition root calls it AFTER the sink's drain barrier: the session
 *    manager keeps an idle connection alive for `idleConnectionTimeoutMs` (15
 *    minutes by default), so a self-terminating agent that only drains would
 *    linger holding the socket. Draining first is what makes closing safe —
 *    close abandons open streams. The dispose is likewise safe only after the
 *    drain barrier: it is fire-and-forget from close()'s sync `void` signature,
 *    and by the time the root calls close() the drain has already quiesced every
 *    fiber the runtime backs, so the dispose has nothing live to race.
 */
export interface RunnerTransport {
	comms(req: CommsCallRequest): Promise<CommsCallResult>;
	lifecycle(req: LifecycleCallRequest): Promise<LifecycleCallResult>;
	forge(req: ForgeCallRequest): Promise<ForgeCallResult>;
	publishSpine(): PublishSpine;
	postConversationFrame(
		req: PostConversationFrameRequest,
		options?: CallOptions,
	): Promise<PostConversationFrameResponse>;
	control(
		req: ControlSubscribeRequest,
		options?: CallOptions,
	): AsyncIterable<AgentControl>;
	close(): void;
}

/**
 * A RunnerTransport over a Connect client that dials the in-container Unix
 * socket at socketPath. Uses the gRPC transport over Node's http2 module for
 * cleartext HTTP/2 (h2c), matching the Runner's socket door.
 *
 * The socket is addressed via the session manager's http2 session options
 * (Node http2/tls's Unix-socket `path` option — NOT `socketPath`, which is the
 * http.request name for the HTTP/1.1 transport). When `path` is set,
 * http2.connect ignores host/port, so `baseUrl` is a required-but-ignored
 * placeholder the URL parser needs.
 *
 * The session manager is constructed here rather than left implicit so the
 * transport has a handle to abort: `sessionManager` supersedes the transport's
 * own `nodeOptions`, so the socket path moves onto the manager with it.
 */
export function createUnixSocketTransport(socketPath: string): RunnerTransport {
	// Placeholder host: the URL parser requires one, but http2.connect ignores
	// host/port once the session options name the Unix socket.
	const sessionManager = new Http2SessionManager(
		"http://unix",
		{},
		{ path: socketPath },
	);
	const transport = createGrpcTransport({
		baseUrl: "http://unix",
		sessionManager,
	});
	const client = createClient(AgentGateway, transport);
	// The single ManagedRuntime this transport owns and every Effect lane behind
	// it (sink, spine, source) shares, so the production wiring path runs on ONE
	// scheduler (design record §T5). The default logger is removed so a
	// handled/swallowed lane failure does not double-report to the console;
	// makeOtelLayer() adds the transport's OTel provider when an OTLP endpoint is
	// configured and Layer.empty otherwise, so instrumentation is inert with no
	// endpoint (design docs/designs/platform/compass-agent-effect-otel/design.md
	// Decision 4). close() disposes it; the sibling factories BORROW it (never
	// dispose) via the module-private channel.
	const runtime = ManagedRuntime.make(
		Layer.merge(Logger.remove(Logger.defaultLogger), makeOtelLayer()),
	);
	// The Publish spine is created once on first use and shared by the sink +
	// source; memoize it so both reach the same single stream. It runs on the
	// transport's runtime (threaded by argument — the spine takes `publish`, not
	// `transport`, so it cannot read the channel), so its drain() does NOT dispose
	// the borrowed runtime.
	let spine: PublishSpine | undefined;
	const runnerTransport: RunnerTransport = {
		comms: (req) => client.comms(req),
		lifecycle: (req) => client.lifecycle(req),
		forge: (req) => client.forge(req),
		publishSpine: () => {
			spine ??= createPublishSpine((stream) => client.publish(stream), runtime);
			return spine;
		},
		postConversationFrame: (req, options) =>
			client.postConversationFrame(req, options),
		control: (req, options) => client.control(req, options),
		close: () => {
			sessionManager.abort();
			// Fire-and-forget from the sync `void` signature: the composition root
			// calls close() only AFTER the sink's drain barrier, which has already
			// quiesced every fiber this runtime backs, so the dispose races nothing
			// (design record §T5; `index.ts` close() doc above). ManagedRuntime.dispose
			// is not expected to reject; the `.catch` is a guard so a future rejecting
			// dispose surfaces as nothing rather than an unhandledRejection at teardown.
			void runtime.dispose().catch(() => {});
		},
	};
	// Publish the owned runtime on the module-private channel so createSocketFrameSink
	// and createSocketControlSource BORROW it instead of each making their own
	// (design record §T5). Absent for a fake transport → those factories fall back
	// to a self-owned default runtime, disposed at their own teardown seam.
	setTransportRuntime(runnerTransport, runtime);
	return runnerTransport;
}
