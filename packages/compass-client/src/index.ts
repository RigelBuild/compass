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

// The W3C trace-context RESPONSE header the server sets on every unary reply
// (go/internal/otel/interceptor.go). Draft-stage in the spec, but the standard
// name and the "00-<32hex traceid>-<16hex spanid>-<2hex flags>" grammar are
// what the server emits, and the network door CORS-exposes it, so the browser
// can read it.
const traceResponseHeader = "traceresponse";

/**
 * A one-slot mailbox holding the trace id of the most recent server reply.
 *
 * Mutable on purpose, and the mutability is the whole point: the transport is
 * constructed during boot BEFORE the analytics client exists, so the writer
 * (this package's response interceptor) and the reader (the analytics wrapper,
 * layers above) cannot be introduced to each other at construction time. A
 * stable reference handed to both closes that gap without reordering boot and
 * without the transport layer taking a dependency on analytics.
 *
 * The write discipline — the transport interceptor writes, everything above it
 * only reads — is a CONVENTION, not a type guarantee: `current` is structurally
 * writable, so `clients.traceId.current = "x"` type-checks from any consumer.
 * Splitting into a reader/writer pair would make that structural, but with
 * exactly one writer and one reader the pair buys a type distinction nobody is
 * reaching around, at the cost of two interfaces and a cast at the seam that
 * constructs the slot — so the convention stands, documented rather than
 * enforced.
 */
export interface TraceIdSink {
	current: string | undefined;
}

// A 32-hex trace id of all zeros is explicitly invalid per W3C trace-context —
// it is what an uninitialized/unsampled span context renders as, so stamping it
// would correlate every such event to one bogus "trace".
const zeroTraceId = "00000000000000000000000000000000";

/**
 * Extract the 32-hex trace id from a `traceresponse` header value, or
 * `undefined` when the value is absent or not a trace id we can trust.
 *
 * Only field 2 is returned: the span id and flags describe one server-side span,
 * which is not the correlation key, and forwarding the whole `00-…-…-…` string
 * would join nothing on the trace side.
 *
 * Version handling follows the spec's forward-compatibility rule: fields 1-3
 * keep their meaning across versions, so a future version is parsed by reading
 * the same three positions and ignoring anything appended. Two things are NOT
 * tolerated. A malformed 4-field value: at the known version `00` the grammar
 * is fully specified, so a bad span id or flag byte means the sender is broken,
 * and trusting field 2 out of a broken value is a guess. And version `ff`,
 * which the spec reserves as never-valid — a sender emitting it is stating the
 * value is not a real trace context, so trusting its field 2 would contradict
 * this parser's own rule that a value we cannot vouch for is no value at all.
 *
 * Hex is lowercase per the spec and returned exactly as received; normalizing
 * case would silently rewrite the key the trace backend indexed.
 */
export function parseTraceResponse(value: string): string | undefined {
	const fields = value.split("-");
	// Fields 1-3 are position-stable across versions; fewer than four means
	// there is no trace id position to read at all.
	if (fields.length < 4) {
		return undefined;
	}
	const [version, traceId, spanId, flags] = fields;
	if (version === undefined || !/^[0-9a-f]{2}$/.test(version)) {
		return undefined;
	}
	// `ff` is shaped like a version but reserved invalid, so it never reaches the
	// forward-compat branch below: a future-version parse would otherwise trust
	// field 2 out of a value the spec says can never be a trace context.
	if (version === "ff") {
		return undefined;
	}
	// Version 00's grammar is exactly four fields. Extra fields are a future
	// version's business, never this one's.
	if (version === "00" && fields.length !== 4) {
		return undefined;
	}
	if (traceId === undefined || !/^[0-9a-f]{32}$/.test(traceId)) {
		return undefined;
	}
	if (traceId === zeroTraceId) {
		return undefined;
	}
	if (spanId === undefined || !/^[0-9a-f]{16}$/.test(spanId)) {
		return undefined;
	}
	if (flags === undefined || !/^[0-9a-f]{2}$/.test(flags)) {
		return undefined;
	}
	return traceId;
}

/**
 * Records each response's `traceresponse` trace id into `sink`, so a consumer
 * above the transport can stamp it on whatever it emits next.
 *
 * An absent or unparseable header CLEARS NOTHING: the last known good trace id
 * stays in the sink. A call that carried no trace id is not evidence the
 * previous trace ended — the alternative (blanking on every untraced reply)
 * would leave the sink empty most of the time and drop correlation for events
 * that genuinely belong to the last traced call. Same reasoning for a failed
 * call: the rejection propagates untouched and the sink is left alone.
 */
export function traceResponseInterceptor(sink: TraceIdSink): Interceptor {
	return (next) => async (req) => {
		const res = await next(req);
		// `?? ""` collapses "header absent" with "header present but empty" —
		// harmless, because both mean there is nothing to record and the empty
		// string parses to `undefined` on the length check. A DUPLICATE
		// `traceresponse` (a realistic proxy artifact) is equally safe by
		// construction: `Headers.get` comma-joins the values, the join is
		// unparseable, and the sink keeps its last known good id rather than
		// picking one of two candidate traces at random. Both are considered
		// no-ops, not oversights.
		const traceId = parseTraceResponse(
			res.header.get(traceResponseHeader) ?? "",
		);
		if (traceId !== undefined) {
			sink.current = traceId;
		}
		return res;
	};
}

// The full interceptor list every client/transport factory installs, and the one
// place the two concerns compose. The bearer rule is unchanged (and still throws
// first on a misconfigured credential). The trace sink follows the same
// omitted-means-off discipline: no sink ⇒ no trace interceptor at all, so a
// caller that does not ask for correlation gets byte-identical behavior —
// including `undefined` rather than an empty list when neither is asked for.
function callInterceptors(
	token?: string,
	traceSink?: TraceIdSink,
): Interceptor[] | undefined {
	const bearer = bearerInterceptors(token);
	if (!traceSink) {
		return bearer;
	}
	return [...(bearer ?? []), traceResponseInterceptor(traceSink)];
}

/** A typed client for the Compass server over a given transport. */
export type CompassClient = Client<typeof CompassService>;

/** Create a typed compass.v1 client bound to `transport`. */
export function createCompassClient(transport: Transport): CompassClient {
	return createClient(CompassService, transport);
}

/**
 * Build one gRPC-Web `Transport` at `baseUrl` — the shared transport both the
 * comms and compass clients dial over, and the same instance the query layer
 * keys and calls by (`@connectrpc/connect-query-core` embeds a Transport
 * reference in every query key, so cache identity requires ONE instance).
 * When `token` is set, every request carries `authorization: Bearer <token>`,
 * exactly as the per-client web factories install it. Exposed so callers that
 * need cache-coherent queries build clients + queries over one transport rather
 * than the per-client transports each web factory buries.
 *
 * `opts.fetch` injects the transport's `fetch`: absent (the browser dev path)
 * the transport uses the platform `fetch`, byte-for-byte as before; supplied
 * (the native desktop shell) it routes every request through a shell-provided
 * fetch that tunnels over IPC — the one seam that lets the same clients dial
 * either transport with nothing above the boundary knowing the mode.
 *
 * `opts.traceSink` opts this transport into recording each reply's
 * `traceresponse` trace id; omitted, no trace interceptor is installed and the
 * transport behaves exactly as before.
 */
export function createCompassWebTransport(
	baseUrl: string,
	token?: string,
	opts?: { fetch?: typeof globalThis.fetch; traceSink?: TraceIdSink },
): Transport {
	return createGrpcWebTransport({
		baseUrl,
		// `opts.fetch` is already `typeof globalThis.fetch` — the exact type the
		// transport's `fetch` option takes — so it threads through directly; the
		// truthiness guard keeps the browser dev path (no injected fetch) building
		// the same `{ baseUrl, interceptors }` config as before.
		...(opts?.fetch ? { fetch: opts.fetch } : {}),
		interceptors: callInterceptors(token, opts?.traceSink),
	});
}

// Re-exported so non-web consumers can type a custom transport without
// importing @connectrpc/connect directly (the fence blocks that import).
export type { Transport } from "@connectrpc/connect";

// Re-exported so test fixtures build an in-memory fake server through the one
// door (a `createRouterTransport` handler serving compass.v1 methods), without
// importing @connectrpc/connect directly — the fence blocks that import, and a
// fake transport is the vendor's documented no-HTTP test path for the query
// layer. Dev/test-only; the shipped app dials `createCompassWebTransport`.
export { createRouterTransport } from "@connectrpc/connect";

/**
 * Create a compass.v1 client over gRPC-Web at `baseUrl` — the door the web UI
 * uses. Bundles the transport so UI code imports only `@compass/client`. When
 * `token` is set, every request carries `authorization: Bearer <token>`; when
 * `traceSink` is set, each reply's `traceresponse` trace id is recorded into it.
 */
export function createCompassWebClient(
	baseUrl: string,
	token?: string,
	traceSink?: TraceIdSink,
): CompassClient {
	return createCompassClient(
		createGrpcWebTransport({
			baseUrl,
			interceptors: callInterceptors(token, traceSink),
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
 * every request carries `authorization: Bearer <token>`; when `traceSink` is
 * set, each reply's `traceresponse` trace id is recorded into it.
 */
export function createCompassClientOverFetch(
	// The fetch shape `createGrpcWebTransport` consumes — the request/response
	// call signature, not the full DOM `fetch` (which also carries `preconnect`).
	fetch: (input: RequestInfo | URL, init?: RequestInit) => Promise<Response>,
	baseUrl = "http://compass.localhost",
	token?: string,
	traceSink?: TraceIdSink,
): CompassClient {
	// connect-web types its `fetch` option as `typeof globalThis.fetch`, which
	// under some DOM/bun lib configs carries extra members (e.g. `preconnect`) a
	// custom transport fetch has no reason to implement. The call signature is
	// all the transport uses, so cast at this single boundary.
	return createCompassClient(
		createGrpcWebTransport({
			baseUrl,
			fetch: fetch as typeof globalThis.fetch,
			interceptors: callInterceptors(token, traceSink),
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
 * `authorization: Bearer <token>`; when `traceSink` is set, each reply's
 * `traceresponse` trace id is recorded into it.
 */
export function createCommsWebClient(
	baseUrl: string,
	token?: string,
	traceSink?: TraceIdSink,
): CommsClient {
	return createCommsClient(
		createGrpcWebTransport({
			baseUrl,
			interceptors: callInterceptors(token, traceSink),
		}),
	);
}

/**
 * Create a comms client whose gRPC-Web transport routes every request through a
 * caller-supplied `fetch`, mirroring `createCompassClientOverFetch`. When
 * `token` is set, every request carries `authorization: Bearer <token>`; when
 * `traceSink` is set, each reply's `traceresponse` trace id is recorded into it.
 */
export function createCommsClientOverFetch(
	fetch: (input: RequestInfo | URL, init?: RequestInit) => Promise<Response>,
	baseUrl = "http://compass.localhost",
	token?: string,
	traceSink?: TraceIdSink,
): CommsClient {
	// Same single-boundary fetch cast as the compass door above.
	return createCommsClient(
		createGrpcWebTransport({
			baseUrl,
			fetch: fetch as typeof globalThis.fetch,
			interceptors: callInterceptors(token, traceSink),
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
	ListTopicsRequest,
	ListTopicsResponse,
	Message,
	MessageBlock,
	PinnedEntry,
	RosterEntry,
	SubscribeCommsResponse,
	SystemAccount,
	Topic,
	TopicUpserted,
	UserAccount,
} from "./gen/compass/v1/comms_pb";
export {
	AccountSchema,
	AgentAccountSchema,
	AgentPresence,
	AskOptionSchema,
	AskQuestionSchema,
	AskSchema,
	ChannelGroupSchema,
	ChannelGroupVisibility,
	ChannelKind,
	ChannelPostPolicy,
	ChannelSchema,
	CommsService,
	ListMessagesResponseSchema,
	MessageBlockSchema,
	MessageSchema,
	PinnedEntrySchema,
	RosterEntrySchema,
	RosterScope,
	SubscribeCommsResponseSchema,
	SystemAccountSchema,
	TopicSchema,
	UserAccountSchema,
} from "./gen/compass/v1/comms_pb";
export type {
	AgentAttribution,
	AgentSessionStatus,
	ChangedStats,
	Check,
	ChecksSummary,
	ForgeRef,
	GetServerInfoRequest,
	GetServerInfoResponse,
	Issue,
	PullRequest,
	ResyncRequired,
	Review,
	ReviewThread,
	ServerStatus,
	SubscribeEventsRequest,
	SubscribeEventsResponse,
	TrackerRef,
} from "./gen/compass/v1/compass_pb";
export {
	AgentSessionState,
	CompassService,
	ForgeProvider,
	GetServerInfoResponseSchema,
	IssueSchema,
	IssueState,
	PullRequestSchema,
	ServerState,
	SubscribeEventsResponseSchema,
} from "./gen/compass/v1/compass_pb";
