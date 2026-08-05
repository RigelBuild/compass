/// <reference types="bun" />
// Contracts defended here (the webview→daemon fetch adapter, daemon-transport.ts):
//  - createDaemonFetch(ipc) forwards the request the gRPC-Web transport built to
//    the ShellIpc `rpc` call: the URL's *path+query only* (origin dropped), the
//    headers, and the body as a byte array.
//  - the ordered head→body→end frames reassemble into a `Response` whose
//    status/headers come from the head frame and whose streamed body is the
//    base64-decoded body chunks concatenated in order; `end` closes the stream.
//    A multi-body stream yields each decoded chunk in order (SubscribeEvents).
//  - a caller-minted `requestId` correlates the `rpc` call with the proxy task;
//    cancelling the response stream, or aborting the request's signal, fires
//    `ipc.cancel` with that *same* id.
//
// The seam is driven by a hand-written fake `ShellIpc` — no Tauri, no
// `window.__TAURI_INTERNALS__`, no network — exactly the way any shell binding
// would drive it: it captures the `rpc` args + the `onFrame` callback so the
// test can push response frames, and records `cancel(requestId)` calls.

import { beforeEach, describe, expect, test } from "bun:test";
import {
	createDaemonFetch,
	type ResponseFrame,
	type ShellIpc,
} from "./daemon-transport";

/** The captured state of a single `ipc.rpc(...)` invocation. */
type RpcCall = {
	args: {
		requestId: string;
		path: string;
		headers: { name: string; value: string }[];
		body: number[];
	};
	onFrame: (frame: ResponseFrame) => void;
};

/** A stand-in for a shell's IPC binding: `rpc` captures its args + `onFrame`
 *  (which the test drives) and resolves `rpcMade` so the test can grab them;
 *  `cancel` records the requestId it was called with. `rpc` returns a promise
 *  that never settles on its own — the frames the test pushes via `onFrame`
 *  drive the Response, exactly as a real shell binding behaves. */
class FakeShellIpc implements ShellIpc {
	rpcCalls: RpcCall[] = [];
	cancelCalls: string[] = [];
	private readonly made = Promise.withResolvers<RpcCall>();
	/** Resolves once `rpc` has been called, handing the test the captured call. */
	readonly rpcMade = this.made.promise;

	rpc(
		args: RpcCall["args"],
		onFrame: (frame: ResponseFrame) => void,
	): Promise<void> {
		const call: RpcCall = { args, onFrame };
		this.rpcCalls.push(call);
		this.made.resolve(call);
		// Never settles on its own: the transport stays open until frames end it.
		return new Promise<void>(() => {});
	}

	cancel(requestId: string): void {
		this.cancelCalls.push(requestId);
	}
}

let ipc: FakeShellIpc;

beforeEach(() => {
	ipc = new FakeShellIpc();
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
	const reader = present(response.body, "response body").getReader();
	const chunks: number[] = [];
	for (;;) {
		const { done, value } = await reader.read();
		if (done) break;
		if (value) chunks.push(...value);
	}
	return chunks;
}

describe("createDaemonFetch", () => {
	test("forwards path+query, headers, and body bytes to ipc.rpc, then a unary head→body→end resolves a Response", async () => {
		const daemonFetch = createDaemonFetch(ipc);
		// A high byte (250) guards against sign/charcode mangling of the body.
		const body = new Uint8Array([1, 2, 3, 250]);
		const fetched = daemonFetch(
			"https://daemon.invalid/compass.v1.CompassService/GetDaemonInfo?a=1&b=two",
			{
				method: "POST",
				headers: { "content-type": "application/grpc-web+proto", "x-k": "v" },
				body,
			},
		);

		const { args, onFrame } = await ipc.rpcMade;

		// path+query only (origin dropped), headers forwarded, body as byte array.
		expect(args.path).toBe(
			"/compass.v1.CompassService/GetDaemonInfo?a=1&b=two",
		);
		const headerMap = new Map(args.headers.map((h) => [h.name, h.value]));
		expect(headerMap.get("content-type")).toBe("application/grpc-web+proto");
		expect(headerMap.get("x-k")).toBe("v");
		expect(args.body).toEqual([1, 2, 3, 250]);
		expect(typeof args.requestId).toBe("string");
		expect(args.requestId.length).toBeGreaterThan(0);

		// Unary: one head + one body + end.
		onFrame({ kind: "head", status: 200, headers: [["grpc-status", "0"]] });
		onFrame({ kind: "body", chunk: b64([9, 8, 7]) });
		onFrame({ kind: "end" });

		const response = await fetched;
		expect(response.status).toBe(200);
		expect(response.headers.get("grpc-status")).toBe("0");
		expect(await readAllBytes(response)).toEqual([9, 8, 7]);
	});

	test("multi-frame stream: head + multiple body frames + end yield each decoded chunk in order", async () => {
		const daemonFetch = createDaemonFetch(ipc);
		const fetched = daemonFetch(
			"https://daemon.invalid/compass.v1.CompassService/SubscribeEvents",
		);
		const { onFrame } = await ipc.rpcMade;

		onFrame({ kind: "head", status: 200, headers: [] });
		onFrame({ kind: "body", chunk: b64([1, 2]) });
		onFrame({ kind: "body", chunk: b64([3, 4, 5]) });
		onFrame({ kind: "body", chunk: b64([250]) });
		onFrame({ kind: "end" });

		const response = await fetched;
		// Concatenated in arrival order across the three body frames.
		expect(await readAllBytes(response)).toEqual([1, 2, 3, 4, 5, 250]);
	});

	test("mid-stream cancel of the response body fires ipc.cancel with the same requestId", async () => {
		const daemonFetch = createDaemonFetch(ipc);
		const fetched = daemonFetch(
			"https://daemon.invalid/compass.v1.CompassService/SubscribeEvents",
		);
		const { args, onFrame } = await ipc.rpcMade;

		onFrame({ kind: "head", status: 200, headers: [] });
		onFrame({ kind: "body", chunk: b64([1, 2, 3]) });

		const response = await fetched;
		const reader = present(response.body, "response body").getReader();
		// Consume the first chunk, then drop the subscription mid-stream.
		const first = await reader.read();
		expect(first.value && [...first.value]).toEqual([1, 2, 3]);
		await reader.cancel();

		// The upstream proxy is torn down with the same id the rpc was issued under.
		expect(ipc.cancelCalls).toEqual([args.requestId]);
	});

	test("aborting the request's signal mid-stream fires ipc.cancel with the same requestId", async () => {
		const daemonFetch = createDaemonFetch(ipc);
		const controller = new AbortController();
		const fetched = daemonFetch(
			"https://daemon.invalid/compass.v1.CompassService/SubscribeEvents",
			{ signal: controller.signal },
		);
		const { args, onFrame } = await ipc.rpcMade;

		onFrame({ kind: "head", status: 200, headers: [] });
		const response = await fetched;
		const reader = present(response.body, "response body").getReader();

		controller.abort(new DOMException("gone", "AbortError"));
		expect(ipc.cancelCalls).toEqual([args.requestId]);
		// The in-flight read rejects with the abort reason.
		await expect(reader.read()).rejects.toThrow("gone");
	});

	test("an error frame before the head rejects the fetch promise", async () => {
		const daemonFetch = createDaemonFetch(ipc);
		const fetched = daemonFetch(
			"https://daemon.invalid/compass.v1.CompassService/GetDaemonInfo",
		);
		const { onFrame } = await ipc.rpcMade;
		onFrame({ kind: "error", message: "boom" });
		await expect(fetched).rejects.toThrow("boom");
	});

	test("an already-aborted signal rejects the fetch up front without issuing the rpc", async () => {
		const daemonFetch = createDaemonFetch(ipc);
		const controller = new AbortController();
		controller.abort(new DOMException("gone before start", "AbortError"));
		const fetched = daemonFetch(
			"https://daemon.invalid/compass.v1.CompassService/GetDaemonInfo",
			{ signal: controller.signal },
		);
		// The head rejects with the abort reason, and the doomed RPC is never
		// fired: starting a call we would immediately cancel — or omitting the
		// early return so `head` never settles — is the regression this pins.
		await expect(fetched).rejects.toThrow("gone before start");
		expect(ipc.rpcCalls).toEqual([]);
	});

	test("an error frame after the head surfaces through the body stream, leaving the resolved Response intact", async () => {
		const daemonFetch = createDaemonFetch(ipc);
		const fetched = daemonFetch(
			"https://daemon.invalid/compass.v1.CompassService/SubscribeEvents",
		);
		const { onFrame } = await ipc.rpcMade;

		onFrame({ kind: "head", status: 200, headers: [] });
		onFrame({ kind: "body", chunk: b64([1, 2]) });
		// The fetch promise already resolved on the head frame...
		const response = await fetched;
		expect(response.status).toBe(200);
		// ...so a later error frame is bimodal-routed to the stream (controller
		// .error), not the already-settled fetch promise: inverting the headSeen
		// check would surface the failure on the wrong channel.
		onFrame({ kind: "error", message: "mid-stream daemon failure" });
		await expect(readAllBytes(response)).rejects.toThrow(
			"mid-stream daemon failure",
		);
	});

	test("a concurrent stream-cancel and signal-abort collapse to exactly one ipc.cancel", async () => {
		const daemonFetch = createDaemonFetch(ipc);
		const controller = new AbortController();
		const fetched = daemonFetch(
			"https://daemon.invalid/compass.v1.CompassService/SubscribeEvents",
			{ signal: controller.signal },
		);
		const { args, onFrame } = await ipc.rpcMade;

		onFrame({ kind: "head", status: 200, headers: [] });
		const response = await fetched;
		const reader = present(response.body, "response body").getReader();

		// Both teardown paths fire: dropping the reader tears down the stream,
		// then aborting the signal would tear it down again.
		await reader.cancel();
		controller.abort(new DOMException("gone", "AbortError"));

		// The fire-once `canceled` guard means the upstream proxy is torn down
		// exactly once — removing it would fire cancel twice against the daemon.
		expect(ipc.cancelCalls).toEqual([args.requestId]);
	});
});
