// The T4 transport contract (SEA-1351): createUnixSocketTransport must dial a
// Runner-style Unix socket, speak h2c to the AgentGateway.Comms RPC, and return
// the typed CommsCallResult. These tests stand up a real connect-node h2c server
// bound to a Unix socket and drive an actual comms() call through it — proving
// the socket dial, the h2c handshake, and the typed round-trip end to end. A
// mock would restate the transport; only a live socket server can catch a broken
// nodeOptions.path, a wrong baseUrl, or a protocol mismatch.

import { afterEach, expect, test } from "bun:test";
import * as fs from "node:fs";
import * as http2 from "node:http2";
import * as os from "node:os";
import * as path from "node:path";
import { create } from "@bufbuild/protobuf";
import { connectNodeAdapter } from "@connectrpc/connect-node";

import {
	AgentGateway,
	CommsCallRequestSchema,
	type CommsCallResult,
	CommsCallResultSchema,
} from "../gen/compass/v1/agent_gateway_pb";
import {
	MessageBlockSchema,
	PostMessageRequestSchema,
	PostMessageResponseSchema,
} from "../gen/compass/v1/comms_pb";
import { createUnixSocketTransport } from "./index";

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
