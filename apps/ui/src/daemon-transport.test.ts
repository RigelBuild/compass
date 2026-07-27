/// <reference types="bun" />
// Contracts defended here (the webview→daemon fetch adapter, daemon-transport.ts):
//  - daemonFetch forwards the request the gRPC-Web transport built to the
//    `compass_rpc` command: HTTP method, the URL's *path+query only* (origin
//    dropped), the headers, and the body as a byte array.
//  - the ordered head→body→end Channel frames reassemble into a `Response` whose
//    status/headers come from the head frame and whose streamed body is the
//    base64-decoded body chunks concatenated in order; `end` closes the stream.
//  - an `error` frame *before* the head rejects the returned fetch promise; an
//    `error` frame *after* the head errors the response's body stream instead.
//  - a caller-minted `requestId` correlates the `compass_rpc` call with the
//    Rust proxy task; cancelling the response stream, or aborting the request's
//    signal, fires `compass_rpc_cancel` with that *same* id — exactly once (a
//    `canceled` guard), after which late Channel frames are dropped. An
//    already-aborted signal cancels up front and never delivers a body.
//
// The real `@tauri-apps/api/core` `Channel`/`invoke` reach for
// `window.__TAURI_INTERNALS__` (a webview global absent under bun), so the module
// is swapped for hand-written fakes: `invoke` captures the payload and hands the
// test the `Channel` instance so it can drive response frames, exactly the way
// the Rust bridge would.

import { beforeEach, describe, expect, mock, test } from "bun:test";

// Mirrors the Rust `ResponseFrame` wire (bridge.rs) — the frames the daemon
// sends over the Channel and this adapter reassembles.
type ResponseFrame =
	| { kind: "head"; status: number; headers: [string, string][] }
	| { kind: "body"; chunk: string }
	| { kind: "end" }
	| { kind: "error"; message: string };

// The exact argument shape daemonFetch passes to invoke("compass_rpc", …).
type CompassRpcArgs = {
	requestId: string;
	method: string;
	url: string;
	headers: { name: string; value: string }[];
	body: number[];
	channel: FakeChannel<ResponseFrame>;
};

type InvokeCall = { cmd: string; args: CompassRpcArgs };

/** A stand-in for the Tauri `Channel`: daemonFetch sets `onmessage`, the test
 *  drives it. No `window.__TAURI_INTERNALS__` dependency, unlike the real one. */
class FakeChannel<T = unknown> {
	onmessage: (message: T) => void = () => {};
}

// Reset per test; the mock closure reads the live bindings.
let invokeCalls: InvokeCall[] = [];
let invokeMade: Promise<InvokeCall>;
let resolveInvokeMade: (call: InvokeCall) => void;

mock.module("@tauri-apps/api/core", () => ({
	Channel: FakeChannel,
	invoke: (cmd: string, args: CompassRpcArgs) => {
		const call: InvokeCall = { cmd, args };
		invokeCalls.push(call);
		resolveInvokeMade(call);
		// The real invoke resolves when the command returns (after End); the
		// adapter drives the body off the Channel, not this promise, so resolving
		// success here is faithful and never triggers the pre-head `.catch`.
		return Promise.resolve();
	},
}));

// Loaded after the mock is installed: a static import would hoist above
// mock.module and bind the real `@tauri-apps/api/core`. (Sanctioned
// dynamic-import exception: exercising the module-mock boundary.)
const { daemonFetch } = await import("./daemon-transport");

beforeEach(() => {
	invokeCalls = [];
	invokeMade = new Promise<InvokeCall>((resolve) => {
		resolveInvokeMade = resolve;
	});
});

/** Standard-base64 of a byte sequence, for building body-frame chunks. */
function b64(bytes: number[]): string {
	return btoa(String.fromCharCode(...bytes));
}

/** Assert a nullable value is present, narrowing it; throws when absent, so a
 * missing value fails the test loudly instead of an optional chain silently
 * no-op'ing past a regression. */
function present<T>(value: T | null | undefined, what: string): T {
	if (value == null) throw new Error(`expected ${what} to be present`);
	return value;
}

/** Read a response body stream fully into a flat byte array. */
async function readAllBytes(response: Response): Promise<number[]> {
	const reader = present(response.body, "a response body stream").getReader();
	const out: number[] = [];
	for (;;) {
		const { done, value } = await reader.read();
		if (done) break;
		out.push(...value);
	}
	return out;
}

describe("daemonFetch", () => {
	test("forwards method, path+query, headers, and body bytes to compass_rpc", async () => {
		// A high byte (250) guards against sign/charcode mangling of the body.
		const body = new Uint8Array([1, 2, 3, 250]);
		const fetched = daemonFetch(
			"https://daemon.invalid/compass.v1.CompassService/GetDaemonInfo?a=1&b=two",
			{
				method: "POST",
				headers: {
					"content-type": "application/grpc-web+proto",
					"x-grpc-web": "1",
				},
				body,
			},
		);

		const call = await invokeMade;
		expect(call.cmd).toBe("compass_rpc");
		expect(call.args.method).toBe("POST");
		// Origin dropped; only the daemon-relative path + query is forwarded.
		expect(call.args.url).toBe(
			"/compass.v1.CompassService/GetDaemonInfo?a=1&b=two",
		);
		const header = (name: string) =>
			call.args.headers.find((h) => h.name === name)?.value;
		expect(header("content-type")).toBe("application/grpc-web+proto");
		expect(header("x-grpc-web")).toBe("1");
		expect(call.args.body).toEqual([1, 2, 3, 250]);

		// Close out the fetch so it doesn't dangle.
		call.args.channel.onmessage({ kind: "head", status: 200, headers: [] });
		call.args.channel.onmessage({ kind: "end" });
		await fetched;
	});

	test("reassembles head→body→end into a streaming Response with the decoded body", async () => {
		const fetched = daemonFetch(
			"https://daemon.invalid/compass.v1.CompassService/SubscribeEvents",
		);
		const { channel } = (await invokeMade).args;

		channel.onmessage({
			kind: "head",
			status: 201,
			headers: [
				["content-type", "application/grpc-web+proto"],
				["x-frame", "one"],
			],
		});

		const response = await fetched;
		expect(response.status).toBe(201);
		expect(response.headers.get("content-type")).toBe(
			"application/grpc-web+proto",
		);
		expect(response.headers.get("x-frame")).toBe("one");

		// Two chunks, one carrying a 0xFF byte, must decode and concatenate in the
		// order received; `end` then closes the stream so the read completes.
		channel.onmessage({ kind: "body", chunk: b64([104, 105, 255]) });
		channel.onmessage({ kind: "body", chunk: b64([0, 1, 2]) });
		channel.onmessage({ kind: "end" });

		expect(await readAllBytes(response)).toEqual([104, 105, 255, 0, 1, 2]);
	});

	test("rejects the fetch when an error frame arrives before the head", async () => {
		const fetched = daemonFetch(
			"https://daemon.invalid/compass.v1.CompassService/GetDaemonInfo",
		);
		const { channel } = (await invokeMade).args;

		channel.onmessage({ kind: "error", message: "daemon socket refused" });

		await expect(fetched).rejects.toThrow("daemon socket refused");
	});

	test("errors the response body stream when an error frame arrives after the head", async () => {
		const fetched = daemonFetch(
			"https://daemon.invalid/compass.v1.CompassService/SubscribeEvents",
		);
		const { channel } = (await invokeMade).args;

		// Head first: the fetch resolves, so a later failure can no longer reject
		// it — it must surface through the body stream instead.
		channel.onmessage({ kind: "head", status: 200, headers: [] });
		const response = await fetched;

		channel.onmessage({ kind: "body", chunk: b64([9, 9]) });
		channel.onmessage({ kind: "error", message: "mid-stream daemon failure" });

		await expect(readAllBytes(response)).rejects.toThrow(
			"mid-stream daemon failure",
		);
	});

	// (a) The gRPC-Web transport dropping a subscription cancels the reader; that
	// must tear down the upstream proxy, keyed by the very id the rpc was sent with.
	test("(a) cancelling the response stream fires compass_rpc_cancel with the same requestId", async () => {
		const fetched = daemonFetch(
			"https://daemon.invalid/compass.v1.CompassService/SubscribeEvents",
		);
		const { channel } = (await invokeMade).args;
		channel.onmessage({ kind: "head", status: 200, headers: [] });
		const response = await fetched;

		const rpc = present(
			invokeCalls.find((c) => c.cmd === "compass_rpc"),
			"a compass_rpc call",
		);
		// Consumer stops reading: reader.cancel() runs the stream's cancel hook.
		await present(response.body, "a response body stream").getReader().cancel();

		const cancels = invokeCalls.filter((c) => c.cmd === "compass_rpc_cancel");
		expect(cancels).toHaveLength(1);
		expect(cancels[0].args.requestId).toBe(rpc.args.requestId);
	});

	// (b) An abort after the head is live must both stop the proxy (once) and
	// surface the abort reason through the already-resolved response's stream.
	test("(b) a signal aborted mid-stream fires cancel once and errors the stream", async () => {
		const controller = new AbortController();
		const fetched = daemonFetch(
			"https://daemon.invalid/compass.v1.CompassService/SubscribeEvents",
			{ signal: controller.signal },
		);
		const { channel } = (await invokeMade).args;
		channel.onmessage({ kind: "head", status: 200, headers: [] });
		const response = await fetched;

		const rpc = present(
			invokeCalls.find((c) => c.cmd === "compass_rpc"),
			"a compass_rpc call",
		);
		controller.abort(new DOMException("navigated away", "AbortError"));

		const cancels = invokeCalls.filter((c) => c.cmd === "compass_rpc_cancel");
		expect(cancels).toHaveLength(1);
		expect(cancels[0].args.requestId).toBe(rpc.args.requestId);
		// The stream carries the abort reason to the consumer.
		await expect(readAllBytes(response)).rejects.toThrow("navigated away");
	});

	// (c) Aborted before the call even starts: the fetch contract says reject
	// immediately with the abort reason, and don't start the RPC at all (no point
	// firing a call we'd cancel on the same tick). A bare cancel-without-reject
	// would leave the promise hanging forever, so this pins the reject.
	test("(c) an already-aborted signal rejects up front without starting the RPC", async () => {
		const controller = new AbortController();
		controller.abort(new DOMException("gone", "AbortError"));
		const fetched = daemonFetch(
			"https://daemon.invalid/compass.v1.CompassService/SubscribeEvents",
			{ signal: controller.signal },
		);

		// The returned promise rejects with the signal's abort reason.
		await expect(fetched).rejects.toThrow("gone");
		// No RPC is started for an already-dead request — neither the proxy call
		// nor a cancel for a request the Rust side never registered.
		expect(invokeCalls).toHaveLength(0);
	});

	// (d) Two independent teardown paths racing at once must still hit the daemon
	// with a single cancel — the `canceled` flag collapses the duplicate.
	test("(d) stream-cancel and signal-abort together fire compass_rpc_cancel exactly once", async () => {
		const controller = new AbortController();
		const fetched = daemonFetch(
			"https://daemon.invalid/compass.v1.CompassService/SubscribeEvents",
			{ signal: controller.signal },
		);
		const { channel } = (await invokeMade).args;
		channel.onmessage({ kind: "head", status: 200, headers: [] });
		const response = await fetched;

		await present(response.body, "a response body stream").getReader().cancel();
		controller.abort(new DOMException("also aborted", "AbortError"));

		const cancels = invokeCalls.filter((c) => c.cmd === "compass_rpc_cancel");
		expect(cancels).toHaveLength(1);
	});
});
