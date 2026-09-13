// Typed readers for the two startup globals the desktop shell injects into the webview
// (T5.6, OQ-8): the launch mode and the server URL. The SINGLE synchronous no-IPC source
// for how boot dispatches, since the entry point must pick env vs native-client BEFORE any
// Go getter is reachable. In a browser dev build both are absent (readers → undefined).

/** The shell-injected launch mode. Client boots the connect-screen probe;
 *  embedded resolves the bridge connection directly. Owned here — the single
 *  source of truth both the injected global and every boot consumer name. */
export type ShellMode = "embedded" | "client";

declare global {
	interface Window {
		__COMPASS_MODE__?: ShellMode;
		__COMPASS_SERVER_URL__?: string;
	}
}

/** The shell-injected launch mode, or undefined in a browser dev build (no
 *  shell). typeof-guarded so it never throws when `window` is absent. */
export function shellMode(): ShellMode | undefined {
	if (typeof window === "undefined") {
		return undefined;
	}
	return window.__COMPASS_MODE__;
}

/** The shell-injected server URL the client-mode connect screen displays and
 *  dials, or undefined outside the shell. typeof-guarded, never throws. */
export function shellServerUrl(): string | undefined {
	if (typeof window === "undefined") {
		return undefined;
	}
	return window.__COMPASS_SERVER_URL__;
}
