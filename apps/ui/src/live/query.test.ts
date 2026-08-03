import { describe, expect, test } from "bun:test";
import {
	CompassService,
	create,
	createRouterTransport,
	GetServerInfoResponseSchema,
	type Transport,
} from "@compass/client";
import { QueryClient } from "@tanstack/solid-query";
import { createRoot } from "solid-js";
import { createConnectQuery } from "./query";

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
