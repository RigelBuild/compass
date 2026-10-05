import { bootConnection } from "./boot";
import { bootBrowser } from "./boot-browser";
import { bootNativeClient } from "./boot-native";
import { bootSetup } from "./boot-setup";
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
};

export const defaultDeps: BootModeDeps = {
	bootNativeClient,
	bootSetup,
	embeddedConnectionProvider: () =>
		nativeConnectionProvider(shellServerUrl() ?? "http://compass.localhost"),
	bootBrowser,
	bootConnection,
};

function showReopen(root: HTMLElement): void {
	const screen = document.createElement("div");
	screen.setAttribute(
		"style",
		"margin:0;padding:2rem;font:14px/1.6 ui-monospace,SFMono-Regular,Menlo,monospace;color:#e6e6e6;background:#1a1a1a;min-height:100vh",
	);
	const message = document.createElement("p");
	message.textContent =
		"Compass is already set up. Quit and reopen it to change this.";
	const button = document.createElement("button");
	button.type = "button";
	button.textContent = "Quit";
	button.addEventListener("click", () => void quitApp());
	screen.append(message, button);
	root.replaceChildren(screen);
}

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
				showReopen(root);
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
