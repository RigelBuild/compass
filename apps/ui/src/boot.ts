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

import type { Connection } from "./live/connection";

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

/** Resolve the connection, or paint the failure into `root` and return
 *  undefined. Returning undefined rather than a fallback is the point: the
 *  caller has no connection to boot against, so the app must NOT come up. The
 *  rendered detail is the thrown error's OWN message — connection.ts owns the
 *  wording (which variable, what to set it to), and duplicating it here would
 *  let the two drift. */
export function bootConnection(
	root: HTMLElement,
	connect: () => Connection,
): Connection | undefined {
	try {
		return connect();
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
