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
// The two framework-specific calls (the Wails runtime's `Call.ByName` bound
// method + the `Events.On` response-frame subscription) sit behind a local
// `ShellIpc` seam so the frame contract and all stream/cancel/abort logic are
// framework-agnostic: the browser dev path keeps the default network `fetch`,
// and any shell can supply its own `ShellIpc` binding.

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
 * for the same `requestId`. Framework calls (`Call.ByName`, `Events.On`) live
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

// The Wails v3 binding of the seam — the only place that touches
// `@wailsio/runtime`. `rpc` subscribes to the per-request runtime event
// `"compass_rpc:"+requestId` BEFORE invoking the bound `CompassRPC` method (by
// name), delivering each `ResponseFrame` to `onFrame`, and unsubscribes on the
// terminal frame (`end`/`error`); `cancel` invokes the bound `CompassRPCCancel`
// method. The Go shell emits one runtime event per ordered frame carrying the
// JS `ResponseFrame` shape (go/cmd/compass-app/bridge_service.go:9-12,199-201).
import { Call, Events } from "@wailsio/runtime";
import type { ConnectionProvider, ResolvedConnection } from "./live/provider";

// The fully-qualified names of the bound Go methods, as the Wails binding
// generator computes them for a `main`-package service: `main.<Struct>.<Method>`
// (v3 collectMethod: reflect reports the main package's path as "main", then
// `path + "." + structName + "." + methodName`). The service is `bridgeService`
// in `go/cmd/compass-app` (package main), so its bound methods are namespaced
// under `main.bridgeService`.
const RPC_METHOD = "main.bridgeService.CompassRPC";
const RPC_CANCEL_METHOD = "main.bridgeService.CompassRPCCancel";
const CONNECT_METHOD = "main.bridgeService.Connect";

/** Build the Wails binding of the shell IPC seam. `rpc` wires the response-frame
 *  subscription up before firing the call so no frame can race ahead of the
 *  listener, and tears the subscription down on the terminal frame; `cancel`
 *  best-effort invokes the cancel method and swallows a race with the proxy
 *  finishing. */
export function wailsShellIpc(): ShellIpc {
	return {
		rpc(args, onFrame) {
			const eventName = `compass_rpc:${args.requestId}`;
			// Subscribe BEFORE invoking so the first (head) frame can never be
			// emitted before the listener is installed. `Events.On` returns its
			// own unsubscribe function; the terminal frame calls it so a finished
			// stream leaves no dangling listener.
			let off: (() => void) | undefined;
			const unsubscribe = () => {
				off?.();
				off = undefined;
			};
			off = Events.On(eventName, (event: Events.WailsEvent) => {
				const frame = event.data as ResponseFrame;
				onFrame(frame);
				if (frame.kind === "end" || frame.kind === "error") unsubscribe();
			});
			// The Go `CompassRPC` returns nothing (it launches the streaming proxy
			// and returns immediately); the frames arrive as events. Surface an
			// invoke rejection to the caller so `createDaemonFetch` can fail the
			// head/stream, and drop the subscription so it does not leak.
			return Call.ByName(RPC_METHOD, args).then(
				() => {},
				(err: unknown) => {
					unsubscribe();
					throw err instanceof Error ? err : new Error(String(err));
				},
			);
		},
		cancel(requestId) {
			// Best-effort: swallow a cancel that races the proxy finishing (an
			// unknown/already-finished id is a no-op on the Go side).
			Call.ByName(RPC_CANCEL_METHOD, { requestId }).catch(() => {});
		},
	};
}

/** The sealed result of a shell `Connect` probe. `kind` is `""` on success and
 *  one of the failure kinds otherwise (mirrors the Go `connectResult`,
 *  design.md T5.3). The bearer never crosses this seam — it is stored shell-side
 *  only (DL-109). */
export type ConnectResult = {
	ok: boolean;
	kind:
		| ""
		| "bad-url"
		| "bad-cert"
		| "bad-token"
		| "version-mismatch"
		| "other";
	message: string;
	accountId: string;
	serverVersion: string;
	apiVersion: string;
};

/** Invoke the Go shell's `Connect` bound method by name, passing the pasted
 *  token (or `""` for the boot-internal "use the stored one" probe, T5.5), and
 *  return its classified result. The method lives in the (unmerged) T5.3 stack;
 *  it is called purely by string name through the Wails runtime, never imported,
 *  so this compiles and is testable against a fake runtime without the Go method
 *  existing on main. */
export function shellConnect(token: string): Promise<ConnectResult> {
	return Call.ByName(CONNECT_METHOD, { token }) as Promise<ConnectResult>;
}

/** The native (desktop-shell) connection provider. `resolve()` hands back the
 *  shell base URL plus a `fetchImpl` that tunnels gRPC-Web over the Wails IPC —
 *  and NEVER a bearer: `token` is always `undefined` because in client mode the
 *  bearer lives only shell-side, presented by the shell as it proxies each call
 *  (DL-109). This is the one place a shell dependency (`wailsShellIpc`) meets the
 *  provider seam, kept out of `provider.ts` so that module stays Wails-free. */
export function nativeConnectionProvider(baseUrl: string): ConnectionProvider {
	return {
		async resolve(): Promise<ResolvedConnection> {
			// The transport only ever invokes the call signature of `fetch`; the
			// daemon fetch deliberately omits `fetch.preconnect` (a no-op over the
			// IPC tunnel), so widen it to the `typeof fetch` the seam declares.
			const fetchImpl = createDaemonFetch(wailsShellIpc()) as typeof fetch;
			return { baseUrl, token: undefined, fetchImpl };
		},
	};
}
