// Live and fixture boot share this shell mount and query defaults.

import { createRouter, hashHistory, type RouterHistory } from "@solidjs/router";
import { render } from "@solidjs/web";
import { QueryClient, QueryClientProvider } from "@tanstack/solid-query";
import App from "./App";
import { RefLinkRulesContext, StoreContext } from "./context";
import type { LiveClients } from "./live/client";
import { applyReduceMotion } from "./preferences";
import { appRoutes } from "./routes";
import type { AppStore } from "./store";

/** The one app-lifetime QueryClient shape — the server-state cache the query
 *  layer keys against (query record §A1). The single source of the app's query
 *  defaults: both the live boot (`index.tsx main()`) and the fixture boot
 *  (`boot-fixture.ts`) build their client here so they cannot drift. */
export function newAppQueryClient(): QueryClient {
	return new QueryClient({
		defaultOptions: { queries: { retry: 1, staleTime: 30_000 } },
	});
}

/** `hashHistory` whose reads carry the entry's state, so back/forward sees the
 *  view id a navigation stamped (record A2). It also listens to `popstate`:
 *  two entries with the same hash but different views fire no `hashchange`. */
export function stateHashHistory(): RouterHistory {
	const base = hashHistory();
	const read = (): { value: string; state: unknown } => {
		const value = base.get();
		return {
			value: typeof value === "string" ? value : value.value,
			state: window.history.state,
		};
	};
	const baseInit = base.init;
	return {
		...base,
		get: read,
		init: (notify) => {
			const onPop = (): void => notify(read());
			const stopBase = baseInit?.(onPop);
			window.addEventListener("popstate", onPop);
			return () => {
				stopBase?.();
				window.removeEventListener("popstate", onPop);
			};
		},
	};
}

/** Mount the full App shell — the store in a `StoreContext` provider wrapping
 *  the `QueryClientProvider` and the router instance, whose render-prop child is
 *  the `App` root layout receiving the matched route as `props.children`. Router
 *  2 builds one immutable instance per app; the hash history keeps the shell's
 *  in-URL routing. Returns solid-js/web `render`'s disposer so a test that mounts
 *  the real shell can tear it down (production boot ignores it — the app lives
 *  for the process). */
export function mountShell(
	root: HTMLElement,
	store: AppStore,
	queryClient: QueryClient,
	clients?: Pick<LiveClients, "comms" | "compass">,
): () => void {
	applyReduceMotion(document.documentElement, store.reduceMotion());
	const Router = createRouter({
		routes: appRoutes,
		history: stateHashHistory(),
	});
	return render(
		() => (
			<StoreContext value={store}>
				<RefLinkRulesContext value={store.refLinkRules}>
					<QueryClientProvider client={queryClient}>
						<Router>{(props) => <App {...props} clients={clients} />}</Router>
					</QueryClientProvider>
				</RefLinkRulesContext>
			</StoreContext>
		),
		root,
	);
}
