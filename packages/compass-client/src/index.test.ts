import { describe, expect, spyOn, test } from "bun:test";
import * as connectWeb from "@connectrpc/connect-web";
import {
	bearerAuthInterceptor,
	type CommsClient,
	CommsService,
	type CompassClient,
	CompassService,
	create,
	createCommsClient,
	createCommsClientOverFetch,
	createCommsWebClient,
	createCompassClient,
	createCompassClientOverFetch,
	createCompassWebClient,
	createCompassWebTransport,
	createRouterTransport,
	GetServerInfoResponseSchema,
	parseTraceResponse,
	posthogSessionHeader,
	SubscribeCommsResponseSchema,
	sessionIdInterceptor,
	type TraceIdSink,
	type Transport,
	traceResponseInterceptor,
} from "./index";

// The option bag `createCompassWebTransport` hands the vendor gRPC-Web factory.
// The composed interceptor list is not readable off a built Transport, so this
// is the only surface on which "installs nothing when nothing was asked for"
// is observable at all.
type GrpcWebOptions = Parameters<typeof connectWeb.createGrpcWebTransport>[0];
type Interceptors = NonNullable<GrpcWebOptions["interceptors"]>;

// Spy the vendor factory for exactly one construction and report what it was
// handed — the same spy-the-factory seam apps/ui/src/live/provider.test.ts uses
// one layer up. Restored in `finally` so no spy leaks into a sibling test.
function transportOptionsFor(build: () => Transport): GrpcWebOptions {
	const spy = spyOn(connectWeb, "createGrpcWebTransport");
	try {
		build();
		const args = spy.mock.calls[0];
		if (args === undefined) {
			throw new Error(
				"transportOptionsFor: the transport factory was never called",
			);
		}
		return args[0];
	} finally {
		spy.mockRestore();
	}
}

// The REAL composed list for (token, sink), read off the production factory
// rather than hand-assembled, so a composition test cannot pass against an
// order or membership the shipped path never builds.
function composedInterceptors(token: string, sink: TraceIdSink): Interceptors {
	const { interceptors } = transportOptionsFor(() =>
		createCompassWebTransport("http://compass.localhost", token, {
			traceSink: sink,
		}),
	);
	if (interceptors === undefined) {
		throw new Error("composedInterceptors: token and sink installed nothing");
	}
	return interceptors;
}

// The fetch shape the OverFetch factories accept — the request/response call
// signature, not the full DOM `fetch`. This is the injection point for a mock
// fetch, so no test touches the network.
type FetchLike = (
	input: RequestInfo | URL,
	init?: RequestInit,
) => Promise<Response>;

// Route one unary call through a capturing fetch and report exactly what the
// transport handed it. The call rejects (the mock never returns a gRPC-Web
// frame) — that's expected: we gate on the settled promise, by which point the
// transport has already invoked fetch and the capture is populated.
async function captureRequest(
	run: (fetch: FetchLike) => Promise<unknown>,
): Promise<{
	url: string;
	authorization: string | null;
	sessionId: string | null;
}> {
	let url = "";
	let headers = new Headers();
	const fetch: FetchLike = async (input, init) => {
		url = String(input);
		headers = new Headers(init?.headers);
		throw new Error("captureRequest: short-circuit before response");
	};
	await expect(run(fetch)).rejects.toThrow();
	return {
		url,
		authorization: headers.get("authorization"),
		// Reported so the session-id cases can assert PRESENCE with a value and
		// ABSENCE as `null` — an empty-string header would read back as "" and is
		// a distinct (and forbidden) outcome from absent.
		sessionId: headers.get(posthogSessionHeader),
	};
}

describe("bearerAuthInterceptor", () => {
	test("sets authorization: Bearer <token> and forwards the same request to next", async () => {
		let calls = 0;
		let receivedReq: unknown;
		const sentinel = Symbol("next-response");
		const next = (req: unknown) => {
			calls++;
			receivedReq = req;
			return Promise.resolve(sentinel);
		};
		const req = { header: new Headers() };

		const result = await bearerAuthInterceptor("tok-abc")(next as never)(
			req as never,
		);

		expect(req.header.get("authorization")).toBe("Bearer tok-abc");
		expect(calls).toBe(1);
		expect(receivedReq).toBe(req);
		expect(result as unknown).toBe(sentinel);
	});

	test("replaces any pre-existing authorization rather than appending (set-once)", async () => {
		const next = (req: unknown) => Promise.resolve(req);
		const req = { header: new Headers({ authorization: "Bearer stale" }) };

		await bearerAuthInterceptor("fresh-token")(next as never)(req as never);

		// `Headers.set` replaces: the stale value is gone and the header reads
		// back as exactly the new token. An `.append` regression would leave the
		// comma-joined "Bearer stale, Bearer fresh-token", reddening this.
		expect(req.header.get("authorization")).toBe("Bearer fresh-token");
	});
});

describe("comms factory typed surface", () => {
	const unusedTransport: Transport = {
		unary: () => Promise.reject(new Error("transport unused in surface test")),
		stream: () => Promise.reject(new Error("transport unused in surface test")),
	};
	const okFetch: FetchLike = () => Promise.reject(new Error("fetch unused"));

	const clients: Array<[string, CommsClient]> = [
		["createCommsClient(transport)", createCommsClient(unusedTransport)],
		["createCommsWebClient(baseUrl)", createCommsWebClient("http://localhost")],
		[
			"createCommsWebClient(baseUrl, token)",
			createCommsWebClient("http://localhost", "tok"),
		],
		["createCommsClientOverFetch(fetch)", createCommsClientOverFetch(okFetch)],
		[
			"createCommsClientOverFetch(fetch, baseUrl, token)",
			createCommsClientOverFetch(okFetch, "http://localhost", "tok"),
		],
	];

	for (const [label, client] of clients) {
		test(`${label} exposes the CommsService rpc surface`, () => {
			expect(typeof client.createUser).toBe("function");
			expect(typeof client.listChannels).toBe("function");
			expect(typeof client.postMessage).toBe("function");
			expect(typeof client.subscribeComms).toBe("function");
		});
	}
});

describe("compass factory typed surface", () => {
	const unusedTransport: Transport = {
		unary: () => Promise.reject(new Error("transport unused in surface test")),
		stream: () => Promise.reject(new Error("transport unused in surface test")),
	};
	const okFetch: FetchLike = () => Promise.reject(new Error("fetch unused"));

	const clients: Array<[string, CompassClient]> = [
		["createCompassClient(transport)", createCompassClient(unusedTransport)],
		[
			"createCompassWebClient(baseUrl)",
			createCompassWebClient("http://localhost"),
		],
		[
			"createCompassWebClient(baseUrl, token)",
			createCompassWebClient("http://localhost", "tok"),
		],
		[
			"createCompassClientOverFetch(fetch)",
			createCompassClientOverFetch(okFetch),
		],
		[
			"createCompassClientOverFetch(fetch, baseUrl, token)",
			createCompassClientOverFetch(okFetch, "http://localhost", "tok"),
		],
	];

	for (const [label, client] of clients) {
		test(`${label} exposes the CompassService rpc surface`, () => {
			expect(typeof client.getServerInfo).toBe("function");
			expect(typeof client.subscribeEvents).toBe("function");
		});
	}
});

describe("bearer token is installed on the wire only when supplied", () => {
	test("comms with token routes to CommsService and carries the bearer header", async () => {
		const { url, authorization } = await captureRequest((fetch) =>
			createCommsClientOverFetch(
				fetch,
				"http://compass.localhost",
				"s3cret",
			).createUser({ handle: "ada" }),
		);
		expect(url).toBe(
			"http://compass.localhost/compass.v1.CommsService/CreateUser",
		);
		expect(authorization).toBe("Bearer s3cret");
	});

	test("comms without token sends no authorization header", async () => {
		const { url, authorization } = await captureRequest((fetch) =>
			createCommsClientOverFetch(fetch, "http://compass.localhost").createUser({
				handle: "ada",
			}),
		);
		expect(url).toBe(
			"http://compass.localhost/compass.v1.CommsService/CreateUser",
		);
		expect(authorization).toBeNull();
	});

	test("compass with token routes to CompassService and carries the bearer header", async () => {
		const { url, authorization } = await captureRequest((fetch) =>
			createCompassClientOverFetch(
				fetch,
				"http://compass.localhost",
				"s3cret",
			).getServerInfo({}),
		);
		expect(url).toBe(
			"http://compass.localhost/compass.v1.CompassService/GetServerInfo",
		);
		expect(authorization).toBe("Bearer s3cret");
	});

	test("compass without token sends no authorization header", async () => {
		const { url, authorization } = await captureRequest((fetch) =>
			createCompassClientOverFetch(
				fetch,
				"http://compass.localhost",
			).getServerInfo({}),
		);
		expect(url).toBe(
			"http://compass.localhost/compass.v1.CompassService/GetServerInfo",
		);
		expect(authorization).toBeNull();
	});
});

test("createCompassWebClient exposes the compass.v1 surface over gRPC-Web", () => {
	const client: CompassClient = createCompassWebClient("http://localhost");
	expect(typeof client.getServerInfo).toBe("function");
	expect(typeof client.subscribeEvents).toBe("function");
});

describe("createCompassWebTransport injects an optional fetch", () => {
	test("with no opts the transport is built as before (platform fetch)", () => {
		// The default (browser dev) path: no opts, so the transport carries the
		// compass.v1 surface exactly as today. The existing web-client/factory
		// tests above pin the no-opts wire behavior; this asserts the transport
		// builds and serves a client.
		const transport: Transport = createCompassWebTransport("http://localhost");
		const client = createCompassClient(transport);
		expect(typeof client.getServerInfo).toBe("function");
	});

	test("with opts.fetch the injected fetch is the one the transport calls", async () => {
		const { url, authorization } = await captureRequest((fetch) =>
			createCompassClient(
				createCompassWebTransport("http://compass.localhost", "s3cret", {
					// FetchLike is the request/response call signature the transport
					// uses; the opts shape names the full DOM `fetch`, so cast here.
					fetch: fetch as typeof globalThis.fetch,
				}),
			).getServerInfo({}),
		);
		// The capture is populated only if the transport called the injected
		// fetch: the shell-provided fetch is the seam the native app drives.
		expect(url).toBe(
			"http://compass.localhost/compass.v1.CompassService/GetServerInfo",
		);
		expect(authorization).toBe("Bearer s3cret");
	});
});

describe("empty token fails loud (misconfigured credential)", () => {
	// A throw-if-used fetch stub: the guard fires during factory construction,
	// before any request, so fetch must never be reached. If a factory ever gets
	// far enough to call it, this rejects — and the "throws synchronously at
	// construction" contract is already broken by then.
	const unusedFetch: FetchLike = () =>
		Promise.reject(new Error("fetch must not run: guard throws first"));

	// Each public factory called with token === "" — a misconfigured credential,
	// not an omitted one. The contract is that the throw is synchronous at
	// construction, so every row is a plain thunk we never await.
	const factories: Array<[string, () => unknown]> = [
		[
			'createCompassWebClient(baseUrl, "")',
			() => createCompassWebClient("http://compass.localhost", ""),
		],
		[
			'createCompassClientOverFetch(fetch, baseUrl, "")',
			() =>
				createCompassClientOverFetch(
					unusedFetch,
					"http://compass.localhost",
					"",
				),
		],
		[
			'createCommsWebClient(baseUrl, "")',
			() => createCommsWebClient("http://compass.localhost", ""),
		],
		[
			'createCommsClientOverFetch(fetch, baseUrl, "")',
			() =>
				createCommsClientOverFetch(unusedFetch, "http://compass.localhost", ""),
		],
	];

	for (const [label, callFactory] of factories) {
		test(`${label} throws synchronously with a meaningful message`, () => {
			// Loose match — ross owns the final wording — but it MUST name the
			// failure (empty / misconfigured), never a bare valueless throw.
			expect(callFactory).toThrow(/empty|misconfigured/i);
		});
	}

	test("token === undefined (arg omitted) is a deliberate no-auth, not a throw", () => {
		// Locks the three-way distinction: "" is misconfigured and throws, but an
		// omitted token stays a legitimate no-auth construction. A guard that
		// threw on any falsy token (undefined included) would redden this.
		expect(() =>
			createCompassWebClient("http://compass.localhost"),
		).not.toThrow();
		expect(() =>
			createCompassClientOverFetch(unusedFetch, "http://compass.localhost"),
		).not.toThrow();
		expect(() =>
			createCommsWebClient("http://compass.localhost"),
		).not.toThrow();
		expect(() =>
			createCommsClientOverFetch(unusedFetch, "http://compass.localhost"),
		).not.toThrow();
	});
});

describe("parseTraceResponse", () => {
	const traceId = "4bf92f3577b34da6a3ce929d0e0e4736";
	const spanId = "00f067aa0ba902b7";

	test("returns field 2 of a well-formed version-00 value", () => {
		expect(parseTraceResponse(`00-${traceId}-${spanId}-01`)).toBe(traceId);
	});

	test("returns the trace id ALONE, never the whole header value", () => {
		// The correlation key PostHog joins on is the bare 32-hex id; forwarding
		// the "00-…-…-…" string would match nothing in the trace backend.
		const value = `00-${traceId}-${spanId}-01`;
		expect(parseTraceResponse(value)).not.toBe(value);
		expect(parseTraceResponse(value)).toHaveLength(32);
	});

	test("rejects the all-zero trace id (invalid per W3C)", () => {
		// An unsampled/uninitialized span context renders as all zeros; stamping
		// it would collapse every such event onto one bogus trace.
		expect(
			parseTraceResponse(`00-${"0".repeat(32)}-${spanId}-01`),
		).toBeUndefined();
	});

	test("rejects a trace id that is 31 or 33 hex chars (exact-32 boundary)", () => {
		expect(
			parseTraceResponse(`00-${"a".repeat(31)}-${spanId}-01`),
		).toBeUndefined();
		expect(
			parseTraceResponse(`00-${"a".repeat(33)}-${spanId}-01`),
		).toBeUndefined();
	});

	test("rejects non-hex in the trace id position", () => {
		expect(
			parseTraceResponse(`00-${"z".repeat(32)}-${spanId}-01`),
		).toBeUndefined();
	});

	const rejected: Array<[string, string]> = [
		["empty value (header absent)", ""],
		["too few fields", `00-${traceId}-${spanId}`],
		["version-00 with an extra field", `00-${traceId}-${spanId}-01-extra`],
		["malformed span id", `00-${traceId}-abc-01`],
		["malformed flags", `00-${traceId}-${spanId}-0`],
		["non-hex version", `zz-${traceId}-${spanId}-01`],
		["version ff (reserved invalid per W3C)", `ff-${traceId}-${spanId}-01`],
		// Hex is lowercase per the spec and never normalized: an uppercase id
		// would be a DIFFERENT key from the one the trace backend indexed, so
		// accepting it (with or without a downcase) would stamp a key that joins
		// nothing. The positive lowercase case is covered above.
		["uppercase hex trace id", `00-${traceId.toUpperCase()}-${spanId}-01`],
	];

	for (const [label, value] of rejected) {
		test(`rejects ${label}`, () => {
			expect(parseTraceResponse(value)).toBeUndefined();
		});
	}

	test("accepts a FUTURE version carrying extra fields (forward compat)", () => {
		// Fields 1-3 are position-stable across versions, so a later version with
		// appended fields is still readable. A parser that hard-required "00"
		// would go blind the day the spec advances.
		expect(parseTraceResponse(`01-${traceId}-${spanId}-01-future`)).toBe(
			traceId,
		);
	});

	test("rejects a malformed FOUR-field value even at a future version", () => {
		// Forward compatibility is not a license to trust garbage: the trace-id
		// position must still hold 32 hex chars.
		expect(parseTraceResponse(`01-nope-${spanId}-01`)).toBeUndefined();
	});
});

describe("traceResponseInterceptor round-trips through a real transport", () => {
	const traceId = "4bf92f3577b34da6a3ce929d0e0e4736";
	const spanId = "00f067aa0ba902b7";

	// An in-memory compass.v1 server (the vendor's no-HTTP test path) whose
	// GetServerInfo handler sets whatever `traceresponse` the test wants — the
	// same header the Go otel interceptor sets in production. `sink` mirrors the
	// production choice: supplied, the trace interceptor is installed; omitted,
	// none is, which is the no-sink case.
	function fakeServer(
		traceResponse: string | undefined,
		sink?: TraceIdSink,
	): Transport {
		return createRouterTransport(
			({ service }) => {
				service(CompassService, {
					getServerInfo: (_req, ctx) => {
						if (traceResponse !== undefined) {
							ctx.responseHeader.set("traceresponse", traceResponse);
						}
						return create(GetServerInfoResponseSchema, {
							version: "1.2.3",
							apiVersion: "compass.v1",
						});
					},
				});
			},
			sink
				? { transport: { interceptors: [traceResponseInterceptor(sink)] } }
				: undefined,
		);
	}

	test("a valid traceresponse populates the sink with only the 32-hex id", async () => {
		const sink: TraceIdSink = { current: undefined };
		const header = `00-${traceId}-${spanId}-01`;

		await createCompassClient(fakeServer(header, sink)).getServerInfo({});

		expect(sink.current).toBe(traceId);
		// The whole "00-…" value must NOT leak through: that is the regression
		// this asserts against, not merely "something was written".
		expect(sink.current).not.toBe(header);
	});

	test("an absent header leaves the sink at its prior value", async () => {
		// A reply that carried no trace id is not evidence the previous trace
		// ended, so last-known-good stays. A clear-on-absent regression would
		// blank this and drop correlation for most events.
		const sink: TraceIdSink = { current: traceId };

		await createCompassClient(fakeServer(undefined, sink)).getServerInfo({});

		expect(sink.current).toBe(traceId);
	});

	test("an unparseable header leaves the sink at its prior value", async () => {
		const sink: TraceIdSink = { current: traceId };

		await createCompassClient(fakeServer("garbage", sink)).getServerInfo({});

		expect(sink.current).toBe(traceId);
	});

	test("the response still reaches the caller unchanged", async () => {
		// The interceptor observes; it must not swallow or reshape the reply.
		const sink: TraceIdSink = { current: undefined };

		const resp = await createCompassClient(
			fakeServer(`00-${traceId}-${spanId}-01`, sink),
		).getServerInfo({});

		expect(resp.version).toBe("1.2.3");
		expect(resp.apiVersion).toBe("compass.v1");
	});

	test("a STREAMING call carries every message AND still records the id", async () => {
		// The interceptor must read the RESPONSE HEADER only. A refactor that
		// reached into `res.message` to find the trace id would type-check for
		// unary and silently break the comms stream — the app's primary data
		// source — with every other test in this file still green.
		const sink: TraceIdSink = { current: undefined };
		const transport = createRouterTransport(
			({ service }) => {
				service(CommsService, {
					subscribeComms: async function* (_req, ctx) {
						ctx.responseHeader.set(
							"traceresponse",
							`00-${traceId}-${spanId}-01`,
						);
						for (const seq of [1n, 2n, 3n]) {
							yield create(SubscribeCommsResponseSchema, { seq });
						}
					},
				});
			},
			{ transport: { interceptors: [traceResponseInterceptor(sink)] } },
		);

		const seqs: bigint[] = [];
		for await (const msg of createCommsClient(transport).subscribeComms({
			sinceSeq: 0n,
		})) {
			seqs.push(msg.seq);
		}

		expect(seqs).toEqual([1n, 2n, 3n]);
		expect(sink.current).toBe(traceId);
	});

	test("bearer and trace interceptors COMPOSE on one call", async () => {
		// The two concerns are installed by one composer, and the bearer tests
		// above never pass a sink. Both halves of a fully-configured client are
		// asserted here in a single round trip: the request carried the token
		// outbound and the reply's trace id came back inbound.
		const sink: TraceIdSink = { current: undefined };
		// A holder rather than a `let`: assigned only inside the handler closure,
		// a `let` initialized to null narrows to `null` at the assertion below and
		// the comparison stops type-checking.
		const captured: { authorization: string | null } = { authorization: null };
		const transport = createRouterTransport(
			({ service }) => {
				service(CompassService, {
					getServerInfo: (_req, ctx) => {
						captured.authorization = ctx.requestHeader.get("authorization");
						ctx.responseHeader.set(
							"traceresponse",
							`00-${traceId}-${spanId}-01`,
						);
						return create(GetServerInfoResponseSchema, {
							version: "1.2.3",
							apiVersion: "compass.v1",
						});
					},
				});
			},
			// The interceptor list the production composer builds for
			// (token, sink) — read off the transport factory rather than
			// hand-assembled, so this pins the real composition and its order.
			{ transport: { interceptors: composedInterceptors("tok", sink) } },
		);

		await createCompassClient(transport).getServerInfo({});

		expect(captured.authorization).toBe("Bearer tok");
		expect(sink.current).toBe(traceId);
	});

	test("a SECOND call overwrites the first call's trace id", async () => {
		// The sink holds the MOST RECENT reply's id, so the interceptor must
		// write on every response. A write-once regression (or a one-shot guard)
		// would leave the first id in place and pass every single-call test here.
		const first = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa";
		const second = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb";
		const sink: TraceIdSink = { current: undefined };

		await createCompassClient(
			fakeServer(`00-${first}-${spanId}-01`, sink),
		).getServerInfo({});
		expect(sink.current).toBe(first);

		await createCompassClient(
			fakeServer(`00-${second}-${spanId}-01`, sink),
		).getServerInfo({});

		expect(sink.current).toBe(second);
	});
});

// A counting `next` returning a Symbol sentinel, plus a REAL `Headers` request
// — the direct-interceptor seam, following the bearerAuthInterceptor precedent
// above. The real `Headers` is what makes a `Headers.set` TypeError reachable,
// and the sentinel is what proves nothing threw.
//
// This seam exists because `captureRequest` CANNOT witness a would-throw case:
// its capturing fetch always throws, so it must gate on
// `rejects.toThrow()` — which a broken interceptor's own TypeError satisfies —
// and its header readback is a pre-initialized empty `Headers`, so absence also
// passes. Both assertions go green on exactly the defect. Never route a
// would-throw value onto the capture seam.
function directSeam() {
	let calls = 0;
	const sentinel = Symbol("next-response");
	const next = (_req: unknown) => {
		calls++;
		return Promise.resolve(sentinel);
	};
	const req = { header: new Headers() };
	return {
		req,
		sentinel,
		next,
		get calls() {
			return calls;
		},
	};
}

describe("sessionIdInterceptor stamps only a sendable session id", () => {
	const validId = "0199a1b2-3c4d-7e8f-9012-3456789abcde";

	// The header name is a WIRE CONTRACT with the server's J1 interceptor
	// (go/internal/otel/interceptor.go, PostHogSessionHeader) and is CORS-allowed
	// by exactly that string in the network door. Every other case reads the
	// header through the exported const, so all of them would stay green under a
	// rename; only this pins the literal both ends must agree on.
	test("the header name is exactly X-POSTHOG-SESSION-ID", () => {
		expect(posthogSessionHeader).toBe("X-POSTHOG-SESSION-ID");
	});

	test("a valid id is carried on the request as X-POSTHOG-SESSION-ID", async () => {
		const { url, sessionId } = await captureRequest((fetch) =>
			createCompassClient(
				createCompassWebTransport("http://compass.localhost", undefined, {
					fetch: fetch as typeof globalThis.fetch,
					sessionId: () => validId,
				}),
			).getServerInfo({}),
		);

		expect(url).toBe(
			"http://compass.localhost/compass.v1.CompassService/GetServerInfo",
		);
		expect(sessionId).toBe(validId);
	});

	// Analytics off (NoopAnalytics.sessionId() ⇒ undefined) must send NO header
	// at all, not an empty one: an empty header spends wire bytes asserting a
	// correlation that does not exist, and the server trim-drops it anyway.
	test("getter returns undefined ⇒ the header is absent, not empty", async () => {
		const { sessionId } = await captureRequest((fetch) =>
			createCompassClient(
				createCompassWebTransport("http://compass.localhost", undefined, {
					fetch: fetch as typeof globalThis.fetch,
					sessionId: () => undefined,
				}),
			).getServerInfo({}),
		);

		expect(sessionId).toBeNull();
	});

	// posthog-js's get_session_id() legitimately returns "" before it is fully
	// initialized; the guard's `+` quantifier rejects it.
	test('getter returns "" ⇒ the header is absent', async () => {
		const { sessionId } = await captureRequest((fetch) =>
			createCompassClient(
				createCompassWebTransport("http://compass.localhost", undefined, {
					fetch: fetch as typeof globalThis.fetch,
					sessionId: () => "",
				}),
			).getServerInfo({}),
		);

		expect(sessionId).toBeNull();
	});

	test("201 ASCII chars ⇒ over the cap, the header is absent", async () => {
		const { sessionId } = await captureRequest((fetch) =>
			createCompassClient(
				createCompassWebTransport("http://compass.localhost", undefined, {
					fetch: fetch as typeof globalThis.fetch,
					sessionId: () => "a".repeat(201),
				}),
			).getServerInfo({}),
		);

		expect(sessionId).toBeNull();
	});

	// The boundary the `<=` in `id.length <= MAX_SESSION_ID_LEN` owns: a `<`
	// typo reddens HERE and nowhere else, because the 201 case stays green under
	// both operators. 200 is legal on the server too — its check is
	// `len(id) > maxSessionIDLen` — so refusing it would be needlessly stricter
	// than the wire contract.
	test("exactly 200 ASCII chars ⇒ at the cap, the header is PRESENT", async () => {
		const atCap = "a".repeat(200);
		const { sessionId } = await captureRequest((fetch) =>
			createCompassClient(
				createCompassWebTransport("http://compass.localhost", undefined, {
					fetch: fetch as typeof globalThis.fetch,
					sessionId: () => atCap,
				}),
			).getServerInfo({}),
		);

		expect(sessionId).toBe(atCap);
	});

	// A Latin-1 value does NOT throw — `Headers.set` accepts U+0080–U+00FF — so
	// the capture seam is the right one here and the readback IS the whole
	// assertion. What the ASCII guard buys: without it a browser emits this as
	// a single raw high byte, which fails the server's utf8.ValidString check,
	// so the id is silently DROPPED. Not asserting transmitted bytes here —
	// that is a transport-encoding property, not this interceptor's contract.
	test("a Latin-1 value (sess-é) ⇒ the header is absent", async () => {
		const { sessionId } = await captureRequest((fetch) =>
			createCompassClient(
				createCompassWebTransport("http://compass.localhost", undefined, {
					fetch: fetch as typeof globalThis.fetch,
					sessionId: () => "sess-é",
				}),
			).getServerInfo({}),
		);

		expect(sessionId).toBeNull();
	});

	// ONE transport, hence ONE interceptor instance, driven across two requests
	// with a different capturing fetch each time. Building a fresh transport per
	// request would let a construction-time memo re-read the getter and pass
	// both multi-request cases below, which is exactly the defect they exist to
	// catch — so the fetch is indirected through a mutable slot instead.
	function oneClientAcrossRequests(sessionId: () => string | undefined) {
		let currentFetch: FetchLike = () =>
			Promise.reject(new Error("oneClientAcrossRequests: no fetch installed"));
		const client = createCompassClient(
			createCompassWebTransport("http://compass.localhost", undefined, {
				fetch: ((input: RequestInfo | URL, init?: RequestInit) =>
					currentFetch(input, init)) as typeof globalThis.fetch,
				sessionId,
			}),
		);
		return (fetch: FetchLike) => {
			currentFetch = fetch;
			return client.getServerInfo({});
		};
	}

	test("the getter is called per request, so a fresh value is sent", async () => {
		const ids = ["sess-first", "sess-second"];
		let call = 0;
		const build = oneClientAcrossRequests(() => ids[call++]);

		const first = await captureRequest(build);
		const second = await captureRequest(build);

		expect(first.sessionId).toBe("sess-first");
		expect(second.sessionId).toBe("sess-second");
	});

	// Self-healing, the direction forward-propagation does NOT cover: a
	// construction-time cache or a first-value memo still passes the laziness
	// case above while failing this one, because it would pin the early empty
	// value forever.
	test('"" on the first call then a valid id on the second ⇒ only the second carries the header', async () => {
		const ids: (string | undefined)[] = ["", validId];
		let call = 0;
		const build = oneClientAcrossRequests(() => ids[call++]);

		const first = await captureRequest(build);
		const second = await captureRequest(build);

		expect(first.sessionId).toBeNull();
		expect(second.sessionId).toBe(validId);
	});

	// The guard is deliberately NARROWER than `Headers.set`: set accepts a space,
	// a tab, and DEL, and this rejects all three. Nothing else asserts that extra
	// narrowing, so widening the class one codepoint (0x20 for 0x21) would put a
	// space-bearing value on the wire unnoticed. Capture seam is correct here —
	// none of these makes `Headers.set` throw, so the readback IS the assertion.
	const narrowerThanHeadersSet: [string, string][] = [
		["a space", "sess id"],
		["a tab", "sess\tid"],
		["a DEL byte", "sess\x7f"],
	];

	for (const [label, value] of narrowerThanHeadersSet) {
		test(`${label} ⇒ the header is absent, though Headers.set would take it`, async () => {
			const { sessionId } = await captureRequest((fetch) =>
				createCompassClient(
					createCompassWebTransport("http://compass.localhost", undefined, {
						fetch: fetch as typeof globalThis.fetch,
						sessionId: () => value,
					}),
				).getServerInfo({}),
			);

			expect(sessionId).toBeNull();
		});
	}
});

// Three values that make `Headers.set` THROW a TypeError, so a missing guard
// would fail the whole RPC rather than merely lose a correlation key. Each case
// asserts the full triple: header absent, `next` ran exactly once, and the
// awaited result IS the sentinel — the third is load-bearing, since it cannot
// pass if the interceptor threw before reaching `next`. Values are kept SHORT
// on purpose: a long non-ASCII value is rejected by the length cap first and
// never reaches `header.set`, so it could not exercise the throw at all.
describe("sessionIdInterceptor rejects would-throw values without failing the request", () => {
	const wouldThrow: [string, string][] = [
		["a non-Latin-1 id (> U+00FF)", "sess-日本語"],
		["a value containing CRLF", "sess\r\nx"],
		["a lone surrogate", "\uD800"],
	];

	for (const [label, value] of wouldThrow) {
		test(`${label} ⇒ no header, next still ran, nothing threw`, async () => {
			const seam = directSeam();

			const result = await sessionIdInterceptor(() => value)(
				seam.next as never,
			)(seam.req as never);

			expect(seam.req.header.get(posthogSessionHeader)).toBeNull();
			expect(seam.calls).toBe(1);
			expect(result as unknown).toBe(seam.sentinel);
		});
	}
});

// `undefined` and `""` are absent-value cases rather than values `Headers.set`
// rejects — but the GUARD can throw on them, which puts them in the class above:
// a bare `SENDABLE.test(undefined)` coerces to the string "undefined" and
// PASSES, so evaluation reaches `id.length` on undefined and throws. So the
// capture seam cannot witness them either, and `undefined` is the case that
// matters most: it is the shipped default (`NoopAnalytics.sessionId()`), so a
// guard that throws on it fails EVERY request whenever analytics is off.
describe("sessionIdInterceptor survives an absent session id", () => {
	const absent: [string, string | undefined][] = [
		["undefined — analytics off, the shipped default", undefined],
		['"" — posthog before it has initialized', ""],
	];

	for (const [label, value] of absent) {
		test(`${label} ⇒ no header, next still ran, nothing threw`, async () => {
			const seam = directSeam();

			const result = await sessionIdInterceptor(() => value)(
				seam.next as never,
			)(seam.req as never);

			expect(seam.req.header.get(posthogSessionHeader)).toBeNull();
			expect(seam.calls).toBe(1);
			expect(result as unknown).toBe(seam.sentinel);
		});
	}
});

describe("callInterceptors installs only what was asked for", () => {
	// The behavioral claim of the omitted-means-off rule: neither concern
	// requested ⇒ the transport is handed `undefined`, NOT an empty list, so an
	// unconfigured client's transport config is byte-identical to before either
	// interceptor existed. Nothing but the transport factory's own argument can
	// witness that, so the factory is spied and its argument read — the same
	// seam apps/ui/src/live/provider.test.ts uses on this package's factory.
	test("no token and no sink ⇒ interceptors is undefined, not []", () => {
		const opts = transportOptionsFor(() =>
			createCompassWebTransport("http://compass.localhost"),
		);

		expect(opts.interceptors).toBeUndefined();
	});

	test("a token alone ⇒ exactly one interceptor", () => {
		const opts = transportOptionsFor(() =>
			createCompassWebTransport("http://compass.localhost", "tok"),
		);

		expect(opts.interceptors).toHaveLength(1);
	});

	test("a sink alone ⇒ exactly one interceptor", () => {
		const sink: TraceIdSink = { current: undefined };
		const opts = transportOptionsFor(() =>
			createCompassWebTransport("http://compass.localhost", undefined, {
				traceSink: sink,
			}),
		);

		expect(opts.interceptors).toHaveLength(1);
	});

	test("token and sink ⇒ both, bearer first", () => {
		const sink: TraceIdSink = { current: undefined };
		const opts = transportOptionsFor(() =>
			createCompassWebTransport("http://compass.localhost", "tok", {
				traceSink: sink,
			}),
		);

		expect(opts.interceptors).toHaveLength(2);
	});

	// The direction that catches an append placed inside the old early return:
	// `callInterceptors` used to be `const bearer = ...; if (!traceSink) return
	// bearer;`, so a session interceptor appended after that guard is skipped
	// entirely whenever no trace sink is configured — and every OTHER
	// membership direction below still passes. This is the only one that reddens.
	test("a sessionId getter alone ⇒ exactly one interceptor", () => {
		const opts = transportOptionsFor(() =>
			createCompassWebTransport("http://compass.localhost", undefined, {
				sessionId: () => "sess-1",
			}),
		);

		expect(opts.interceptors).toHaveLength(1);
	});

	test("sessionId and sink ⇒ exactly two interceptors", () => {
		const sink: TraceIdSink = { current: undefined };
		const opts = transportOptionsFor(() =>
			createCompassWebTransport("http://compass.localhost", undefined, {
				traceSink: sink,
				sessionId: () => "sess-1",
			}),
		);

		expect(opts.interceptors).toHaveLength(2);
	});

	test("token, sink and sessionId ⇒ exactly three interceptors", () => {
		const sink: TraceIdSink = { current: undefined };
		const opts = transportOptionsFor(() =>
			createCompassWebTransport("http://compass.localhost", "tok", {
				traceSink: sink,
				sessionId: () => "sess-1",
			}),
		);

		expect(opts.interceptors).toHaveLength(3);
	});

	// Composition ORDER, not just membership. The restructure from an early
	// return to an accumulating list made order a fresh degree of freedom, and
	// every case above is order-blind (`toHaveLength` counts). Order is benign
	// TODAY — the session interceptor writes a request header and the trace one
	// reads a response header, so they commute — and this pins it so the suite
	// notices if that stops being true. Each interceptor is identified by its
	// observable effect: position 0 stamps authorization, position 2 stamps the
	// session header, so trace is the middle by elimination.
	test("token, sink and sessionId ⇒ bearer first, session last", async () => {
		const sink: TraceIdSink = { current: undefined };
		const opts = transportOptionsFor(() =>
			createCompassWebTransport("http://compass.localhost", "tok", {
				traceSink: sink,
				sessionId: () => "sess-1",
			}),
		);
		const interceptors = opts.interceptors ?? [];

		const first = directSeam();
		await interceptors[0]?.(first.next as never)(first.req as never);
		expect(first.req.header.get("authorization")).toBe("Bearer tok");
		expect(first.req.header.get(posthogSessionHeader)).toBeNull();

		const last = directSeam();
		await interceptors[2]?.(last.next as never)(last.req as never);
		expect(last.req.header.get(posthogSessionHeader)).toBe("sess-1");
		expect(last.req.header.get("authorization")).toBeNull();
	});
});
