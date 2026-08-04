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

// The connect-query-core → solid-query glue seam (query record §A2/§T2).
//
// A fake server is a `createRouterTransport` handler serving the compass.v1
// method — the vendor's documented no-HTTP test path — so the round-trip
// exercises the real seam, never a mock of it. Both tests run under a BARE
// `createRoot` with the QueryClient passed EXPLICITLY and NO
// `QueryClientProvider`: this is exactly the store-root usage pattern T3 relies
// on (§A3 — the store's owner has no provider ancestor), and it MUST NOT throw
// `No QueryClient set`. Reading through a context-less provider would.

// A fake CompassService serving GetServerInfo with a fixed body.
function fakeTransport(version: string, apiVersion: string): Transport {
	return createRouterTransport(({ service }) => {
		service(CompassService, {
			getServerInfo: () =>
				create(GetServerInfoResponseSchema, { version, apiVersion }),
		});
	});
}

// A test client with no retries/GC — a failing queryFn settles immediately and
// cache entries live for the test, no timers to await.
function testClient(): QueryClient {
	return new QueryClient({
		defaultOptions: {
			queries: { retry: false, gcTime: Number.POSITIVE_INFINITY },
		},
	});
}

// Drive the query to a settled (non-pending) state by draining the microtask
// queue until it resolves — no wall-clock timer. createRouterTransport's
// in-memory server yields through the event loop across several microtask hops,
// so this polls the query's OWN state (the real signal we're waiting on) with a
// bounded drain rather than guessing a fixed count.
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

	// The load-bearing spike (§A3): a query under a bare createRoot with an
	// explicitly-passed QueryClient and NO QueryClientProvider must not throw
	// `No QueryClient set` at creation and must still resolve — proving the
	// store-root pattern before T3 depends on it. A context-resolved client would
	// throw the moment the query is created outside a provider.
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

// A fake CommsService serving cursor-paged listMessages over a fixed,
// newest-first corpus (comms_pb.ts:1424 — `before_message_id` is the cursor:
// empty = the newest page, otherwise the page strictly older than that id).
// The server returns up to `limit` messages per page, exactly like the daemon.
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
	// The cursor plumbing is the trickiest generics in the seam: initialPageParam
	// = input[pageParamKey] seeds page 1, each fetch merges the pageParam back into
	// the input, and getNextPageParam derives the next cursor from the last page.
	// This round-trips all three against a real paged createRouterTransport fake
	// (never a mock of the seam), under the SAME bare-createRoot / explicit-client
	// / no-provider pattern the store relies on (§A3).
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
					// initialPageParam = input[pageParamKey] = "" → the newest page.
					beforeMessageId: "",
				}),
				{
					transport,
					queryClient,
					pageParamKey: "beforeMessageId",
					// End of history = a short (< limit) page: nothing older to fetch.
					getNextPageParam: (lastPage) =>
						lastPage.messages.length < 2
							? undefined
							: lastPage.messages[lastPage.messages.length - 1].id,
				},
			);
		});
		try {
			await settleQuery(query);
			// Page 1: the newest `limit` messages, in server order.
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
