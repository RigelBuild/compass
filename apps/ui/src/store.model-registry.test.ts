import { describe, expect, test } from "bun:test";
import {
	CompassService,
	create,
	createRouterTransport,
	type GetModelRegistryResponse,
	GetModelRegistryResponseSchema,
	type Transport,
} from "@compass/client";
import { createRoot, flush } from "solid-js";
import { createAppStore, modelRegistryRows } from "./store";
import { testQueryClient } from "./test-support";

const transportFor = (
	getModelRegistry: () => GetModelRegistryResponse,
): Transport =>
	createRouterTransport(({ service }) =>
		service(CompassService, { getModelRegistry }),
	);

// Drain microtasks until the query settles; the router transport resolves across
// several hops (same bound as live/query.test.ts).
async function settle(read: () => { status: string }): Promise<void> {
	for (let i = 0; i < 200 && read().status === "pending"; i++) {
		await Promise.resolve();
		flush();
	}
}

function response(
	version: bigint,
	entries: Record<
		string,
		{ displayName: string; candidates: { provider: string; modelId: string }[] }
	>,
) {
	return create(GetModelRegistryResponseSchema, {
		version,
		registry: { entries },
	});
}

describe("model registry store state", () => {
	test("is offline without a transport", () => {
		const store = createRoot(() =>
			createAppStore({ queryClient: testQueryClient() }),
		);
		expect(store.modelRegistry()).toEqual({ status: "offline" });
	});

	test("sorts stable names and preserves candidate order and version", async () => {
		const transport = transportFor(() =>
			response(7n, {
				zeta: {
					displayName: "Zeta",
					candidates: [
						{ provider: "first", modelId: "one" },
						{ provider: "second", modelId: "two" },
					],
				},
				alpha: {
					displayName: "Alpha",
					candidates: [{ provider: "third", modelId: "three" }],
				},
			}),
		);
		let dispose!: () => void;
		const store = createRoot((d) => {
			dispose = d;
			return createAppStore({ queryClient: testQueryClient(), transport });
		});
		try {
			await settle(store.modelRegistry);
			expect(store.modelRegistry()).toEqual({
				status: "ready",
				version: 7n,
				entries: [
					{
						stableName: "alpha",
						displayName: "Alpha",
						candidates: [{ provider: "third", modelId: "three" }],
					},
					{
						stableName: "zeta",
						displayName: "Zeta",
						candidates: [
							{ provider: "first", modelId: "one" },
							{ provider: "second", modelId: "two" },
						],
					},
				],
			});
		} finally {
			dispose();
		}
	});

	test("represents an unconfigured registry as empty ready state", async () => {
		const transport = transportFor(() => response(0n, {}));
		let dispose!: () => void;
		const store = createRoot((d) => {
			dispose = d;
			return createAppStore({ queryClient: testQueryClient(), transport });
		});
		try {
			await settle(store.modelRegistry);
			expect(store.modelRegistry()).toEqual({
				status: "ready",
				version: 0n,
				entries: [],
			});
		} finally {
			dispose();
		}
	});

	test("maps RPC failures to error state", async () => {
		const transport = transportFor(() => {
			throw new Error("boom");
		});
		let dispose!: () => void;
		const store = createRoot((d) => {
			dispose = d;
			return createAppStore({ queryClient: testQueryClient(), transport });
		});
		try {
			await settle(store.modelRegistry);
			// A non-Connect throw reaches the client as Code.Internal.
			expect(store.modelRegistry()).toEqual({
				status: "error",
				message: "[internal] internal error",
			});
		} finally {
			dispose();
		}
	});

	test("keeps the loaded registry when a later refetch fails", async () => {
		let fail = false;
		const transport = transportFor(() => {
			if (fail) throw new Error("down");
			return response(2n, {
				opus: {
					displayName: "Opus",
					candidates: [{ provider: "anthropic", modelId: "opus" }],
				},
			});
		});
		const queryClient = testQueryClient();
		let dispose!: () => void;
		const store = createRoot((d) => {
			dispose = d;
			return createAppStore({ queryClient, transport });
		});
		try {
			await settle(store.modelRegistry);
			fail = true;
			await queryClient.refetchQueries();
			flush();
			expect(store.modelRegistry()).toEqual({
				status: "ready",
				version: 2n,
				entries: [
					{
						stableName: "opus",
						displayName: "Opus",
						candidates: [{ provider: "anthropic", modelId: "opus" }],
					},
				],
			});
		} finally {
			dispose();
		}
	});

	test("maps entries into sorted rows", () => {
		expect(
			modelRegistryRows(
				response(1n, {
					b: { displayName: "B", candidates: [] },
					a: { displayName: "A", candidates: [] },
				}).registry?.entries ?? {},
			),
		).toEqual([
			{ stableName: "a", displayName: "A", candidates: [] },
			{ stableName: "b", displayName: "B", candidates: [] },
		]);
	});
});
