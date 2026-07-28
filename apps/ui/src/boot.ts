// The boot guard for the one step that runs BEFORE render(): resolving the live
// connection from the Vite env.
//
// `resolveConnection` throws by design when VITE_COMPASS_BASE_URL or
// VITE_COMPASS_CALLER_ID is absent (live/connection.ts:52-73) — a live build with
// no door URL or no caller identity is a misconfiguration, and dialing a wrong
// default or deriving membership against a wrong "me" would be worse than
// failing. That requiredness is correct and stays. What was wrong is where the
// throw landed: at module initialization in index.tsx, so it killed the module
// before render() ran and the developer saw an empty page with only a console
// error. The resolver's messages already say exactly which variable is missing
// and what to set it to — they just never reached a human.
//
// So this module catches at that boundary and paints the message into #root.
// Split the same way connection.ts split itself: `bootConnection` is pure over
// its inputs (an element + a connect thunk) and unit-testable; index.tsx is the
// thin wrapper that passes the real root and `connectionFromEnv`.

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

		// Built from DOM nodes, not innerHTML: `detail` is env-derived text and
		// textContent can never be parsed as markup.
		const screen = document.createElement("div");
		screen.setAttribute("style", SCREEN_STYLE);
		const heading = document.createElement("h1");
		heading.setAttribute("style", "margin:0 0 1rem;font-size:1rem");
		heading.textContent = "Compass UI cannot start: misconfigured environment";
		const message = document.createElement("p");
		message.setAttribute("style", DETAIL_STYLE);
		message.textContent = detail;
		const hint = document.createElement("p");
		hint.setAttribute("style", "margin:1.5rem 0 0;color:#9c9c9c");
		hint.textContent =
			"Set it in apps/ui/.env.local (or the shell running the dev server), " +
			"then restart. apps/ui/.env.development holds the checked-in defaults " +
			"for a local `devenv up` server.";
		screen.append(heading, message, hint);

		// replaceChildren, not append: a second boot into the same root must
		// replace the screen rather than stack a second copy under the first.
		root.replaceChildren(screen);
		return undefined;
	}
}
