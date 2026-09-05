// Typed readers for the two startup globals the desktop shell injects into the
// webview at window creation (T5.6, OQ-8): the launch mode and the server URL.
// They are the SINGLE synchronous, no-IPC source of truth for how boot should
// dispatch — the entry point (index.tsx) must pick the env-provider vs the
// native-client boot path BEFORE any Go getter is reachable (a Go getter is an
// IPC call only available inside the Wails shell), so the shell hands the mode
// (and, for the client-mode connect screen, the read-only server URL) across the
// window boundary as plain globals rather than a runtime round-trip.
//
// Injected by the shell (`application.WebviewWindowOptions` in run(), T5.6); in
// a browser dev build (no shell) both are simply absent, which the readers
// report as undefined so the caller falls back to the unchanged env path.

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
