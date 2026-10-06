import { bootConnection } from "./boot";
import { bootBrowser } from "./boot-browser";
import { bootNativeClient } from "./boot-native";
import { bootSetup, renderReopenScreen } from "./boot-setup";
import { nativeConnectionProvider, quitApp } from "./daemon-transport";
import type { ConnectionProvider, ResolvedConnection } from "./live/provider";
import { type ShellMode, shellServerUrl } from "./shell-globals";

export type BootMode = ShellMode | undefined;

export type BootModeDeps = {
	bootNativeClient: (
		root: HTMLElement,
	) => Promise<ResolvedConnection | undefined>;
	bootSetup: (root: HTMLElement) => Promise<ResolvedConnection | undefined>;
	embeddedConnectionProvider: () => ConnectionProvider;
	bootBrowser: (root: HTMLElement) => Promise<ResolvedConnection | undefined>;
	bootConnection: (
		root: HTMLElement,
		resolve: () => Promise<ResolvedConnection>,
	) => Promise<ResolvedConnection | undefined>;
	quitApp: () => Promise<void>;
};
export const defaultDeps: BootModeDeps = {
	bootNativeClient,
	bootSetup,
	// The daemon fetch constructs Requests from this base, so it must be absolute.
	embeddedConnectionProvider: () =>
		nativeConnectionProvider(shellServerUrl() ?? "http://compass.localhost"),
	bootBrowser,
	bootConnection,
	quitApp,
};

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
		case "setup":
			return () => deps.bootSetup(root);
		case "reopen":
			return async () => {
				renderReopenScreen(root, deps.quitApp);
				return undefined;
			};
		case undefined:
			return () => deps.bootBrowser(root);
		default: {
			const exhaustive: never = mode;
			throw new Error(`Unhandled boot mode: ${exhaustive}`);
		}
	}
}
