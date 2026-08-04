// Test-only support for the query layer (query record §Test story).
//
// `createAppStore` now requires a `QueryClient` (the store's query-backed reads
// key against it, passed explicitly since the store's owner has no
// QueryClientProvider ancestor — §A3). Tests build a FRESH client per store so
// no cache state leaks across tests: `retry: false` makes a failing queryFn
// settle to error immediately (no backoff timers to await), and
// `gcTime: Infinity` keeps cache entries for the test's lifetime rather than
// racing a garbage-collection timer. Dev/test-only — nothing shipped imports it.

import { QueryClient } from "@tanstack/solid-query";

/** A fresh, isolated `QueryClient` for one test/store — no retries, no GC. */
export function testQueryClient(): QueryClient {
	return new QueryClient({
		defaultOptions: {
			queries: { retry: false, gcTime: Number.POSITIVE_INFINITY },
		},
	});
}
