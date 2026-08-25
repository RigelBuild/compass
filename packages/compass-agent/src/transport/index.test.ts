// The T4 transport contract (SEA-1351): createUnixSocketTransport must dial a
// Runner-style Unix socket, speak h2c to the AgentGateway.Comms RPC, and return
// the typed CommsCallResult. These tests stand up a real connect-node h2c server
// bound to a Unix socket and drive an actual comms() call through it — proving
// the socket dial, the h2c handshake, and the typed round-trip end to end. A
// mock would restate the transport; only a live socket server can catch a broken
// nodeOptions.path, a wrong baseUrl, or a protocol mismatch.

import { afterEach, expect, spyOn, test } from "bun:test";
import * as fs from "node:fs";
import * as http2 from "node:http2";
import * as os from "node:os";
import * as path from "node:path";
import { create } from "@bufbuild/protobuf";
import { connectNodeAdapter } from "@connectrpc/connect-node";
import { Effect } from "effect";

import { AgentSessionState, SessionFrameSchema } from "../compassv1";
import type { OutboundFrame } from "../frame";
import {
	AgentGateway,
	CommsCallRequestSchema,
	type CommsCallResult,
	CommsCallResultSchema,
	ForgeCallRequestSchema,
	type ForgeCallResult,
	ForgeCallResultSchema,
	GetIssueRequestSchema,
	PublishFrameResponseSchema,
} from "../gen/compass/v1/agent_gateway_pb";
import {
	MessageBlockSchema,
	PostMessageRequestSchema,
	PostMessageResponseSchema,
} from "../gen/compass/v1/comms_pb";
import { IssueSchema } from "../gen/compass/v1/compass_pb";
import { createSocketFrameSink } from "./frame-sink";
import { createUnixSocketTransport } from "./index";
import { getTransportRuntime } from "./runtime-channel";

// One server + one socket per test, torn down in afterEach so a failing case
// never leaks the socket file or a listening server into the next test.
let activeServer: http2.Http2Server | undefined;
let activeSocketPath: string | undefined;

afterEach(async () => {
	if (activeServer !== undefined) {
		const server = activeServer;
		await new Promise<void>((resolve) => server.close(() => resolve()));
		activeServer = undefined;
	}
	// server.close() unlinks the bound socket, but a test that fails before
	// listen (or a partial bind) can leave the file behind — remove defensively.
	if (activeSocketPath !== undefined && fs.existsSync(activeSocketPath)) {
		fs.unlinkSync(activeSocketPath);
	}
	activeSocketPath = undefined;
});

// Stand up an h2c server on a SHORT Unix socket path (Linux sun_path limit is
// ~107 bytes, so os.tmpdir() + a short name, never a deeply nested dir) serving
// AgentGateway.Comms with the supplied handler. Resolves once listen fires — the
// only synchronization point; no sleep, no retry.
async function serveComms(
	handler: (callId: string) => CommsCallResult,
): Promise<string> {
	const socketPath = path.join(
		os.tmpdir(),
		`t4-${process.pid}-${Date.now()}-${Math.random().toString(36).slice(2, 8)}.sock`,
	);
	const adapter = connectNodeAdapter({
		routes(router) {
			router.rpc(AgentGateway.method.comms, async (req) => handler(req.callId));
		},
	});
	const server = http2.createServer(adapter);
	activeServer = server;
	activeSocketPath = socketPath;
	await new Promise<void>((resolve) => server.listen(socketPath, resolve));
	return socketPath;
}

test("comms() dials the Unix socket over h2c and round-trips the post result", async () => {
	// Server echoes the caller's callId and answers with the post-response
	// variant carrying a distinct message id, so the assertions below can only
	// pass if the request reached the socket and the typed result came back.
	const socketPath = await serveComms((callId) =>
		create(CommsCallResultSchema, {
			callId,
			result: {
				case: "post",
				value: create(PostMessageResponseSchema, {
					message: { id: "srv-msg-1", blocks: [] },
				}),
			},
		}),
	);

	const transport = createUnixSocketTransport(socketPath);
	const result = await transport.comms(
		create(CommsCallRequestSchema, {
			callId: "tc-1",
			call: {
				case: "post",
				value: create(PostMessageRequestSchema, {
					container: { case: "channelId", value: "chan-1" },
					blocks: [
						create(MessageBlockSchema, {
							block: { case: "text", value: "hi" },
						}),
					],
				}),
			},
		}),
	);

	// callId round-trips: the correlation id the agent minted comes back intact.
	expect(result.callId).toBe("tc-1");
	// The post variant is present and carries the server's typed response.
	expect(result.result.case).toBe("post");
	if (result.result.case !== "post") throw new Error("expected post variant");
	expect(result.result.value.message?.id).toBe("srv-msg-1");
});

test("comms() round-trips the in-band error variant", async () => {
	const socketPath = await serveComms((callId) =>
		create(CommsCallResultSchema, {
			callId,
			result: {
				case: "error",
				value: { code: "not_found", message: "no such channel" },
			},
		}),
	);

	const transport = createUnixSocketTransport(socketPath);
	const result = await transport.comms(
		create(CommsCallRequestSchema, {
			callId: "tc-2",
			call: {
				case: "post",
				value: create(PostMessageRequestSchema, {
					container: { case: "channelId", value: "chan-2" },
				}),
			},
		}),
	);

	expect(result.callId).toBe("tc-2");
	// The error is an in-band result variant (a tool error the agent renders),
	// NOT a thrown ConnectError — assert it arrives as the typed error case.
	expect(result.result.case).toBe("error");
	if (result.result.case !== "error") throw new Error("expected error variant");
	expect(result.result.value.code).toBe("not_found");
	expect(result.result.value.message).toBe("no such channel");
});

// Stand up an h2c server serving AgentGateway.Forge with the supplied handler.
// Same short-socket-path + listen-only synchronization as serveComms; only the
// mounted RPC differs, proving the transport's `forge` delegation reaches the
// generated client's Forge method (not comms/lifecycle).
async function serveForge(
	handler: (callId: string) => ForgeCallResult,
): Promise<string> {
	const socketPath = path.join(
		os.tmpdir(),
		`t4f-${process.pid}-${Date.now()}-${Math.random().toString(36).slice(2, 8)}.sock`,
	);
	const adapter = connectNodeAdapter({
		routes(router) {
			router.rpc(AgentGateway.method.forge, async (req) => handler(req.callId));
		},
	});
	const server = http2.createServer(adapter);
	activeServer = server;
	activeSocketPath = socketPath;
	await new Promise<void>((resolve) => server.listen(socketPath, resolve));
	return socketPath;
}

test("forge() dials the Unix socket over h2c and round-trips the issue result", async () => {
	const socketPath = await serveForge((callId) =>
		create(ForgeCallResultSchema, {
			callId,
			result: {
				case: "issue",
				value: create(IssueSchema, { number: 42, repo: "octo/repo" }),
			},
		}),
	);

	const transport = createUnixSocketTransport(socketPath);
	const result = await transport.forge(
		create(ForgeCallRequestSchema, {
			callId: "tc-f1",
			call: {
				case: "getIssue",
				value: create(GetIssueRequestSchema, {
					repo: "octo/repo",
					issueNumber: 42n,
				}),
			},
		}),
	);

	expect(result.callId).toBe("tc-f1");
	expect(result.result.case).toBe("issue");
	if (result.result.case !== "issue") throw new Error("expected issue variant");
	expect(result.result.value.number).toBe(42);
	expect(result.result.value.repo).toBe("octo/repo");
});

test("forge() round-trips the in-band error variant", async () => {
	const socketPath = await serveForge((callId) =>
		create(ForgeCallResultSchema, {
			callId,
			result: {
				case: "error",
				value: { code: "not_found", message: "no such repo", retryAfterMs: 0 },
			},
		}),
	);

	const transport = createUnixSocketTransport(socketPath);
	const result = await transport.forge(
		create(ForgeCallRequestSchema, {
			callId: "tc-f2",
			call: {
				case: "getIssue",
				value: create(GetIssueRequestSchema, {
					repo: "octo/repo",
					issueNumber: 1n,
				}),
			},
		}),
	);

	expect(result.callId).toBe("tc-f2");
	// In-band error is a typed result variant (a tool error the agent renders),
	// NOT a thrown ConnectError.
	expect(result.result.case).toBe("error");
	if (result.result.case !== "error") throw new Error("expected error variant");
	expect(result.result.value.code).toBe("not_found");
	expect(result.result.value.message).toBe("no such repo");
});

// Stand up an h2c server serving AgentGateway.Publish (the client-stream the
// FrameSink's trace lane rides), draining every frame so the spine's cycled
// batch resolves cleanly. Same short-socket-path discipline as serveComms.
async function servePublish(received: string[]): Promise<string> {
	const socketPath = path.join(
		os.tmpdir(),
		`t5-${process.pid}-${Date.now()}-${Math.random().toString(36).slice(2, 8)}.sock`,
	);
	const adapter = connectNodeAdapter({
		routes(router) {
			router.rpc(AgentGateway.method.publish, async (stream) => {
				for await (const _ of stream) received.push("publish");
				return create(PublishFrameResponseSchema, {});
			});
		},
	});
	const server = http2.createServer(adapter);
	activeServer = server;
	activeSocketPath = socketPath;
	await new Promise<void>((resolve) => server.listen(socketPath, resolve));
	return socketPath;
}

function traceFrame(): OutboundFrame {
	return {
		kind: "session",
		value: create(SessionFrameSchema, { state: AgentSessionState.UNSPECIFIED }),
	};
}

// The leak class close() exists for (index.ts:50-62, design record §T5): the
// transport owns ONE ManagedRuntime backing the sink/spine/source lanes, and
// close() — called after the sink's drain barrier — is what disposes it, so no
// live fiber outlives the transport. This drives a real socket end to end: a
// sink over the shared transport emits + drains a frame, and then close()
// disposes the runtime. Both halves of the ownership contract are pinned:
//   - after sink.drain() the runtime is STILL live — a BORROWING factory must
//     never dispose the shared runtime (a premature dispose would break the
//     still-open sibling spine/source), so drain leaves it usable; and
//   - after transport.close() the runtime is disposed — the transport owns that
//     disposal, and once disposed it runs no further work, so no fiber keeps the
//     loop alive.
// A live server-side receipt of the frame proves the shared runtime actually
// backed the send (non-vacuity: a runtime disposed too early at drain would have
// thrown inside the sink rather than flushing).
test("close() after drain() disposes the single transport-owned runtime, leaving no live fibers", async () => {
	const received: string[] = [];
	const socketPath = await servePublish(received);

	const transport = createUnixSocketTransport(socketPath);
	// The sink borrows the transport-owned runtime through the module-private
	// channel (production wiring path) — it does not make its own.
	const sink = createSocketFrameSink(transport);
	const runtime = getTransportRuntime(transport);
	if (runtime === undefined) {
		throw new Error("transport did not publish its runtime on the channel");
	}

	sink.emit(traceFrame());
	await sink.drain?.();

	// The frame reached the Runner: the shared runtime backed a real send, so a
	// borrowed runtime disposed too early at drain would have reddened here.
	expect(received).toEqual(["publish"]);
	// Borrowed, so drain() must NOT have disposed it — the runtime is still usable.
	expect(runtime.runSync(Effect.succeed("live"))).toBe("live");

	// close() must be the thing that disposes the runtime — the leak class this
	// test exists for. Spy on dispose BEFORE close() so the assertion pins
	// close()'s own call, not a dispose the test issued: awaiting the runtime
	// here directly (as a "second, idempotent dispose") would tear it down
	// regardless of whether close() ever called it, so a close() that forgot to
	// dispose would still pass. With the spy, a close() that only aborts the
	// session leaves toHaveBeenCalled false and reddens.
	const disposeSpy = spyOn(runtime, "dispose");
	transport.close();
	expect(disposeSpy).toHaveBeenCalledTimes(1);
	// dispose is fire-and-forget from close()'s sync signature; await the exact
	// promise close() started (captured by the spy) — no second dispose, no
	// wall-clock poll — to observe its completion deterministically.
	await disposeSpy.mock.results[0]?.value;

	// Disposed by close(): the runtime rejects further work, so no fiber it backed
	// survives the transport. runSync on a disposed ManagedRuntime throws.
	expect(() => runtime.runSync(Effect.succeed("dead"))).toThrow(
		"ManagedRuntime disposed",
	);
});
