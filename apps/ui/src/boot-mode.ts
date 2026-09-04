import { bootConnection } from "./boot";
import { bootNativeClient } from "./boot-native";
import { nativeConnectionProvider } from "./daemon-transport";
import type { ConnectionProvider, ResolvedConnection } from "./live/provider";
import { envConnectionProvider } from "./live/provider";
import { shellServerUrl } from "./shell-globals";

export type BootMode = "client" | "embedded" | undefined;

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
	embeddedConnectionProvider: () =>
		nativeConnectionProvider(shellServerUrl() ?? ""),
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
