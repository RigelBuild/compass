import { describe, expect, test } from "bun:test";
import { bootCaller, bootConnection, renderBootError } from "./boot";
import { resolveCaller } from "./live/client";
import { createFakeCompass, type FakeCompass } from "./live/compass-fake";
import { resolveConnection } from "./live/connection";

// boot.ts guards the one unrecoverable step that runs BEFORE render(): resolving
// the connection from the env. `resolveConnection` throws by design on a missing
// VITE_COMPASS_BASE_URL (connection.ts), and that throw used to escape module
// initialization in index.tsx — killing the module before render() ever ran, so
// the developer got a BLANK #root and a console error nobody looks at. (The
// caller's account id is no longer an env throw: it is learned from the server
// via WhoAmI after connect, so a failure to learn it is a post-connect RPC
// failure handled in index.tsx, not a resolve-time throw guarded here.)
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
// `renderBootError` is the shared screen-painter both boot failure paths use
// (env-resolve here, WhoAmI failure in index.tsx); its own test defends that it
// paints legible DOM nodes and replaces prior content.

const VALID = {
	VITE_COMPASS_BASE_URL: "http://127.0.0.1:50051",
};

describe("bootConnection", () => {
	test("returns the resolved connection and leaves the root untouched", async () => {
		const el = document.createElement("div");
		const connection = await bootConnection(el, async () => ({
			...resolveConnection(VALID),
			fetchImpl: undefined,
		}));

		expect(connection).toEqual({
			baseUrl: "http://127.0.0.1:50051",
			token: undefined,
			fetchImpl: undefined,
		});
		// Nothing rendered: the real render() owns the root on the happy path.
		expect(el.childNodes.length).toBe(0);
	});

	// The one required-env leg. It asserts the rendered text carries the
	// resolver's OWN message (recomputed here from the same thrown Error, never a
	// copied literal) — so rewording connection.ts's guidance can never silently
	// leave the boot screen showing stale instructions.
	test("renders the resolver's message into the root when baseUrl is missing", async () => {
		const el = document.createElement("div");
		const env = { VITE_COMPASS_TOKEN: "tok" };
		const thrown = (() => {
			try {
				resolveConnection(env);
			} catch (error) {
				return error as Error;
			}
			throw new Error("resolveConnection(baseUrl-missing) did not throw");
		})();

		const connection = await bootConnection(el, async () => ({
			...resolveConnection(env),
			fetchImpl: undefined,
		}));

		// Requiredness preserved: no connection, so the caller cannot boot on.
		expect(connection).toBeUndefined();
		// Visible, not blank — and naming the exact variable to set.
		expect(el.childNodes.length).toBeGreaterThan(0);
		expect(el.textContent).toContain("VITE_COMPASS_BASE_URL");
		expect(el.textContent).toContain(thrown.message);
	});

	test("renders a non-Error throw rather than blanking", async () => {
		const el = document.createElement("div");

		expect(
			await bootConnection(el, async () => {
				throw "env exploded";
			}),
		).toBeUndefined();
		expect(el.textContent).toContain("env exploded");
	});

	// A second boot into a root that already holds content must REPLACE it, not
	// append — otherwise a retry would stack error screens.
	test("replaces existing root content instead of appending", async () => {
		const el = document.createElement("div");
		el.append(document.createElement("span"));

		await bootConnection(el, async () => ({
			...resolveConnection({}),
			fetchImpl: undefined,
		}));
		await bootConnection(el, async () => ({
			...resolveConnection({}),
			fetchImpl: undefined,
		}));

		// The child COUNT is the stacking assertion, and it comes first so the
		// failure reads as `2 !== 1` rather than a serialized-DOM dump: `append`
		// in place of `replaceChildren` leaves two error screens here.
		expect(el.childNodes.length).toBe(1);
		// The pre-existing content is gone, and what remains is the screen.
		expect(el.querySelector("span")).toBeNull();
		expect(el.textContent).toContain("VITE_COMPASS_BASE_URL");
	});
});

describe("renderBootError (shared boot-failure painter)", () => {
	test("paints the heading, detail, and hint as text into the root", () => {
		const el = document.createElement("div");

		renderBootError(el, "cannot learn caller", "whoAmI failed", "reload");

		// The WhoAmI-failure path (index.tsx) renders through this painter, so a
		// legible screen carrying the server-derived detail is what boot shows
		// when the identity round-trip fails.
		expect(el.childNodes.length).toBe(1);
		expect(el.textContent).toContain("cannot learn caller");
		expect(el.textContent).toContain("whoAmI failed");
		expect(el.textContent).toContain("reload");
	});

	test("replaces existing root content instead of appending", () => {
		const el = document.createElement("div");
		el.append(document.createElement("span"));

		renderBootError(el, "h", "d", "hint");
		renderBootError(el, "h", "d", "hint");

		expect(el.childNodes.length).toBe(1);
		expect(el.querySelector("span")).toBeNull();
	});
});

describe("bootCaller (post-connect caller-identity guard)", () => {
	test("returns the caller id the server resolves and leaves the root untouched", async () => {
		const el = document.createElement("div");
		const fake = createFakeCompass();
		fake.whoAmIAccountId.accountId = "acc-x";

		const callerId = await bootCaller(el, () => resolveCaller(fake.client));

		// The id main() threads into the store + workspaceKey is exactly what the
		// server resolved, and the happy path writes nothing — the real render()
		// owns the root.
		expect(callerId).toBe("acc-x");
		expect(el.childNodes.length).toBe(0);
	});

	// The load-bearing failure contract this PR exists to hold: an unlearnable
	// "me" must paint the boot screen and STOP boot (return undefined, so main()
	// returns without rendering) rather than boot the app against no caller. Two
	// ways it fails — the RPC rejects, or the server answers with an empty id
	// resolveCaller rejects (live/client.ts) — must both stop here, through the
	// fake's own scaffolding.
	for (const [label, arm, detail] of [
		[
			"the WhoAmI RPC rejects",
			(f: FakeCompass) => f.failNextWhoAmI(new Error("boom-unavailable")),
			"boom-unavailable",
		],
		[
			"the server returns an empty account id",
			(f: FakeCompass) => {
				f.whoAmIAccountId.accountId = "";
			},
			"empty account id",
		],
	] as const) {
		test(`paints the boot-error screen and returns undefined when ${label}`, async () => {
			const el = document.createElement("div");
			const fake = createFakeCompass();
			arm(fake);

			const callerId = await bootCaller(el, () => resolveCaller(fake.client));

			// Undefined is the stop signal — main() returns without rendering.
			expect(callerId).toBeUndefined();
			// Visible, not blank, naming the caller-identity boundary, and
			// carrying the specific cause (the thrown message / the empty-id guard).
			expect(el.childNodes.length).toBeGreaterThan(0);
			expect(el.textContent).toContain("could not learn the caller identity");
			expect(el.textContent).toContain(detail);
		});
	}
});
