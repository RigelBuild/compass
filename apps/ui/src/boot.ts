// The boot guards for the steps that run BEFORE render(): resolving the live
// connection from the Vite env, and (in index.tsx) learning the caller's own
// account id from the server via the WhoAmI RPC once the transport is up.
//
// `resolveConnection` throws by design when VITE_COMPASS_BASE_URL is absent
// (live/connection.ts) — a live build with no door URL is a misconfiguration,
// and dialing a wrong default would be worse than failing. That requiredness is
// correct and stays. The caller's account id is NOT a resolve-time env throw
// anymore: it is learned from the server via WhoAmI after connect, so a failure
// to learn it is a post-connect RPC failure at a different boundary (index.tsx),
// not a missing-env throw here.
//
// What was wrong before this module existed is where the resolve throw landed:
// at module initialization in index.tsx, so it killed the module before render()
// ran and the developer saw an empty page with only a console error. So this
// module catches at that boundary and paints the message into #root. Split the
// same way connection.ts split itself: `bootConnection` is pure over its inputs
// (an element + a connect thunk) and unit-testable; index.tsx is the thin
// wrapper that passes the real root and `connectionFromEnv`.
//
// `renderBootError` is the shared screen-painter both boot failure paths use —
// the env-resolve failure here and the WhoAmI failure in index.tsx — so the two
// screens never drift in style, only in their (path-specific) wording.

import type { ResolvedConnection } from "./live/provider";

// Inline styles, not a class from app.css: this screen is the failure path for
// boot itself, so it must not depend on a stylesheet having loaded. Kept to the
// few declarations that make a stack-trace-free message readable on a bare page.
const SCREEN_STYLE = [
	"margin:0",
	"padding:2rem",
	"font:14px/1.6 ui-monospace,SFMono-Regular,Menlo,monospace",
	"color:#e6e6e6",
	"background:#1a1a1a",
	"min-height:100vh",
].join(";");
const DETAIL_STYLE = ["margin:0", "white-space:pre-wrap", "color:#ff9c9c"].join(
	";",
);

/** Paint a boot-failure screen into `root` from DOM nodes (never innerHTML:
 *  `detail` is env- or server-derived text and textContent can never be parsed
 *  as markup). `replaceChildren`, not append: a second boot into the same root
 *  must replace the screen rather than stack a second copy. Both boot failure
 *  paths (the env-resolve throw here and the WhoAmI RPC failure in index.tsx)
 *  render through this one painter so their screens never drift. */
export function renderBootError(
	root: HTMLElement,
	heading: string,
	detail: string,
	hint: string,
): void {
	const screen = document.createElement("div");
	screen.setAttribute("style", SCREEN_STYLE);
	const headingEl = document.createElement("h1");
	headingEl.setAttribute("style", "margin:0 0 1rem;font-size:1rem");
	headingEl.textContent = heading;
	const message = document.createElement("p");
	message.setAttribute("style", DETAIL_STYLE);
	message.textContent = detail;
	const hintEl = document.createElement("p");
	hintEl.setAttribute("style", "margin:1.5rem 0 0;color:#9c9c9c");
	hintEl.textContent = hint;
	screen.append(headingEl, message, hintEl);
	root.replaceChildren(screen);
}

/** Resolve the connection through its provider, or paint the failure into `root`
 *  and return undefined. The async mirror of `bootCaller`: `resolve` is a thunk
 *  over a ConnectionProvider (index.tsx passes
 *  `() => envConnectionProvider().resolve()`), so this stays pure over its inputs
 *  (a root + a thunk) and unit-testable without `import.meta`. Returning
 *  undefined rather than a fallback is the point: the caller has no connection to
 *  boot against, so the app must NOT come up. The rendered detail is the thrown
 *  error's OWN message — connection.ts owns the wording (which variable, what to
 *  set it to), and duplicating it here would let the two drift. */
export async function bootConnection(
	root: HTMLElement,
	resolve: () => Promise<ResolvedConnection>,
): Promise<ResolvedConnection | undefined> {
	try {
		return await resolve();
	} catch (error) {
		// A non-Error throw still has to be legible — String() over silence.
		const detail = error instanceof Error ? error.message : String(error);
		renderBootError(
			root,
			"Compass UI cannot start: misconfigured environment",
			detail,
			"Set it in apps/ui/.env.local (or the shell running the dev server), " +
				"then restart. apps/ui/.env.development holds the checked-in defaults " +
				"for a local `devenv up` server.",
		);
		return undefined;
	}
}

/** Resolve the caller's account id, or paint the failure into `root` and return
 *  undefined — the async mirror of `bootConnection` for the one post-connect
 *  step that must complete before render(). `resolve` is a thunk over the live
 *  compass client (index.tsx passes `() => resolveCaller(clients.compass)`), so
 *  this stays pure over its inputs (a root + a thunk) and unit-testable without
 *  a real transport. A rejection means the server answered but we could not
 *  learn "me" — a different boundary from the env-resolve throw above, so its
 *  screen names that distinct cause, but both render through `renderBootError`.
 *  Undefined is the same stop signal `bootConnection` returns: the app must NOT
 *  come up without a caller (it scopes every listing and drives rail
 *  membership). */
export async function bootCaller(
	root: HTMLElement,
	resolve: () => Promise<string>,
): Promise<string | undefined> {
	try {
		return await resolve();
	} catch (error) {
		const detail = error instanceof Error ? error.message : String(error);
		renderBootError(
			root,
			"Compass UI cannot start: could not learn the caller identity",
			detail,
			"The server was reached but the WhoAmI request failed, so the UI " +
				"cannot determine which account it is connected as. Check that the " +
				"server is healthy and the bearer token is valid, then reload.",
		);
		return undefined;
	}
}
