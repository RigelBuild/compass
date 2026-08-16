// The client-mode boot gate: the sibling of `bootConnection` for the native
// desktop shell. Unlike the browser dev path (which resolves a connection from
// the Vite env and boots straight through), client mode cannot dial until the
// shell has ARMED a connection — the bearer lives shell-side only (DL-109) and
// the shell learns whether it can reach the server only by probing. So boot
// fires the one auto-connect probe `shellConnect("")` (the empty-token sentinel
// meaning "use the stored token"), shows a `connecting` state while it is in
// flight, and branches on the classified result:
//
//   - ok               → resolve the native provider and hand the connection to
//                         the existing boot chain (index.tsx main()), UNIFORMLY
//                         (no mode-conditional above the transport seam).
//   - any failure kind → render the connect screen and keep it up, retrying in
//                         place from the token input, until a probe succeeds.
//
// This is a boot GATE, not a router route (OQ-2): it owns #root before the app
// mounts, exactly as `renderBootError` does, painting DOM nodes (never
// innerHTML). The token pasted into the input is passed to `shellConnect(token)`
// and then the input is cleared; NO module-scope binding ever holds it and
// nothing writes it to storage (DL-109).

import {
	type ConnectResult,
	nativeConnectionProvider,
	shellConnect,
} from "./daemon-transport";
import type { ConnectionProvider, ResolvedConnection } from "./live/provider";
import { shellServerUrl } from "./shell-globals";

// The transport seam `bootNativeClient` consumes, injectable so a test drives
// stubs directly rather than replacing the whole `./daemon-transport` module
// (Bun's `mock.module` is process-global and its restore does not reliably
// rebind a sibling suite's named imports, so a whole-module mock leaks across
// files and turns test outcomes order-dependent). Production callers omit it and
// get the real transport via `defaultNativeBootDeps`.
export type NativeBootDeps = {
	shellConnect: (token: string) => Promise<ConnectResult>;
	nativeConnectionProvider: (baseUrl: string) => ConnectionProvider;
};

const defaultNativeBootDeps: NativeBootDeps = {
	shellConnect,
	nativeConnectionProvider,
};

// The bare-page styling the gate shares with `renderBootError` (boot.ts): this
// screen runs before any stylesheet is guaranteed loaded, so it inlines the few
// declarations that keep it legible. Kept in sync in spirit with boot.ts, not
// imported, because the two screens differ in layout (this one has inputs).
const SCREEN_STYLE = [
	"margin:0",
	"padding:2rem",
	"font:14px/1.6 ui-monospace,SFMono-Regular,Menlo,monospace",
	"color:#e6e6e6",
	"background:#1a1a1a",
	"min-height:100vh",
].join(";");
const HEADING_STYLE = "margin:0 0 1rem;font-size:1rem";
const URL_STYLE = ["margin:0 0 1.5rem", "color:#9c9c9c"].join(";");
const DETAIL_STYLE = [
	"margin:0 0 1rem",
	"white-space:pre-wrap",
	"color:#ff9c9c",
].join(";");
const INPUT_STYLE = [
	"width:100%",
	"box-sizing:border-box",
	"padding:0.5rem",
	"font:inherit",
	"color:#e6e6e6",
	"background:#111",
	"border:1px solid #444",
	"border-radius:4px",
].join(";");
const BUTTON_STYLE = [
	"margin-top:1rem",
	"padding:0.5rem 1rem",
	"font:inherit",
	"cursor:pointer",
].join(";");

/** The per-failure-kind screen copy (design failure-state table). `heading` is
 *  the one-line theme; `hint` is the actionable follow-up. `other` shows the
 *  server's own safe message — never a silent fallthrough. */
function failureCopy(result: ConnectResult): { heading: string; hint: string } {
	switch (result.kind) {
		case "bad-url":
			return {
				heading: "Can't reach the host",
				hint: "Check the server URL below is reachable, then paste your token and connect.",
			};
		case "bad-cert":
			return {
				heading: "Can't verify the server's certificate",
				hint: "The server's TLS certificate could not be verified — check its ca_cert, then try again.",
			};
		case "bad-token":
			return {
				heading: "The server rejected this token",
				hint: "Paste a valid token and connect.",
			};
		case "version-mismatch":
			return {
				heading: `App speaks compass.v1; server speaks ${result.apiVersion}`,
				hint: "The app and the server disagree on the API version — upgrade whichever is behind.",
			};
		default:
			return {
				heading: "Could not connect",
				hint: result.message,
			};
	}
}

/** Fire the auto-connect probe, then either resolve the native connection or
 *  keep the connect screen up until a user-driven `shellConnect(token)` wins.
 *  Returns undefined only if the user genuinely cannot proceed; in practice it
 *  resolves once a probe succeeds (the screen retries in place). */
export async function bootNativeClient(
	root: HTMLElement,
	deps: NativeBootDeps = defaultNativeBootDeps,
): Promise<ResolvedConnection | undefined> {
	renderConnecting(root);

	// The single boot-internal auto-connect probe (the empty-token sentinel).
	// A user can never fire this: the connect button is disabled on empty input.
	const probe = await deps.shellConnect("");
	if (probe.ok) {
		return deps.nativeConnectionProvider(shellServerUrl() ?? "").resolve();
	}

	// The probe failed: hand off to the connect screen, which owns #root and
	// resolves only when a user-driven connect succeeds.
	return awaitUserConnect(root, probe, deps);
}

/** Paint the in-flight `connecting` state into `root`. */
function renderConnecting(root: HTMLElement): void {
	const screen = document.createElement("div");
	screen.setAttribute("style", SCREEN_STYLE);
	const headingEl = document.createElement("h1");
	headingEl.setAttribute("style", HEADING_STYLE);
	headingEl.textContent = "Connecting…";
	const urlEl = document.createElement("p");
	urlEl.setAttribute("style", URL_STYLE);
	urlEl.textContent = shellServerUrl() ?? "";
	screen.append(headingEl, urlEl);
	root.replaceChildren(screen);
}

/** Render the connect screen for a failed probe and keep it up, driving
 *  `shellConnect(token)` from the token input on each submit, until a probe
 *  succeeds — then resolve the native connection. The token lives only in the
 *  input's live value for the duration of one call and is cleared after (no
 *  module-scope binding, DL-109). */
function awaitUserConnect(
	root: HTMLElement,
	initial: ConnectResult,
	deps: NativeBootDeps,
): Promise<ResolvedConnection> {
	return new Promise<ResolvedConnection>((resolve) => {
		const screen = document.createElement("div");
		screen.setAttribute("style", SCREEN_STYLE);

		const headingEl = document.createElement("h1");
		headingEl.setAttribute("style", HEADING_STYLE);
		screen.append(headingEl);

		const detailEl = document.createElement("p");
		detailEl.setAttribute("style", DETAIL_STYLE);
		screen.append(detailEl);

		const urlEl = document.createElement("p");
		urlEl.setAttribute("style", URL_STYLE);
		// Read-only: the server URL is fixed by the shell, not editable here.
		urlEl.textContent = `Server: ${shellServerUrl() ?? ""}`;
		screen.append(urlEl);

		const input = document.createElement("input");
		input.setAttribute("style", INPUT_STYLE);
		input.type = "password";
		input.placeholder = "Paste your token";
		input.autocomplete = "off";
		screen.append(input);

		const button = document.createElement("button");
		button.setAttribute("style", BUTTON_STYLE);
		button.type = "button";
		button.textContent = "Connect";
		screen.append(button);

		// Submit is disabled on empty input, so the empty-token "use-the-stored-one"
		// sentinel is only ever the boot-internal probe, never a user action.
		const syncDisabled = (): void => {
			button.disabled = input.value.length === 0;
		};
		input.addEventListener("input", syncDisabled);

		// Paint the failure kind the probe returned.
		const paint = (result: ConnectResult): void => {
			const { heading, hint } = failureCopy(result);
			headingEl.textContent = heading;
			detailEl.textContent = hint;
		};
		paint(initial);
		syncDisabled();

		const submit = (): void => {
			const token = input.value;
			if (token.length === 0) {
				return;
			}
			button.disabled = true;
			// Clear the input immediately: the token is now in flight to the shell
			// and must never linger UI-side (DL-109). No binding retains it.
			input.value = "";
			void deps.shellConnect(token).then((result) => {
				if (result.ok) {
					resolve(
						deps.nativeConnectionProvider(shellServerUrl() ?? "").resolve(),
					);
					return;
				}
				// Retry in place: re-render the matching failure state, keep the
				// screen up, and re-enable submit once the user types again.
				paint(result);
				syncDisabled();
			});
		};
		button.addEventListener("click", submit);

		root.replaceChildren(screen);
	});
}
