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

import type {
	CommsCallRequest,
	CommsCallResult,
	ControlSubscribeRequest,
	PostConversationFrameRequest,
	PostConversationFrameResponse,
} from "../gen/compass/v1/agent_gateway_pb";
import { AgentGateway } from "../gen/compass/v1/agent_gateway_pb";
import type { AgentControl } from "../gen/compass/v1/agent_pb";
import { createPublishSpine, type PublishSpine } from "./publish-spine";

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
 *  - `close()` — release the underlying HTTP/2 session. The composition root
 *    calls it AFTER the sink's drain barrier: the session manager keeps an idle
 *    connection alive for `idleConnectionTimeoutMs` (15 minutes by default), so
 *    a self-terminating agent that only drains would linger holding the socket.
 *    Draining first is what makes closing safe — close abandons open streams.
 */
export interface RunnerTransport {
	comms(req: CommsCallRequest): Promise<CommsCallResult>;
	publishSpine(): PublishSpine;
	postConversationFrame(
		req: PostConversationFrameRequest,
		options?: CallOptions,
	): Promise<PostConversationFrameResponse>;
	control(req: ControlSubscribeRequest): AsyncIterable<AgentControl>;
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
	// The Publish spine is created once on first use and shared by the sink +
	// source; memoize it so both reach the same single stream.
	let spine: PublishSpine | undefined;
	return {
		comms: (req) => client.comms(req),
		publishSpine: () => {
			spine ??= createPublishSpine((stream) => client.publish(stream));
			return spine;
		},
		postConversationFrame: (req, options) =>
			client.postConversationFrame(req, options),
		control: (req) => client.control(req),
		close: () => sessionManager.abort(),
	};
}
