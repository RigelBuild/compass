import { createRoot } from "solid-js";
import { render } from "solid-js/web";
import App from "./App";
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
const connection = connectionFromEnv();
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
