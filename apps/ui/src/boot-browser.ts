// The browser-mode boot gate: resolve the door from env/origin, then probe WhoAmI with the
// stored (or build-time) bearer. The server answering Unauthenticated means this door needs a
// bearer we lack, so show the token screen and keep the accepted token in localStorage. Any
// other probe failure is not ours to gate: hand the connection on and let boot report it.

import { isUnauthenticated } from "@compass/client";
import { bootConnection } from "./boot";
import {
	BUTTON_STYLE,
	DETAIL_STYLE,
	HEADING_STYLE,
	INPUT_STYLE,
	SCREEN_STYLE,
	URL_STYLE,
} from "./boot-styles";
import { createLiveClients, resolveCaller } from "./live/client";
import {
	type ConnectionProvider,
	envConnectionProvider,
	type ResolvedConnection,
} from "./live/provider";
import { clearToken, loadToken, saveToken } from "./live/token-store";

const REJECTED = "The server rejected this token";
const PASTE_HINT = "Paste a valid token and connect.";

export type BrowserBootDeps = {
	envConnectionProvider: () => ConnectionProvider;
	/** Resolves when the door accepts `conn`'s credential; rejects otherwise. */
	probe: (conn: ResolvedConnection) => Promise<unknown>;
};

export const defaultBrowserBootDeps: BrowserBootDeps = {
	envConnectionProvider,
	probe: (conn) => resolveCaller(createLiveClients(conn).compass),
};

/** Resolve a browser connection whose bearer the door accepts, prompting for a
 *  token when it does not. Returns undefined only when the env cannot name a
 *  door (bootConnection has painted that failure). */
export async function bootBrowser(
	root: HTMLElement,
	deps: BrowserBootDeps = defaultBrowserBootDeps,
): Promise<ResolvedConnection | undefined> {
	const env = await bootConnection(root, () =>
		deps.envConnectionProvider().resolve(),
	);
	if (!env) {
		return undefined;
	}
	const stored = loadToken(env.baseUrl);
	// Stored first (the user last proved it), then the build-time bearer, then none.
	const candidates = [...new Set([stored, env.token])].filter(Boolean);
	for (const token of candidates.length ? candidates : [undefined]) {
		const conn = { ...env, token };
		try {
			await deps.probe(conn);
			return conn;
		} catch (error) {
			if (!isUnauthenticated(error)) {
				return conn;
			}
			if (token === stored) {
				clearToken(env.baseUrl);
			}
		}
	}
	return awaitToken(root, env, deps, candidates.length > 0);
}

/** Show the token screen until the door accepts a pasted token, then store it. */
function awaitToken(
	root: HTMLElement,
	env: ResolvedConnection,
	deps: BrowserBootDeps,
	rejected: boolean,
): Promise<ResolvedConnection> {
	return new Promise<ResolvedConnection>((resolve) => {
		const screen = document.createElement("form");
		screen.setAttribute("style", SCREEN_STYLE);

		const headingEl = document.createElement("h1");
		headingEl.setAttribute("style", HEADING_STYLE);
		const detailEl = document.createElement("p");
		detailEl.setAttribute("style", DETAIL_STYLE);
		const urlEl = document.createElement("p");
		urlEl.setAttribute("style", URL_STYLE);
		urlEl.textContent = `Server: ${env.baseUrl}`;

		const input = document.createElement("input");
		input.setAttribute("style", INPUT_STYLE);
		input.type = "password";
		input.placeholder = "Paste your token";
		input.autocomplete = "off";

		const button = document.createElement("button");
		button.setAttribute("style", BUTTON_STYLE);
		button.type = "submit";
		button.textContent = "Connect";

		screen.append(headingEl, detailEl, urlEl, input, button);

		const paint = (heading: string, detail: string): void => {
			headingEl.textContent = heading;
			detailEl.textContent = detail;
		};
		const paintFailure = (error: unknown): void => {
			if (isUnauthenticated(error)) {
				paint(REJECTED, PASTE_HINT);
				return;
			}
			paint(
				"Could not connect",
				error instanceof Error ? error.message : String(error),
			);
		};
		// One probe at a time: a second success would store a token other than the one booted.
		let inFlight = false;
		const syncDisabled = (): void => {
			button.disabled = inFlight || input.value.trim().length === 0;
		};
		input.addEventListener("input", syncDisabled);
		paint(rejected ? REJECTED : "This server needs a token", PASTE_HINT);
		syncDisabled();

		screen.addEventListener("submit", (event) => {
			event.preventDefault();
			const token = input.value.trim();
			if (!token || inFlight) {
				return;
			}
			inFlight = true;
			button.disabled = true;
			input.value = "";
			const conn = { ...env, token };
			deps.probe(conn).then(
				() => {
					saveToken(env.baseUrl, token);
					// render() appends to root, so the screen must go before mount.
					root.replaceChildren();
					resolve(conn);
				},
				(error: unknown) => {
					inFlight = false;
					paintFailure(error);
					syncDisabled();
				},
			);
		});

		root.replaceChildren(screen);
		input.focus();
	});
}
