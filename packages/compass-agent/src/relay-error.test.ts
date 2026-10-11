import { describe, expect, test } from "bun:test";
// biome-ignore lint/style/noRestrictedImports: construct Runner relay status errors for this renderer test
import { Code, ConnectError } from "@connectrpc/connect";
import { BoardBroker, type BoardTransport, createBoardTools } from "./board";
import { CommsBroker, type CommsTransport, createCommsTools } from "./comms";
import { createForgeTools, ForgeBroker, type ForgeTransport } from "./forge";
import {
	createLifecycleTools,
	LifecycleBroker,
	type LifecycleTransport,
} from "./lifecycle";
import { relayCall } from "./relay-error";

describe("broker tool families", () => {
	test.each([
		[
			"comms",
			() => {
				const transport: CommsTransport = {
					async comms() {
						throw new ConnectError("binding details", Code.FailedPrecondition);
					},
				};
				return createCommsTools(new CommsBroker(transport))[0];
			},
			{},
		],
		[
			"lifecycle",
			() => {
				const transport: LifecycleTransport = {
					async lifecycle() {
						throw new ConnectError("binding details", Code.FailedPrecondition);
					},
				};
				return createLifecycleTools(new LifecycleBroker(transport))[0];
			},
			{ handle: "worker", role: "manager", persona: "test" },
		],
		[
			"forge",
			() => {
				const transport: ForgeTransport = {
					async forge() {
						throw new ConnectError("binding details", Code.FailedPrecondition);
					},
				};
				return createForgeTools(new ForgeBroker(transport))[0];
			},
			{ repo: "o/r", issue_number: 1 },
		],
		[
			"board",
			() => {
				const transport: BoardTransport = {
					async board() {
						throw new ConnectError("binding details", Code.FailedPrecondition);
					},
				};
				return createBoardTools(new BoardBroker(transport))[0];
			},
			{ issue_id: "iss-1", state: "done" },
		],
	] as const)(
		"%s tools use shared FailedPrecondition rendering",
		async (name, makeTool, params) => {
			const tool = makeTool();
			if (tool === undefined) throw new Error(`missing ${name} tool`);
			await expect(tool.execute.call(tool, "tc-1", params)).rejects.toThrow(
				"Your session is not bound to an agent account (the Runner may be reconnecting); the target was not checked.",
			);
		},
	);
});
describe("relayCall", () => {
	test("renders an unbound session as a generic failed precondition", async () => {
		await expect(
			relayCall(async () => {
				throw new ConnectError(
					"binding details must not leak",
					Code.FailedPrecondition,
				);
			}),
		).rejects.toThrow(
			"Your session is not bound to an agent account (the Runner may be reconnecting); the target was not checked.",
		);
	});

	test("renders unavailable relays as retryable", async () => {
		await expect(
			relayCall(async () => {
				throw new ConnectError("socket detail", Code.Unavailable);
			}),
		).rejects.toThrow(
			"The Runner relay is temporarily unavailable; retry the tool call.",
		);
	});

	test("preserves other transport errors unchanged", async () => {
		const failure = new ConnectError("missing tool target", Code.NotFound);
		await expect(
			relayCall(async () => {
				throw failure;
			}),
		).rejects.toBe(failure);
	});
});
