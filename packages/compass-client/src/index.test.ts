import { describe, expect, test } from "bun:test";
import {
	bearerAuthInterceptor,
	type CommsClient,
	type CompassClient,
	createCommsClient,
	createCommsClientOverFetch,
	createCommsWebClient,
	createCompassClient,
	createCompassClientOverFetch,
	createCompassWebClient,
	type Transport,
} from "./index";

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
): Promise<{ url: string; authorization: string | null }> {
	let url = "";
	let headers = new Headers();
	const fetch: FetchLike = async (input, init) => {
		url = String(input);
		headers = new Headers(init?.headers);
		throw new Error("captureRequest: short-circuit before response");
	};
	await expect(run(fetch)).rejects.toThrow();
	return { url, authorization: headers.get("authorization") };
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
