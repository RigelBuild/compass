// The agent side of the agent->Runner call transport: a RunnerCallTransport over a Connect
// client that dials the Runner's per-container Unix socket (RIG-1351 T4) — a local hop, no
// network path. This is the ONE place @connectrpc/connect-node is allowed (a scoped override
// past the biome fence); AgentGateway is internal-only gen, so this is not a server-door client.

import { type CallOptions, createClient } from "@connectrpc/connect";
import {
	createGrpcTransport,
	Http2SessionManager,
} from "@connectrpc/connect-node";
import { Layer, Logger, ManagedRuntime } from "effect";

import type {
	BoardCallRequest,
	BoardCallResult,
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
 * (RIG-1351) landed `comms`; the transport-consolidation C4 lane extends it with
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
 *    (design docs/designs/repo/compass-agent-effect-adoption/design.md §T5).
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
	board(req: BoardCallRequest): Promise<BoardCallResult>;
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
	// The single ManagedRuntime this transport owns and every Effect lane (sink, spine,
	// source) shares, so production runs on ONE scheduler (design record §T5). The default
	// logger is removed so a swallowed lane failure does not double-report; makeOtelLayer()
	// is inert with no OTLP endpoint. close() disposes it; sibling factories BORROW it.
	const runtime = ManagedRuntime.make(
		Layer.merge(Logger.remove(Logger.defaultLogger), makeOtelLayer()),
	);
	// The Publish spine is created once on first use and shared by the sink + source;
	// memoize it so both reach the same single stream. It runs on the transport's runtime
	// (threaded by argument), so its drain() does NOT dispose the borrowed runtime.
	let spine: PublishSpine | undefined;
	const runnerTransport: RunnerTransport = {
		comms: (req) => client.comms(req),
		lifecycle: (req) => client.lifecycle(req),
		forge: (req) => client.forge(req),
		board: (req) => client.board(req),
		publishSpine: () => {
			spine ??= createPublishSpine((stream) => client.publish(stream), runtime);
			return spine;
		},
		postConversationFrame: (req, options) =>
			client.postConversationFrame(req, options),
		control: (req, options) => client.control(req, options),
		close: () => {
			sessionManager.abort();
			// Fire-and-forget from the sync `void` signature: the composition root calls
			// close() only AFTER the sink's drain barrier has quiesced every fiber, so the
			// dispose races nothing. The `.catch` guards a future rejecting dispose from
			// surfacing as an unhandledRejection at teardown.
			void runtime.dispose().catch(() => {});
		},
	};
	// Publish the owned runtime on the module-private channel so the sink + source BORROW
	// it instead of each making their own (design record §T5). Absent for a fake transport,
	// those factories fall back to a self-owned default runtime disposed at their own teardown.
	setTransportRuntime(runnerTransport, runtime);
	return runnerTransport;
}
