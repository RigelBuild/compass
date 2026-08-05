// The desktop-shell transport: a `fetch` implementation that routes gRPC-Web
// requests to the Compass daemon through the shell instead of the network.
//
// A WebView `fetch` can't dial the daemon's Unix socket, so the shell exposes a
// `compass_rpc` command that proxies to it. This adapter turns a gRPC-Web
// `fetch(Request)` into that command call: it sends the request bytes over the
// shell, receives the response as ordered frames, and reassembles them into a
// `Response` whose body is a `ReadableStream` — so
// `createGrpcWebTransport({ fetch })` streams `SubscribeEvents` incrementally,
// with all gRPC-Web framing handled by the generated client.
//
// The two framework-specific calls (Tauri's `invoke` and `Channel`) sit behind
// a local `ShellIpc` seam so the frame contract and all stream/cancel/abort
// logic are framework-agnostic: the browser dev path keeps the default network
// `fetch`, and any shell can supply its own `ShellIpc` binding.

// Mirrors the Rust `ResponseFrame` (bridge.rs): a tagged head/body/end/error
// stream. Body chunks are base64 so they ride the JSON channel as strings.
export type ResponseFrame =
	| { kind: "head"; status: number; headers: [string, string][] }
	| { kind: "body"; chunk: string }
	| { kind: "end" }
	| { kind: "error"; message: string };

/**
 * The shell↔UI frame seam (design §A2). A `ShellIpc` proxies a single gRPC-Web
 * call to the daemon: `rpc` issues the `compass_rpc` request and delivers each
 * ordered `ResponseFrame` to `onFrame`; `cancel` issues `compass_rpc_cancel`
 * for the same `requestId`. Framework calls (`invoke`, `Channel`) live only in
 * a binding of this interface, never above it.
 */
export interface ShellIpc {
	rpc(
		args: {
			requestId: string;
			path: string;
			headers: { name: string; value: string }[];
			body: number[];
		},
		onFrame: (frame: ResponseFrame) => void,
	): Promise<void>;
	cancel(requestId: string): void;
}

/** Decode a standard-base64 body chunk to bytes for the response stream. */
function decodeChunk(b64: string): Uint8Array {
	const bin = atob(b64);
	const out = new Uint8Array(bin.length);
	for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
	return out;
}

/** The fetch this module produces: only the inputs the gRPC-Web transport sets. */
type DaemonFetch = (
	input: RequestInfo | URL,
	init?: RequestInit,
) => Promise<Response>;

/**
 * Build a `fetch` that proxies gRPC-Web calls to the daemon over the given
 * `ShellIpc`. Only the inputs the gRPC-Web transport actually sets cross the
 * seam: headers, body, and the URL's path+query (the daemon is same-origin
 * behind the socket, so the origin is dropped; gRPC-Web is always POST, so the
 * method is implicit).
 */
export function createDaemonFetch(ipc: ShellIpc): DaemonFetch {
	return async (
		input: RequestInfo | URL,
		init?: RequestInit,
	): Promise<Response> => {
		const request = new Request(input as RequestInfo, init);
		const url = new URL(request.url);
		const path = url.pathname + url.search;

		const headers: { name: string; value: string }[] = [];
		request.headers.forEach((value, name) => {
			headers.push({ name, value });
		});

		const bodyBuf = await request.arrayBuffer();
		const body = Array.from(new Uint8Array(bodyBuf));

		// The head frame resolves the returned Response; body frames enqueue onto
		// its stream. A promise bridges the first (head) frame to the awaited
		// return, while later frames drive the ReadableStream controller.
		let controller: ReadableStreamDefaultController<Uint8Array> | undefined;
		let resolveHead!: (r: Response) => void;
		let rejectHead!: (e: unknown) => void;
		const head = new Promise<Response>((res, rej) => {
			resolveHead = res;
			rejectHead = rej;
		});

		// A caller-minted id correlates this call with the Rust proxy task so a
		// cancel can abort it. Fire-once: cancelling the ReadableStream (the
		// gRPC-Web transport dropping a `SubscribeEvents` subscription — an
		// unmounted view, a navigation) or the request's AbortSignal both route
		// here, and without it the daemon-side stream would run until the daemon
		// ended it.
		const requestId = crypto.randomUUID();
		let canceled = false;
		const cancelUpstream = () => {
			if (canceled) return;
			canceled = true;
			// Best-effort: the proxy may have already finished (its id is then
			// gone, and the cancel is a no-op on the Rust side).
			ipc.cancel(requestId);
		};

		let headSeen = false;
		const stream = new ReadableStream<Uint8Array>({
			start(c) {
				controller = c;
			},
			// The consumer stopped reading (unsubscribe / reader.cancel()): abort
			// the upstream daemon proxy rather than leave it streaming into the void.
			cancel() {
				cancelUpstream();
			},
		});

		// An aborted request (navigation, unmount, timeout) also tears down the
		// call: stop the upstream proxy and fail the stream/head with the reason.
		const abortError = () =>
			request.signal?.reason ?? new DOMException("Aborted", "AbortError");
		if (request.signal) {
			// Already aborted before we start: reject immediately (the fetch
			// contract) and don't fire the RPC at all. Merely calling
			// cancelUpstream would leave `head` unsettled — the `canceled` guard
			// then drops every frame — so the returned promise would hang forever.
			if (request.signal.aborted) {
				rejectHead(abortError());
				return head;
			}
			request.signal.addEventListener("abort", () => {
				cancelUpstream();
				const err = abortError();
				if (headSeen) controller?.error(err);
				else rejectHead(err);
			});
		}

		const onFrame = (frame: ResponseFrame) => {
			// Once canceled, ignore late frames — the stream is torn down and
			// enqueuing onto a canceled controller throws.
			if (canceled) return;
			switch (frame.kind) {
				case "head": {
					headSeen = true;
					resolveHead(
						new Response(stream, {
							status: frame.status,
							headers: new Headers(frame.headers),
						}),
					);
					break;
				}
				case "body":
					controller?.enqueue(decodeChunk(frame.chunk));
					break;
				case "end":
					controller?.close();
					break;
				case "error": {
					const err = new Error(frame.message);
					// Before the head arrives the failure rejects `fetch`; after, it
					// surfaces as a stream error the transport maps to a call failure.
					if (headSeen) controller?.error(err);
					else rejectHead(err);
					break;
				}
			}
		};

		ipc.rpc({ requestId, path, headers, body }, onFrame).catch((e) => {
			const err = e instanceof Error ? e : new Error(String(e));
			if (headSeen) controller?.error(err);
			else rejectHead(err);
		});

		return head;
	};
}

// The Tauri binding of the seam — the only place that touches
// `@tauri-apps/api/core`. `rpc` opens the response `Channel`, wires each frame
// to `onFrame`, and issues `compass_rpc`; `cancel` issues `compass_rpc_cancel`.
import { Channel, invoke } from "@tauri-apps/api/core";

const tauriShellIpc: ShellIpc = {
	async rpc(args, onFrame) {
		const channel = new Channel<ResponseFrame>();
		channel.onmessage = onFrame;
		await invoke("compass_rpc", { ...args, channel });
	},
	cancel(requestId) {
		// Best-effort: swallow a cancel that races the proxy finishing.
		invoke("compass_rpc_cancel", { requestId }).catch(() => {});
	},
};

/**
 * A `fetch` that proxies gRPC-Web calls to the daemon over the Tauri bridge.
 * Used only when running inside the shell; the plain browser dev path keeps the
 * default network `fetch`.
 */
export const daemonFetch: DaemonFetch = createDaemonFetch(tauriShellIpc);
