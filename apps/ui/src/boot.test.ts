import { describe, expect, test } from "bun:test";
import { bootConnection } from "./boot";
import { resolveConnection } from "./live/connection";

// boot.ts guards the one unrecoverable step that runs BEFORE render(): resolving
// the connection from the env. `resolveConnection` throws by design on a missing
// VITE_COMPASS_BASE_URL / VITE_COMPASS_CALLER_ID (connection.ts:60-73), and that
// throw used to escape module initialization in index.tsx — killing the module
// before render() ever ran, so the developer got a BLANK #root and a console
// error nobody looks at.
//
// `bootConnection` is the pure, testable half (the same split connection.ts made
// between `resolveConnection` and `connectionFromEnv`): it takes the root element
// and a connect thunk, so these tests drive it with plain env objects — no
// `import.meta`, no real DOM document. What it defends:
//   - the failure is VISIBLE in #root, and names the exact variable, by
//     SURFACING the thrown Error's own message rather than restating it;
//   - a successful resolve is passed through untouched and writes nothing;
//   - requiredness is preserved — a failed boot returns undefined, so the caller
//     cannot fall through and boot the app against a wrong default.

const VALID = {
	VITE_COMPASS_BASE_URL: "http://127.0.0.1:50051",
	VITE_COMPASS_CALLER_ID: "acc-me",
};

describe("bootConnection", () => {
	test("returns the resolved connection and leaves the root untouched", () => {
		const el = document.createElement("div");
		const connection = bootConnection(el, () => resolveConnection(VALID));

		expect(connection).toEqual({
			baseUrl: "http://127.0.0.1:50051",
			token: undefined,
			callerId: "acc-me",
		});
		// Nothing rendered: the real render() owns the root on the happy path.
		expect(el.childNodes.length).toBe(0);
	});

	// The two required-env legs. Each asserts the rendered text carries the
	// resolver's OWN message (recomputed here from the same thrown Error, never a
	// copied literal) — so rewording connection.ts's guidance can never silently
	// leave the boot screen showing stale instructions.
	for (const [label, env, variable] of [
		["baseUrl", { VITE_COMPASS_CALLER_ID: "acc-me" }, "VITE_COMPASS_BASE_URL"],
		[
			"callerId",
			{ VITE_COMPASS_BASE_URL: "http://127.0.0.1:50051" },
			"VITE_COMPASS_CALLER_ID",
		],
	] as const) {
		test(`renders the resolver's message into the root when ${label} is missing`, () => {
			const el = document.createElement("div");
			const thrown = (() => {
				try {
					resolveConnection(env);
				} catch (error) {
					return error as Error;
				}
				throw new Error(`resolveConnection(${label}-missing) did not throw`);
			})();

			const connection = bootConnection(el, () => resolveConnection(env));

			// Requiredness preserved: no connection, so the caller cannot boot on.
			expect(connection).toBeUndefined();
			// Visible, not blank — and naming the exact variable to set.
			expect(el.childNodes.length).toBeGreaterThan(0);
			expect(el.textContent).toContain(variable);
			expect(el.textContent).toContain(thrown.message);
		});
	}

	test("renders a non-Error throw rather than blanking", () => {
		const el = document.createElement("div");

		expect(
			bootConnection(el, () => {
				throw "env exploded";
			}),
		).toBeUndefined();
		expect(el.textContent).toContain("env exploded");
	});

	// A second boot into a root that already holds content must REPLACE it, not
	// append — otherwise a retry would stack error screens.
	test("replaces existing root content instead of appending", () => {
		const el = document.createElement("div");
		el.append(document.createElement("span"));

		bootConnection(el, () => resolveConnection({}));
		bootConnection(el, () => resolveConnection({}));

		// The child COUNT is the stacking assertion, and it comes first so the
		// failure reads as `2 !== 1` rather than a serialized-DOM dump: `append`
		// in place of `replaceChildren` leaves two error screens here.
		expect(el.childNodes.length).toBe(1);
		// The pre-existing content is gone, and what remains is the screen.
		expect(el.querySelector("span")).toBeNull();
		expect(el.textContent).toContain("VITE_COMPASS_BASE_URL");
	});
});
