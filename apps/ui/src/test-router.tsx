// Use memory history and flush async navigation before routed reads.

import {
	createRouter,
	type MemoryHistoryAdapter,
	memoryHistory,
} from "@solidjs/router";
import { render } from "@solidjs/testing-library";
import App from "./App";
import { STUB_COMMS_STATE } from "./comms-stub";
import { RefLinkRulesContext, StoreContext } from "./context";
import { appRoutes } from "./routes";
import { type AppStore, type AppStoreOptions, createAppStore } from "./store";
import { testQueryClient } from "./test-support";

/** Drain the microtask queue so the route-sync effect runs before a read.
 *  Bounded and timer-free, so it stays deterministic. */
export const flush = async (): Promise<void> => {
	for (let i = 0; i < 20; i++) await Promise.resolve();
};

/** Mount the App shell on memory history and return its store, DOM, and history. */
export function mountApp(
	initialPath = "/",
	options: Partial<AppStoreOptions> = {},
): {
	store: AppStore;
	container: HTMLElement;
	history: MemoryHistoryAdapter;
} {
	let store!: AppStore;
	const history = memoryHistory(initialPath);
	const Router = createRouter({ routes: appRoutes, history });
	const { container } = render(() => {
		store = createAppStore({
			initialComms: STUB_COMMS_STATE,
			queryClient: testQueryClient(),
			...options,
		});
		return (
			<StoreContext value={store}>
				<RefLinkRulesContext value={store.refLinkRules}>
					<Router>{(props) => <App {...props} />}</Router>
				</RefLinkRulesContext>
			</StoreContext>
		);
	});
	return { store, container, history };
}
