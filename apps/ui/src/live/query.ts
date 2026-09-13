// The connect-query-core → solid-query glue seam (query record §A2). connect-query-core
// ships React hooks only, so this maps a core descriptor to a `@tanstack/solid-query`
// call. Options are a THUNK (a reactive `input` re-keys/refetches); the `QueryClient` is
// passed EXPLICITLY, never from context (store-internal queries have no provider ancestor).

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
