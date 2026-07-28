// @compass/client — the generated TypeScript client for the compass.v1
// contract. The sole sanctioned way for UI code to reach the server; raw gRPC
// stub/socket access is fenced off by lint (the owned door).

import {
	type Client,
	createClient,
	type Interceptor,
	type Transport,
} from "@connectrpc/connect";
import { createGrpcWebTransport } from "@connectrpc/connect-web";
import { CommsService } from "./gen/compass/v1/comms_pb";
import { CompassService } from "./gen/compass/v1/compass_pb";

/** Sets `authorization: Bearer <token>` on every request. */
export function bearerAuthInterceptor(token: string): Interceptor {
	return (next) => (req) => {
		req.header.set("authorization", `Bearer ${token}`);
		return next(req);
	};
}

// The interceptor list for an optional bearer token, and the single place the
// "one bearer, only when asked" rule lives. Three cases, kept distinct on
// purpose: a non-empty token installs exactly one bearer interceptor; `undefined`
// (the arg omitted) is a deliberate no-auth client that installs none and sends
// no authorization header; an empty string is a misconfigured credential, not a
// request for no auth, so it fails loud rather than silently degrading to an
// unauthenticated client.
function bearerInterceptors(token?: string): Interceptor[] | undefined {
	if (token === "") {
		throw new Error(
			"compass-client: empty bearer token — pass a non-empty token, or omit it for a no-auth client",
		);
	}
	return token ? [bearerAuthInterceptor(token)] : undefined;
}

/** A typed client for the Compass server over a given transport. */
export type CompassClient = Client<typeof CompassService>;

/** Create a typed compass.v1 client bound to `transport`. */
export function createCompassClient(transport: Transport): CompassClient {
	return createClient(CompassService, transport);
}

// Re-exported so non-web consumers can type a custom transport without
// importing @connectrpc/connect directly (the fence blocks that import).
export type { Transport } from "@connectrpc/connect";

/**
 * Create a compass.v1 client over gRPC-Web at `baseUrl` — the door the web UI
 * uses. Bundles the transport so UI code imports only `@compass/client`. When
 * `token` is set, every request carries `authorization: Bearer <token>`.
 */
export function createCompassWebClient(
	baseUrl: string,
	token?: string,
): CompassClient {
	return createCompassClient(
		createGrpcWebTransport({
			baseUrl,
			interceptors: bearerInterceptors(token),
		}),
	);
}

/**
 * Create a compass.v1 client whose gRPC-Web transport routes every request
 * through a caller-supplied `fetch`. The desktop shell passes a `fetch` that
 * tunnels over Tauri IPC to the server's Unix socket, so the webview reaches the
 * server with no TCP port. `baseUrl` is a same-origin placeholder the custom
 * `fetch` ignores \u2014 gRPC-Web only reads it to form the request path. Keeps the
 * `@connectrpc/connect-web` import inside the owned door. When `token` is set,
 * every request carries `authorization: Bearer <token>`.
 */
export function createCompassClientOverFetch(
	// The fetch shape `createGrpcWebTransport` consumes — the request/response
	// call signature, not the full DOM `fetch` (which also carries `preconnect`).
	fetch: (input: RequestInfo | URL, init?: RequestInit) => Promise<Response>,
	baseUrl = "http://compass.localhost",
	token?: string,
): CompassClient {
	// connect-web types its `fetch` option as `typeof globalThis.fetch`, which
	// under some DOM/bun lib configs carries extra members (e.g. `preconnect`) a
	// custom transport fetch has no reason to implement. The call signature is
	// all the transport uses, so cast at this single boundary.
	return createCompassClient(
		createGrpcWebTransport({
			baseUrl,
			fetch: fetch as typeof globalThis.fetch,
			interceptors: bearerInterceptors(token),
		}),
	);
}

/** A typed client for the Compass comms service over a given transport. */
export type CommsClient = Client<typeof CommsService>;

/** Create a typed compass.v1 comms client bound to `transport`. */
export function createCommsClient(transport: Transport): CommsClient {
	return createClient(CommsService, transport);
}

/**
 * Create a comms client over gRPC-Web at `baseUrl`, mirroring
 * `createCompassWebClient`. When `token` is set, every request carries
 * `authorization: Bearer <token>`.
 */
export function createCommsWebClient(
	baseUrl: string,
	token?: string,
): CommsClient {
	return createCommsClient(
		createGrpcWebTransport({
			baseUrl,
			interceptors: bearerInterceptors(token),
		}),
	);
}

/**
 * Create a comms client whose gRPC-Web transport routes every request through a
 * caller-supplied `fetch`, mirroring `createCompassClientOverFetch`. When
 * `token` is set, every request carries `authorization: Bearer <token>`.
 */
export function createCommsClientOverFetch(
	fetch: (input: RequestInfo | URL, init?: RequestInit) => Promise<Response>,
	baseUrl = "http://compass.localhost",
	token?: string,
): CommsClient {
	// Same single-boundary fetch cast as the compass door above.
	return createCommsClient(
		createGrpcWebTransport({
			baseUrl,
			fetch: fetch as typeof globalThis.fetch,
			interceptors: bearerInterceptors(token),
		}),
	);
}

// Re-exported so comms consumers can construct generated messages (test
// fixtures, request payloads) through the one door rather than depending on
// @bufbuild/protobuf directly.
export { create } from "@bufbuild/protobuf";
export type {
	Account,
	AgentAccount,
	Ask,
	AskOption,
	AskQuestion,
	Channel,
	ChannelGroup,
	Message,
	MessageBlock,
	SubscribeCommsResponse,
	UserAccount,
} from "./gen/compass/v1/comms_pb";
export {
	AccountSchema,
	AgentAccountSchema,
	AskOptionSchema,
	AskQuestionSchema,
	AskSchema,
	ChannelGroupSchema,
	ChannelGroupVisibility,
	ChannelKind,
	ChannelSchema,
	CommsService,
	MessageBlockSchema,
	MessageSchema,
	UserAccountSchema,
} from "./gen/compass/v1/comms_pb";
export type {
	AgentSessionStatus,
	GetServerInfoRequest,
	GetServerInfoResponse,
	ResyncRequired,
	ServerStatus,
	SubscribeEventsRequest,
	SubscribeEventsResponse,
} from "./gen/compass/v1/compass_pb";
export {
	AgentSessionState,
	CompassService,
	ServerState,
} from "./gen/compass/v1/compass_pb";
