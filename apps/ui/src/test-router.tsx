// Shared test mount helper (record A4). Component tests route via a
// `memoryHistory` adapter — Solid Router's in-memory history — never real
// `location.hash`, which is shared global state that would leak across tests
// and break `bun test --conditions browser` determinism.
//
// It mirrors mount.tsx's production shape: the store in a `StoreContext`
// provider wrapping the router instance, whose render-prop child is the `App`
// root layout rendering the SAME `appRoutes` table (one shared route module —
// no prod/test drift). `initialPath` seeds the memory history so a test can
// mount straight onto a deep-link. Router 2 builds one instance per mount, so
// each `mountApp` gets a fresh `memoryHistory(initialPath)`.
//
// Under the memory router navigation is ASYNCHRONOUS: an action
// (`openChannel`/`openAgent`/`show*`) navigates, the location updates, and the
// store's route-sync effect writes `view`/`selectedChannelId`/`selectedAgentId`
// one reactive tick later. Tests await `flush()` between an action and a routed
// read (record A2/A4).

import { createRouter, memoryHistory } from "@solidjs/router";
import { render } from "@solidjs/testing-library";
import App from "./App";
import { STUB_COMMS_STATE } from "./comms-stub";
import { StoreContext } from "./context";
import { appRoutes } from "./routes";
import { type AppStore, createAppStore } from "./store";
import { testQueryClient } from "./test-support";

/** Drain the microtask queue so the route-sync effect runs before a read.
 *  Bounded and timer-free, so it stays deterministic. */
export const flush = async (): Promise<void> => {
	for (let i = 0; i < 20; i++) await Promise.resolve();
};

/** Mount the full App shell over a fixture-backed store on `initialPath`,
 *  through the shared route table on a memory-history router. Returns the live
 *  store (to drive actions) and container (to query the DOM). */
export function mountApp(initialPath = "/"): {
	store: AppStore;
	container: HTMLElement;
} {
	let store!: AppStore;
	const Router = createRouter({
		routes: appRoutes,
		history: memoryHistory(initialPath),
	});
	const { container } = render(() => {
		store = createAppStore({
			initialComms: STUB_COMMS_STATE,
			queryClient: testQueryClient(),
		});
		return (
			<StoreContext value={store}>
				<Router>{(props) => <App {...props} />}</Router>
			</StoreContext>
		);
	});
	return { store, container };
}
