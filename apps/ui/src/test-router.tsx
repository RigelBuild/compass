// Shared test mount helper (record A4). Component tests route via
// `MemoryRouter` — Solid Router's in-memory integration — never real
// `location.hash`, which is shared global state that would leak across tests
// and break `bun test --conditions browser` determinism.
//
// It mirrors index.tsx's production shape: the store in a `StoreContext.Provider`
// wrapping the router, rendering the SAME `AppRoutes` table (one shared route
// module — no prod/test drift). `initialPath` seeds the memory history so a test
// can mount straight onto a deep-link.
//
// Under the real (memory) router navigation is ASYNCHRONOUS: an action
// (`openChannel`/`openAgent`/`show*`) navigates, the location updates, and the
// store's route-sync effect writes `view`/`selectedChannelId`/`selectedAgentId`
// one reactive tick later. Tests await `flush()` between an action and a routed
// read (record A2/A4).

import { createMemoryHistory, MemoryRouter } from "@solidjs/router";
import { render } from "@solidjs/testing-library";
import App from "./App";
import { STUB_COMMS_STATE } from "./comms-stub";
import { StoreContext } from "./context";
import { AppRoutes } from "./routes";
import { type AppStore, createAppStore } from "./store";
import { testQueryClient } from "./test-support";

/** Drain the microtask queue so the route-sync effect runs before a read.
 *  Bounded and timer-free, so it stays deterministic. */
export const flush = async (): Promise<void> => {
	for (let i = 0; i < 20; i++) await Promise.resolve();
};

/** Mount the full App shell over a fixture-backed store on `initialPath`,
 *  through the shared route table on a MemoryRouter. Returns the live store
 *  (to drive actions) and container (to query the DOM). */
export function mountApp(initialPath = "/"): {
	store: AppStore;
	container: HTMLElement;
} {
	let store!: AppStore;
	const history = createMemoryHistory();
	history.set({ value: initialPath });
	const { container } = render(() => {
		store = createAppStore({
			initialComms: STUB_COMMS_STATE,
			queryClient: testQueryClient(),
		});
		return (
			<StoreContext.Provider value={store}>
				<MemoryRouter history={history} root={App}>
					<AppRoutes />
				</MemoryRouter>
			</StoreContext.Provider>
		);
	});
	return { store, container };
}
