// The desktop-shell transport: a `fetch` implementation that routes gRPC-Web requests to
// the Compass daemon through the shell (a WebView `fetch` can't dial the daemon's Unix
// socket). Turns a gRPC-Web `fetch(Request)` into the shell's `compass_rpc` call, streaming
// ordered response frames. Wails-specific calls sit behind a `ShellIpc` seam; dev keeps network `fetch`.

// Mirrors the Rust `ResponseFrame` (bridge.rs): a tagged head/body/end/error
// stream. Body chunks are base64 so they ride the JSON channel as strings.
// Optional wire fields are omitted by Go when empty.
export type ResponseFrame =
	| { kind: "head"; status: number; headers?: [string, string][] }
	| { kind: "body"; chunk?: string }
	| { kind: "end" }
	| { kind: "error"; message?: string };

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

// Statuses the Fetch spec forbids a body on.
const NULL_BODY_STATUSES = new Set([101, 103, 204, 205, 304]);

/** Build the fetch Response for a head frame. Response rejects a body on
 *  null-body statuses; later frames then drain into the unread stream. */
function headResponse(
	stream: ReadableStream<Uint8Array>,
	status: number,
	headers: [string, string][] | undefined,
): Response {
	const body = NULL_BODY_STATUSES.has(status) ? null : stream;
	return new Response(body, { status, headers: new Headers(headers) });
}

/** Decode a standard-base64 body chunk to bytes for the response stream. */
function decodeChunk(b64: string): Uint8Array {
	const bin = atob(b64);
	const out = new Uint8Array(bin.length);
	for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
	return out;
}

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
		const request =
			input instanceof Request
				? new Request(input, init)
				: new Request(input.toString(), init);
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

		// A caller-minted id correlates this call with the Rust proxy task so a cancel can
		// abort it. Fire-once: cancelling the ReadableStream (a dropped SubscribeEvents
		// subscription) or the AbortSignal both route here; without it the daemon-side
		// stream would run until the daemon ended it.
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
					resolveHead(headResponse(stream, frame.status, frame.headers));
					break;
				}
				case "body":
					controller?.enqueue(decodeChunk(frame.chunk ?? ""));
					break;
				case "end":
					controller?.close();
					break;
				case "error": {
					const err = new Error(frame.message ?? "Shell RPC failed");
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

// The Wails v3 binding of the seam — the only place that touches `@wailsio/runtime`. `rpc`
// subscribes to the per-request runtime event BEFORE invoking `CompassRPC`, delivers each
// `ResponseFrame` to `onFrame`, and unsubscribes on the terminal frame; `cancel` invokes
// `CompassRPCCancel`. The Go shell emits one runtime event per ordered frame.
import { Application, Call, Events } from "@wailsio/runtime";
import type { ConnectionProvider, ResolvedConnection } from "./live/provider";

// The fully-qualified names of the bound Go methods, as the Wails generator computes them
// for a `main`-package service: `main.<Struct>.<Method>`. The service is `bridgeService`
// in `go/cmd/compass-app` (package main), so its methods are under `main.bridgeService`.
const RPC_METHOD = "main.bridgeService.CompassRPC";
const RPC_CANCEL_METHOD = "main.bridgeService.CompassRPCCancel";
const CONNECT_METHOD = "main.bridgeService.Connect";
const PICK_CA_METHOD = "main.dialogService.PickCACert";
const CHOOSE_EMBEDDED_METHOD = "main.setupService.ChooseEmbedded";
const SHELL_STATE_METHOD = "main.bridgeService.ShellState";

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
				const frame: unknown = event.data;
				if (!isResponseFrame(frame)) {
					onFrame({
						kind: "error",
						message: "Invalid response frame from shell",
					});
					unsubscribe();
					return;
				}
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
			// unknown/already-finished id is a no-op on the Rust side).
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
		| "invalid-url"
		| "invalid-ca"
		| "other";
	message: string;
	accountId: string;
	serverVersion: string;
	apiVersion: string;
	serverUrl?: string;
};

export type ServerChoice = { url: string; caRef: string };
export type PickedCA = { ref: string; name: string };
export type SetupResult = { ok: boolean; message: string };

export type ShellMode = "embedded" | "client" | "setup" | "reopen";
export type ShellState = { mode: ShellMode; serverUrl: string };

function isConnectResult(value: unknown): value is ConnectResult {
	if (value === null || typeof value !== "object") return false;
	if (
		!("ok" in value) ||
		!("kind" in value) ||
		!("message" in value) ||
		!("accountId" in value) ||
		!("serverVersion" in value) ||
		!("apiVersion" in value)
	)
		return false;
	const kinds: ConnectResult["kind"][] = [
		"",
		"bad-url",
		"bad-cert",
		"bad-token",
		"version-mismatch",
		"invalid-url",
		"invalid-ca",
		"other",
	];
	return (
		typeof value.ok === "boolean" &&
		typeof value.kind === "string" &&
		kinds.some((kind) => kind === value.kind) &&
		typeof value.message === "string" &&
		typeof value.accountId === "string" &&
		typeof value.serverVersion === "string" &&
		typeof value.apiVersion === "string" &&
		(!("serverUrl" in value) || typeof value.serverUrl === "string")
	);
}

function isPickedCA(value: unknown): value is PickedCA {
	return (
		value !== null &&
		typeof value === "object" &&
		"ref" in value &&
		typeof value.ref === "string" &&
		"name" in value &&
		typeof value.name === "string"
	);
}

function isSetupResult(value: unknown): value is SetupResult {
	return (
		value !== null &&
		typeof value === "object" &&
		"ok" in value &&
		typeof value.ok === "boolean" &&
		"message" in value &&
		typeof value.message === "string"
	);
}

function isShellState(value: unknown): value is ShellState {
	if (
		value === null ||
		typeof value !== "object" ||
		!("mode" in value) ||
		!("serverUrl" in value)
	)
		return false;
	return (
		(value.mode === "embedded" ||
			value.mode === "client" ||
			value.mode === "setup" ||
			value.mode === "reopen") &&
		typeof value.serverUrl === "string"
	);
}

function isHeaderPair(value: unknown): value is [string, string] {
	return (
		Array.isArray(value) &&
		value.length === 2 &&
		typeof value[0] === "string" &&
		typeof value[1] === "string"
	);
}

function isResponseFrame(value: unknown): value is ResponseFrame {
	if (value === null || typeof value !== "object" || !("kind" in value))
		return false;
	switch (value.kind) {
		case "head": {
			if (
				!("status" in value) ||
				typeof value.status !== "number" ||
				!Number.isInteger(value.status) ||
				value.status < 200 ||
				value.status > 599
			)
				return false;
			return (
				!("headers" in value) ||
				value.headers === undefined ||
				(Array.isArray(value.headers) &&
					value.headers.every((header) => isHeaderPair(header)))
			);
		}
		case "body":
			return (
				!("chunk" in value) ||
				value.chunk === undefined ||
				typeof value.chunk === "string"
			);
		case "end":
			return true;
		case "error":
			return (
				!("message" in value) ||
				value.message === undefined ||
				typeof value.message === "string"
			);
		default:
			return false;
	}
}

export async function shellConnect(
	token: string,
	server?: ServerChoice,
): Promise<ConnectResult> {
	const request = server === undefined ? { token } : { token, server };
	const result: unknown = await Call.ByName(CONNECT_METHOD, request);
	if (!isConnectResult(result))
		throw new TypeError("Invalid Connect result from shell");
	return result;
}

export async function pickCACert(): Promise<PickedCA> {
	const picked: unknown = await Call.ByName(PICK_CA_METHOD);
	if (!isPickedCA(picked))
		throw new TypeError("Invalid PickCACert result from shell");
	return picked;
}

export async function chooseEmbedded(): Promise<SetupResult> {
	const result: unknown = await Call.ByName(CHOOSE_EMBEDDED_METHOD);
	if (!isSetupResult(result))
		throw new TypeError("Invalid ChooseEmbedded result from shell");
	return result;
}

export async function shellState(): Promise<ShellState> {
	const result: unknown = await Call.ByName(SHELL_STATE_METHOD);
	if (!isShellState(result))
		throw new TypeError("Invalid ShellState result from shell");
	return result;
}

export function onSetupDecided(fn: () => void): () => void {
	return Events.On("setup:decided", fn);
}

export async function quitApp(): Promise<void> {
	await Application.Quit();
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
			// The native transport tunnels requests over IPC, so preconnect has no
			// socket to warm and is intentionally a no-op.
			const fetchImpl = Object.assign(createDaemonFetch(wailsShellIpc()), {
				preconnect: (_url: string | URL): void => {},
			});
			return { baseUrl, token: undefined, fetchImpl };
		},
	};
}
