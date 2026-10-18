import { describe, expect, test } from "bun:test";
import { bootCaller, bootConnection, renderBootError } from "./boot";
import { resolveCaller } from "./live/client";
import { createFakeCompass, type FakeCompass } from "./live/compass-fake";
import { resolveConnection } from "./live/connection";

// Missing connection or caller identity must show an error and stop boot.

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

	// Compare rendered text with the resolver's own missing-value message.
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

		// A missing connection prevents caller boot.
		expect(connection).toBeUndefined();
		// Show the missing variable, not a blank root.
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

		// The returned id is server-derived; boot leaves rendering to its caller.
		expect(callerId).toBe("acc-x");
		expect(el.childNodes.length).toBe(0);
	});

	// A failed or empty WhoAmI result must show an error and prevent rendering.
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

			// Undefined stops boot; the visible error includes its cause.
			expect(callerId).toBeUndefined();
			expect(el.childNodes.length).toBeGreaterThan(0);
			expect(el.textContent).toContain("could not learn the caller identity");
			expect(el.textContent).toContain(detail);
		});
	}
});
