import { HashRouter } from "@solidjs/router";
import { QueryClient, QueryClientProvider } from "@tanstack/solid-query";
import { createRoot } from "solid-js";
import { render } from "solid-js/web";
import App from "./App";
import { bootCaller, bootConnection, renderBootError } from "./boot";
import { StoreContext } from "./context";
import { createLiveClients, resolveCaller } from "./live/client";
import {
	envConnectionProvider,
	type ResolvedConnection,
} from "./live/provider";
import { AppRoutes } from "./routes";
import { createAppStore } from "./store";

const root = document.getElementById("root");
if (!root) {
	throw new Error("missing #root element");
}

// Boot resolves the connection through a ConnectionProvider (the default env
// provider in the browser dev build; a shell-provided provider in the native
// app). resolve() is async, so the whole boot sequence runs inside a single
// async chain: bootConnection catches a resolve throw at the boundary and paints
// the resolver's own message into #root — a missing VITE_COMPASS_BASE_URL still
// throws by design (live/connection.ts) and still lands on the same failure
// screen — and main() carries on only when a connection resolved. Undefined
// means there is nothing valid to dial, so we stop rather than boot against a
// wrong default, and the error screen is the whole UI. A post-connection boot
// failure (createRoot/createAppStore/render throwing) routes to the same painter
// so a swallowed `void` promise never leaves a blank #root.
void bootConnection(root, () => envConnectionProvider().resolve())
	.then((connection) => {
		if (connection) {
			return main(root, connection);
		}
	})
	.catch((error) => {
		renderBootError(
			root,
			"Compass UI cannot start",
			error instanceof Error ? error.message : String(error),
			"An unexpected error interrupted boot after the connection was " +
				"established. Reload; if it persists, check the console for the " +
				"full stack.",
		);
	});

// The post-connect boot sequence, async because learning the caller requires a
// round-trip: build the clients, ask the server who we are (WhoAmI via
// bootCaller), then build the store and render. bootCaller owns the failure
// boundary — a WhoAmI rejection or an empty id means the server answered but we
// could not learn "me", so it paints the boot-error screen and returns
// undefined; the app genuinely cannot come up (the caller scopes every listing
// and drives rail membership), so undefined stops boot here without rendering.
// This boundary is distinct from a misconfigured env: the connection resolved
// fine; the identity round-trip is what failed.
async function main(
	root: HTMLElement,
	connection: ResolvedConnection,
): Promise<void> {
	const clients = createLiveClients(connection);

	const callerId = await bootCaller(root, () => resolveCaller(clients.compass));
	// Undefined is bootCaller's stop signal — it already painted the WhoAmI
	// failure screen, so the app must not come up (no caller to scope it).
	if (!callerId) {
		return;
	}

	// One app-lifetime QueryClient — the server-state cache the query layer keys
	// against (query record §A1). Built BEFORE the store so the store can hold it
	// explicitly: the store's createRoot owner never sits under
	// QueryClientProvider (that mounts inside render(), below), so store-internal
	// queries pass this client directly rather than resolving it from context
	// (§A3). Components read the SAME instance through the provider — two access
	// paths, one cache, so invalidations and setQueryData from either side are
	// one source of truth.
	const queryClient = new QueryClient({
		defaultOptions: { queries: { retry: 1, staleTime: 30_000 } },
	});

	// The store is an app-lifetime singleton; createRoot gives its memos a stable
	// owner (intentionally never disposed) so Solid doesn't warn about
	// computations created before render() establishes a root. One unified store
	// drives every surface: the board, the per-agent workspace, and the channel
	// conversation.
	//
	// The owner also scopes the comms stream: the store registers an onCleanup
	// that aborts it, so disposing this root (never, in the app) tears the
	// subscription down. The caller was learned from the server via WhoAmI above.
	const store = createRoot(() =>
		createAppStore({
			comms: clients.comms,
			compass: clients.compass,
			queryClient,
			callerId,
			// Namespace persisted UI prefs (the pinned-agent set) to this
			// deployment, so one server/workspace's account ids never hydrate as
			// pins on another (Record A §T3). The door URL + caller identity is
			// the stable key.
			workspaceKey: `${connection.baseUrl}#${callerId}`,
			// The one failure funnel: a comms stream/write error AND a refused
			// StopAgentSession (Runner-backed — `Unavailable` when the server has
			// no RunnerHub attached) land here, so neither is swallowed.
			onCommsError: (error) => {
				console.error(
					"compass live error",
					error instanceof Error ? error.message : String(error),
				);
			},
		}),
	);

	render(
		() => (
			<StoreContext.Provider value={store}>
				<QueryClientProvider client={queryClient}>
					<HashRouter root={App}>
						<AppRoutes />
					</HashRouter>
				</QueryClientProvider>
			</StoreContext.Provider>
		),
		root,
	);
}
