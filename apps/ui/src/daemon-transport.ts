// The desktop-shell transport: a `fetch` implementation that routes gRPC-Web
// requests to the Compass daemon through the Tauri shell instead of the network.
//
// A WebView `fetch` can't dial the daemon's Unix socket, so the shell exposes a
// `compass_rpc` command that proxies to it. This adapter turns a gRPC-Web
// `fetch(Request)` into that command call: it sends the request bytes over
// `invoke`, receives the response as ordered frames on a Tauri `Channel`, and
// reassembles them into a `Response` whose body is a `ReadableStream` — so
// `createGrpcWebTransport({ fetch })` streams `SubscribeEvents` incrementally,
// with all gRPC-Web framing handled by the generated client.
//
// Used only when running inside the shell; the plain browser dev path keeps the
// default network `fetch`.

import { Channel, invoke } from "@tauri-apps/api/core";

// Mirrors the Rust `ResponseFrame` (bridge.rs): a tagged head/body/end/error
// stream. Body chunks are base64 so they ride the JSON Channel as strings.
type ResponseFrame =
	| { kind: "head"; status: number; headers: [string, string][] }
	| { kind: "body"; chunk: string }
	| { kind: "end" }
	| { kind: "error"; message: string };

/** Decode a standard-base64 body chunk to bytes for the response stream. */
function decodeChunk(b64: string): Uint8Array {
	const bin = atob(b64);
	const out = new Uint8Array(bin.length);
	for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
	return out;
}

/**
 * A `fetch` that proxies gRPC-Web calls to the daemon over the Tauri bridge.
 * Only the inputs the gRPC-Web transport actually sets are honored: method,
 * headers, body, and the URL's path+query (the daemon is same-origin behind the
 * socket, so the origin is dropped).
 */
export const daemonFetch = async (
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

	// The head frame resolves the returned Response; body frames enqueue onto its
	// stream. A promise bridges the Channel's first (head) frame to the awaited
	// return, while later frames drive the ReadableStream controller.
	let controller: ReadableStreamDefaultController<Uint8Array> | undefined;
	let resolveHead!: (r: Response) => void;
	let rejectHead!: (e: unknown) => void;
	const head = new Promise<Response>((res, rej) => {
		resolveHead = res;
		rejectHead = rej;
	});

	// A caller-minted id correlates this call with the Rust proxy task so a
	// cancel can abort it. Fire-once: cancelling the ReadableStream (the gRPC-Web
	// transport dropping a `SubscribeEvents` subscription — an unmounted view, a
	// navigation) or the request's AbortSignal both route here, and without it
	// the daemon-side stream would run until the daemon ended it.
	const requestId = crypto.randomUUID();
	let canceled = false;
	const cancelUpstream = () => {
		if (canceled) return;
		canceled = true;
		// Best-effort: the proxy may have already finished (its id is then gone,
		// and the cancel is a no-op on the Rust side).
		invoke("compass_rpc_cancel", { requestId }).catch(() => {});
	};

	let headSeen = false;
	const stream = new ReadableStream<Uint8Array>({
		start(c) {
			controller = c;
		},
		// The consumer stopped reading (unsubscribe / reader.cancel()): abort the
		// upstream daemon proxy rather than leave it streaming into the void.
		cancel() {
			cancelUpstream();
		},
	});

	// An aborted request (navigation, unmount, timeout) also tears down the call:
	// stop the upstream proxy and fail the stream/head with the abort reason.
	const abortError = () =>
		request.signal?.reason ?? new DOMException("Aborted", "AbortError");
	if (request.signal) {
		// Already aborted before we start: reject immediately (the fetch contract)
		// and don't fire the RPC at all. Merely calling cancelUpstream would leave
		// `head` unsettled — the `canceled` guard then drops every frame — so the
		// returned promise would hang forever.
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

	const channel = new Channel<ResponseFrame>();
	channel.onmessage = (frame) => {
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

	invoke("compass_rpc", {
		requestId,
		method: request.method,
		url: path,
		headers,
		body,
		channel,
	}).catch((e) => {
		const err = e instanceof Error ? e : new Error(String(e));
		if (headSeen) controller?.error(err);
		else rejectHead(err);
	});

	return head;
};
