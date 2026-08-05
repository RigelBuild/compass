import { describe, expect, test } from "bun:test";
import { resolveConnection } from "./connection";

// connection.ts resolves the live-daemon connection (door URL + optional bearer)
// from a Vite-style env record. `resolveConnection` is pure over its input, so
// these tests exercise it directly with plain env objects — no `import.meta`.
// They defend the load-bearing rules the client factory downstream trusts:
// baseUrl is required (a missing/blank one is a misconfiguration that throws, not
// a silent wrong default), and token is normalized to undefined when absent/blank
// (never the empty string the factory rejects). The caller's account id is no
// longer resolved here — it is learned from the server via WhoAmI after connect
// (live/client.ts resolveCaller), so this module no longer reads or asserts it.

describe("resolveConnection", () => {
	test("resolves baseUrl and leaves token undefined when no token is set", () => {
		expect(
			resolveConnection({
				VITE_COMPASS_BASE_URL: "https://h:8443",
			}),
		).toEqual({
			baseUrl: "https://h:8443",
			token: undefined,
		});
	});

	test("trims surrounding whitespace from baseUrl", () => {
		expect(
			resolveConnection({
				VITE_COMPASS_BASE_URL: "  https://h:8443\n",
			}).baseUrl,
		).toBe("https://h:8443");
	});

	for (const [label, value] of [
		["missing", undefined],
		["empty", ""],
		["whitespace-only", "   \t\n"],
	] as const) {
		test(`throws (mentioning VITE_COMPASS_BASE_URL) when baseUrl is ${label}`, () => {
			expect(() =>
				resolveConnection({
					VITE_COMPASS_BASE_URL: value,
				}),
			).toThrow(/VITE_COMPASS_BASE_URL/);
		});
	}

	test("trims a present, non-empty token onto the connection", () => {
		expect(
			resolveConnection({
				VITE_COMPASS_BASE_URL: "https://h:8443",
				VITE_COMPASS_TOKEN: "  secret-token  ",
			}).token,
		).toBe("secret-token");
	});

	for (const [label, value] of [
		["absent", undefined],
		["empty", ""],
		["whitespace-only", "   \t"],
	] as const) {
		test(`normalizes ${label} token to undefined (not "")`, () => {
			const r = resolveConnection({
				VITE_COMPASS_BASE_URL: "https://h:8443",
				VITE_COMPASS_TOKEN: value,
			});
			expect(r.token).toBeUndefined();
			expect(r.token).not.toBe("");
		});
	}
});
