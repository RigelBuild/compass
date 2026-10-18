// A live Unix-socket h2c server pins the transport's dial, RPC, and typed round-trip.

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
	type PutSessionBlobRequest,
	PutSessionBlobRequestSchema,
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

async function serveSessionBlob(
	handler: (request: PutSessionBlobRequest) => void | Promise<void>,
): Promise<string> {
	const socketPath = path.join(
		os.tmpdir(),
		`t4b-${process.pid}-${Date.now()}-${Math.random().toString(36).slice(2, 8)}.sock`,
	);
	const adapter = connectNodeAdapter({
		routes(router) {
			router.rpc(AgentGateway.method.putSessionBlob, async (req) => {
				await handler(req);
				return {};
			});
		},
	});
	const server = http2.createServer(adapter);
	activeServer = server;
	activeSocketPath = socketPath;
	await new Promise<void>((resolve) => server.listen(socketPath, resolve));
	return socketPath;
}

test("putSessionBlob() dials the Unix socket and forwards the bytes", async () => {
	const received: PutSessionBlobRequest[] = [];
	const socketPath = await serveSessionBlob((req) => {
		received.push(req);
	});
	const transport = createUnixSocketTransport(socketPath);
	const request = create(PutSessionBlobRequestSchema, {
		sha256: "0123456789abcdef".repeat(4),
		data: Uint8Array.of(1, 2, 3, 4),
	});

	await transport.putSessionBlob(request);

	expect(received).toHaveLength(1);
	expect(received[0]?.sha256).toBe(request.sha256);
	expect(received[0]?.data).toEqual(request.data);
	transport.close();
});

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

// The transport owns one runtime for sink, spine, and source; only close() disposes it after drain.
// Drain must leave borrowed lanes usable, and a server receipt proves the sink shared that runtime.
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

	// Spy before close so the test proves close itself disposes the runtime.
	const disposeSpy = spyOn(runtime, "dispose");
	transport.close();
	expect(disposeSpy).toHaveBeenCalledTimes(1);
	// Await the disposal promise captured from close(); its sync API returns no promise.
	await disposeSpy.mock.results[0]?.value;

	// Disposed by close(): the runtime rejects further work, so no fiber it backed
	// survives the transport. runSync on a disposed ManagedRuntime throws.
	expect(() => runtime.runSync(Effect.succeed("dead"))).toThrow(
		"ManagedRuntime disposed",
	);
});
