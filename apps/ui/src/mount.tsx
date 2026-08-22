// The shared shell-mount, extracted from index.tsx's `main()` (T1). Both boot
// paths call it with the same arguments in the same order: the live boot
// (`index.tsx main()`) and the offline fixture boot (`boot-fixture.ts`). Keeping
// the render tree in ONE place is what lets the fixture boot mount the identical
// shell without duplicating the JSX block.
//
// Named `mountShell`, NOT `mountApp` — `test-router.tsx:37` already exports a
// test-only `mountApp` (MemoryRouter, `{store,container}` return); a second
// same-named export in the `src` tree would be a grep/import trap.
//
// `newAppQueryClient` is the SINGLE source of the app's query defaults so the
// fixture boot (T2) cannot silently drift from the live client.

import { createRouter, hashHistory } from "@solidjs/router";
import { render } from "@solidjs/web";
import { QueryClient, QueryClientProvider } from "@tanstack/solid-query";
import App from "./App";
import { StoreContext } from "./context";
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
): () => void {
	const Router = createRouter({ routes: appRoutes, history: hashHistory() });
	return render(
		() => (
			<StoreContext value={store}>
				<QueryClientProvider client={queryClient}>
					<Router>{(props) => <App {...props} />}</Router>
				</QueryClientProvider>
			</StoreContext>
		),
		root,
	);
}
