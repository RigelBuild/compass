// Create an isolated QueryClient with retries and cache cleanup disabled.

import { QueryClient } from "@tanstack/solid-query";

/** A fresh, isolated `QueryClient` for one test/store — no retries, no GC. */
export function testQueryClient(): QueryClient {
	return new QueryClient({
		defaultOptions: {
			queries: { retry: false, gcTime: Number.POSITIVE_INFINITY },
		},
	});
}
