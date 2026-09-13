import { bootConnection } from "./boot";
import { bootNativeClient } from "./boot-native";
import { nativeConnectionProvider } from "./daemon-transport";
import type { ConnectionProvider, ResolvedConnection } from "./live/provider";
import { envConnectionProvider } from "./live/provider";
import { type ShellMode, shellServerUrl } from "./shell-globals";

/** The launch mode `bootForMode` dispatches on: the shell-injected `ShellMode`,
 *  or undefined in a browser dev build where no shell sets it. */
export type BootMode = ShellMode | undefined;

export type BootModeDeps = {
	bootNativeClient: (
		root: HTMLElement,
	) => Promise<ResolvedConnection | undefined>;
	embeddedConnectionProvider: () => ConnectionProvider;
	envConnectionProvider: () => ConnectionProvider;
	bootConnection: (
		root: HTMLElement,
		resolve: () => Promise<ResolvedConnection>,
	) => Promise<ResolvedConnection | undefined>;
};

export const defaultDeps: BootModeDeps = {
	bootNativeClient,
	// Embedded never receives __COMPASS_SERVER_URL__ (injected in client mode only), and the
	// bridge fetch routes over Wails IPC by path — so this is a syntactic same-origin
	// placeholder, never dialed. Must be ABSOLUTE: createDaemonFetch does `new Request(url)`,
	// which rejects a relative URL. Matches the packages/compass-client convention.
	embeddedConnectionProvider: () =>
		nativeConnectionProvider(shellServerUrl() ?? "http://compass.localhost"),
	envConnectionProvider,
	bootConnection,
};

/** Select the runtime boot thunk for the shell-injected launch mode. */
export function bootForMode(
	mode: BootMode,
	root: HTMLElement,
	deps: BootModeDeps = defaultDeps,
): () => Promise<ResolvedConnection | undefined> {
	switch (mode) {
		case "client":
			return () => deps.bootNativeClient(root);
		case "embedded":
			return () =>
				deps.bootConnection(root, () =>
					deps.embeddedConnectionProvider().resolve(),
				);
		default:
			return () =>
				deps.bootConnection(root, () => deps.envConnectionProvider().resolve());
	}
}
