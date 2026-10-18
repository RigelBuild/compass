import { describe, expect, test } from "bun:test";
import {
	CommsService,
	CompassService,
	create,
	createRouterTransport,
	GetServerInfoResponseSchema,
	ListMessagesResponseSchema,
	type Transport,
} from "@compass/client";
import { QueryClient } from "@tanstack/solid-query";
import { createRoot } from "solid-js";
import { createConnectInfiniteQuery, createConnectQuery } from "./query";

// Exercise Connect-query glue with an in-memory service and explicit client.
// Bare roots prove that store reads need no provider ancestor.

// A fake CompassService serving GetServerInfo with a fixed body.
function fakeTransport(version: string, apiVersion: string): Transport {
	return createRouterTransport(({ service }) => {
		service(CompassService, {
			getServerInfo: () =>
				create(GetServerInfoResponseSchema, { version, apiVersion }),
		});
	});
}

// Disable retries and cache cleanup timers so each test settles deterministically.
function testClient(): QueryClient {
	return new QueryClient({
		defaultOptions: {
			queries: { retry: false, gcTime: Number.POSITIVE_INFINITY },
		},
	});
}

// Drain microtasks until the query settles, without a wall-clock delay.
async function settleQuery(query: { status: string }): Promise<void> {
	for (let i = 0; i < 200 && query.status === "pending"; i++) {
		await Promise.resolve();
	}
}

describe("createConnectQuery seam (query record T2)", () => {
	test("round-trips a query against a createRouterTransport fake", async () => {
		const transport = fakeTransport("9.9.9", "compass.vX");
		const queryClient = testClient();

		let dispose!: () => void;
		const query = createRoot((d) => {
			dispose = d;
			// Explicit client, NO provider — the store-root pattern (§A3).
			return createConnectQuery(
				CompassService.method.getServerInfo,
				() => ({}),
				{ transport, queryClient },
			);
		});
		try {
			await settleQuery(query);
			expect(query.data?.version).toBe("9.9.9");
			expect(query.data?.apiVersion).toBe("compass.vX");
		} finally {
			dispose();
		}
	});

	// Explicit clients must work outside QueryClientProvider.
	test("runs under a bare createRoot with no QueryClientProvider", async () => {
		const transport = fakeTransport("1.0.0", "compass.v1");
		const queryClient = testClient();

		let dispose!: () => void;
		let threw: unknown;
		const query = createRoot((d) => {
			dispose = d;
			try {
				return createConnectQuery(
					CompassService.method.getServerInfo,
					() => ({}),
					{ transport, queryClient },
				);
			} catch (e) {
				threw = e;
				return undefined;
			}
		});
		try {
			expect(threw).toBeUndefined();
			if (query) await settleQuery(query);
			expect(query?.data?.version).toBe("1.0.0");
		} finally {
			dispose();
		}
	});
});

// Fake cursor-paged messages are newest-first; an empty cursor selects the newest page.
const CORPUS = ["m5", "m4", "m3", "m2", "m1"]; // newest-first

function pagedTransport(limit: number): Transport {
	return createRouterTransport(({ service }) => {
		service(CommsService, {
			listMessages: (req) => {
				const start =
					req.beforeMessageId === ""
						? 0
						: CORPUS.indexOf(req.beforeMessageId) + 1;
				const ids = CORPUS.slice(start, start + limit);
				return create(ListMessagesResponseSchema, {
					messages: ids.map((id) => ({ id })),
				});
			},
		});
	});
}

describe("createConnectInfiniteQuery seam (query record §A6)", () => {
	// Cursor plumbing must seed, advance, and terminate paged requests correctly.
	test("pages a cursor-paged method and terminates at end of history", async () => {
		const transport = pagedTransport(2);
		const queryClient = testClient();

		let dispose!: () => void;
		const query = createRoot((d) => {
			dispose = d;
			return createConnectInfiniteQuery(
				CommsService.method.listMessages,
				() => ({
					container: { case: "channelId" as const, value: "c1" },
					limit: 2,
					// Empty cursor starts from the newest page.
					beforeMessageId: "",
				}),
				{
					transport,
					queryClient,
					pageParamKey: "beforeMessageId",
					// A short page ends history.
					getNextPageParam: (lastPage) =>
						lastPage.messages.length < 2
							? undefined
							: lastPage.messages[lastPage.messages.length - 1].id,
				},
			);
		});
		try {
			await settleQuery(query);
			// First page contains the newest messages in server order.
			expect(query.data?.pages.map((p) => p.messages.map((m) => m.id))).toEqual(
				[["m5", "m4"]],
			);
			expect(query.hasNextPage).toBe(true);

			// The cursor drives fetchNextPage: page 2 is strictly older.
			await query.fetchNextPage();
			expect(query.data?.pages.map((p) => p.messages.map((m) => m.id))).toEqual(
				[
					["m5", "m4"],
					["m3", "m2"],
				],
			);
			expect(query.hasNextPage).toBe(true);

			// Page 3 is a short page (one message) → getNextPageParam returns
			// undefined → end of history, no further page.
			await query.fetchNextPage();
			expect(query.data?.pages.at(-1)?.messages.map((m) => m.id)).toEqual([
				"m1",
			]);
			expect(query.hasNextPage).toBe(false);
		} finally {
			dispose();
		}
	});
});
