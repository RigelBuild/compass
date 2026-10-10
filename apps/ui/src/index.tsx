import { createRoot } from "solid-js";
import { bootCaller, renderBootError } from "./boot";
import { bootForMode } from "./boot-mode";
import { composeBoot } from "./compose-boot";
import { resolveCaller } from "./live/client";
import type { ResolvedConnection } from "./live/provider";
import { mountShell, newAppQueryClient } from "./mount";
import { shellMode } from "./shell-globals";
import { createAppStore } from "./store";
import { sessionLayoutStorage } from "./window-layout";

const root = document.getElementById("root");
if (!root) {
	throw new Error("missing #root element");
}

// Dispatch on fixture build mode, then shell-injected mode, before IPC.
// Client, embedded, and browser-dev share main(); fixture mounts offline.
//
// Keep the fixture comparison inline: Vite folds it in production, eliminating
// the fixture branch and its dynamically imported chunk.
if (import.meta.env.MODE === "fixture") {
	void import("./boot-fixture")
		.then((m) => m.bootFixture(root))
		.catch((error) => {
			renderBootError(
				root,
				"Compass UI cannot start",
				error instanceof Error ? error.message : String(error),
				"This is the offline fixture build.",
			);
		});
} else {
	// Undefined means bootConnection painted a resolve error; client mode stays
	// pending behind its connect gate. The catch handles other boot rejections.
	const bootConnectionForMode = bootForMode(shellMode(), root);
	void bootConnectionForMode()
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
}

// Resolve identity before building the store. A failed WhoAmI paints the boot
// error screen and stops here because the app cannot scope listings without it.
async function main(
	root: HTMLElement,
	connection: ResolvedConnection,
): Promise<void> {
	// Analytics is off without a project key. Trace and session correlation are
	// best-effort; composeBoot's construction order is load-bearing.
	const { analytics, clients } = composeBoot({ connection });

	const callerId = await bootCaller(root, () => resolveCaller(clients.compass));
	// bootCaller paints the WhoAmI failure screen; without an identity, stop boot.
	if (!callerId) {
		return;
	}

	// Identify the caller we just learned via WhoAmI so events attach to a stable
	// distinct id. This stays AFTER bootCaller: the id is its output.
	analytics.identify(callerId);

	// One app-lifetime QueryClient is shared by store internals and components.
	// The store runs outside QueryClientProvider, so it receives the client directly.
	const queryClient = newAppQueryClient();

	// The app-lifetime store uses a stable Solid owner and drives every surface.
	// Its cleanup aborts the comms stream if the owner is ever disposed.
	const store = createRoot(() =>
		createAppStore({
			comms: clients.comms,
			compass: clients.compass,
			transport: clients.transport,
			// Read after WhoAmI so the resume read keys on the right account. App
			// opens the tour when the claim is won.
			tour: clients.compass,
			claimFirstRun: true,
			queryClient,
			callerId,
			// Namespace persisted UI prefs (the pinned-agent set) to this
			// deployment, so one server/workspace's account ids never hydrate as
			// pins on another (Record A §T3). The door URL + caller identity is
			// the stable key.
			workspaceKey: `${connection.baseUrl}#${callerId}`,
			layoutStorage: sessionLayoutStorage(),
			// The one failure funnel: a comms stream/write error AND a refused
			// StopAgentSession (Runner-backed — `Unavailable` when the server has
			// no RunnerHub attached) land here, so neither is swallowed.
			onCommsError: (error) => {
				// biome-ignore lint/suspicious/noConsole: top-level comms-error funnel in the app entrypoint
				console.error(
					"compass live error",
					error instanceof Error ? error.message : String(error),
				);
			},
		}),
	);

	mountShell(root, store, queryClient, clients);
}
