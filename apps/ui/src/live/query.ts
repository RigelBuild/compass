// The connect-query-core → solid-query glue seam (query record §A2).
//
// connect-query-core is framework-agnostic: its option factories return a
// `{ queryKey, queryFn, structuralSharing }` (plus `getNextPageParam` /
// `initialPageParam` for infinite queries), keyed by the generated method
// descriptor + transport + input via `createConnectQueryKey`. No first-party
// Solid binding exists (connect-query ships React hooks only), so this small
// typed module maps a core descriptor to a `@tanstack/solid-query` call. It is
// glue, not a fork — deletable the day a first-party `connect-solid-query` ships.
//
// Two load-bearing choices, both from the record:
//   1. Options are a THUNK. Solid Query re-runs the thunk when a signal it reads
//      changes, so a reactive `input` re-keys and refetches (§A2).
//   2. The `QueryClient` is passed EXPLICITLY as the query hook's second
//      argument (an `Accessor<QueryClient>`, `() => opts.queryClient`), never
//      resolved from context. Store-internal queries have no
//      `QueryClientProvider` ancestor — the store singleton is built before
//      `render()` mounts the provider (§A3) — so context resolution would throw
//      `No QueryClient set` at boot. The provider (§A1) serves components; the
//      store uses the explicit client.

import type {
	DescMessage,
	DescMethodUnary,
	MessageInitShape,
	MessageShape,
} from "@bufbuild/protobuf";
import type { Transport } from "@compass/client";
import {
	createInfiniteQueryOptions,
	createQueryOptions,
} from "@connectrpc/connect-query-core";
import type {
	CreateInfiniteQueryResult,
	CreateQueryResult,
	GetNextPageParamFunction,
	InfiniteData,
	QueryClient,
	SkipToken,
} from "@tanstack/solid-query";
import { useInfiniteQuery, useQuery } from "@tanstack/solid-query";

/** The transport + client both connect-query helpers require. One transport
 *  instance app-wide (query keys embed a Transport reference) and one explicit
 *  `QueryClient` (§A2/§A3): both come from `createLiveClients` / the boot. */
export interface ConnectQueryDeps {
	readonly transport: Transport;
	readonly queryClient: QueryClient;
}

/** A unary Connect method → a Solid Query. `input` is a thunk so input signals
 *  stay reactive; `SkipToken` gates the query off (no fetch) when there is
 *  nothing to load. The `QueryClient` is forwarded EXPLICITLY (§A2). */
export function createConnectQuery<
	I extends DescMessage,
	O extends DescMessage,
>(
	schema: DescMethodUnary<I, O>,
	input: () => MessageInitShape<I> | SkipToken,
	opts: ConnectQueryDeps,
): CreateQueryResult<MessageShape<O>> {
	return useQuery(
		() => ({
			...createQueryOptions(schema, input(), { transport: opts.transport }),
		}),
		() => opts.queryClient,
	);
}

/** A cursor-paged unary Connect method → a Solid infinite query, mirroring
 *  `createConnectQuery` over `createInfiniteQueryOptions`. `pageParamKey` names
 *  the input field the cursor writes; `getNextPageParam` derives the next cursor
 *  from the last page (undefined = end of history). Same explicit `QueryClient`
 *  accessor (§A2). */
export function createConnectInfiniteQuery<
	I extends DescMessage,
	O extends DescMessage,
	ParamKey extends keyof MessageInitShape<I>,
>(
	schema: DescMethodUnary<I, O>,
	input: () =>
		| (MessageInitShape<I> & Required<Pick<MessageInitShape<I>, ParamKey>>)
		| SkipToken,
	opts: ConnectQueryDeps & {
		readonly pageParamKey: ParamKey;
		readonly getNextPageParam: GetNextPageParamFunction<
			MessageInitShape<I>[ParamKey],
			MessageShape<O>
		>;
	},
): CreateInfiniteQueryResult<InfiniteData<MessageShape<O>>> {
	return useInfiniteQuery(
		() => ({
			...createInfiniteQueryOptions(schema, input(), {
				transport: opts.transport,
				pageParamKey: opts.pageParamKey,
				getNextPageParam: opts.getNextPageParam,
			}),
		}),
		() => opts.queryClient,
	);
}
