import { createRoot } from "solid-js";
import { render } from "solid-js/web";
import App from "./App";
import { bootConnection } from "./boot";
import { StoreContext } from "./context";
import { createLiveClients } from "./live/client";
import { connectionFromEnv } from "./live/connection";
import { createAppStore } from "./store";

const root = document.getElementById("root");
if (!root) {
	throw new Error("missing #root element");
}

// The live connection, resolved once at boot from the Vite env (baseUrl +
// bearer + caller account id), and the typed compass.v1 clients built over it.
// Client construction is pure — no request is sent until the store opens the
// SubscribeComms stream.
//
// Resolution is required and can fail: a missing VITE_COMPASS_BASE_URL or
// VITE_COMPASS_CALLER_ID throws by design (live/connection.ts:52-73).
// bootConnection catches that at the boundary and paints the resolver's own
// message into #root, so a misconfigured env is a readable screen naming the
// variable rather than the blank page a throw escaping module init used to
// leave. Undefined means there is nothing valid to dial — we stop rather than
// boot against a wrong default, and the error screen is the whole UI.
const connection = bootConnection(root, connectionFromEnv);
if (!connection) {
	throw new Error(
		"compass: boot aborted — see the configuration error rendered in #root",
	);
}
const clients = createLiveClients(connection);

// The store is an app-lifetime singleton; createRoot gives its memos a stable
// owner (intentionally never disposed) so Solid doesn't warn about computations
// created before render() establishes a root. One unified store drives every
// surface: the board, the per-agent workspace, and the channel conversation.
//
// The owner also scopes the comms stream: the store registers an onCleanup that
// aborts it, so disposing this root (never, in the app) tears the subscription
// down. The caller comes from the connection — the interim identity seam until
// the server surfaces it (live/connection.ts:28-35).
const store = createRoot(() =>
	createAppStore({
		comms: clients.comms,
		compass: clients.compass,
		callerId: connection.callerId,
		// The one failure funnel: a comms stream/write error AND a refused
		// StopAgentSession (Runner-backed — `Unavailable` when the server has no
		// RunnerHub attached) land here, so neither is swallowed.
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
			<App />
		</StoreContext.Provider>
	),
	root,
);
