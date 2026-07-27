import { describe, expect, test } from "bun:test";
import { resolveConnection } from "./connection";

// connection.ts resolves the live-daemon connection (door URL + optional bearer
// + caller account id) from a Vite-style env record. `resolveConnection` is pure
// over its input, so these tests exercise it directly with plain env objects — no
// `import.meta`. They defend the load-bearing rules the client factory and the
// membership derivation downstream trust: baseUrl is required (a missing/blank
// one is a misconfiguration that throws, not a silent wrong default), callerId is
// required (it scopes every listing and drives rail membership — a missing/blank
// one throws rather than deriving "me" wrong), and token is normalized to
// undefined when absent/blank (never the empty string the factory rejects).

// A valid caller id every non-callerId case supplies so it isolates the field
// under test; the callerId-required block below is what defends the field itself.
const CALLER = "acc-me";

describe("resolveConnection", () => {
	test("resolves baseUrl + callerId and leaves token undefined when no token is set", () => {
		expect(
			resolveConnection({
				VITE_COMPASS_BASE_URL: "https://h:8443",
				VITE_COMPASS_CALLER_ID: CALLER,
			}),
		).toEqual({
			baseUrl: "https://h:8443",
			token: undefined,
			callerId: CALLER,
		});
	});

	test("trims surrounding whitespace from baseUrl", () => {
		expect(
			resolveConnection({
				VITE_COMPASS_BASE_URL: "  https://h:8443\n",
				VITE_COMPASS_CALLER_ID: CALLER,
			}).baseUrl,
		).toBe("https://h:8443");
	});

	test("trims surrounding whitespace from callerId", () => {
		expect(
			resolveConnection({
				VITE_COMPASS_BASE_URL: "https://h:8443",
				VITE_COMPASS_CALLER_ID: "  acc-me\t",
			}).callerId,
		).toBe("acc-me");
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
					VITE_COMPASS_CALLER_ID: CALLER,
				}),
			).toThrow(/VITE_COMPASS_BASE_URL/);
		});
	}

	for (const [label, value] of [
		["missing", undefined],
		["empty", ""],
		["whitespace-only", "   \t\n"],
	] as const) {
		test(`throws (mentioning VITE_COMPASS_CALLER_ID) when callerId is ${label}`, () => {
			expect(() =>
				resolveConnection({
					VITE_COMPASS_BASE_URL: "https://h:8443",
					VITE_COMPASS_CALLER_ID: value,
				}),
			).toThrow(/VITE_COMPASS_CALLER_ID/);
		});
	}

	test("trims a present, non-empty token onto the connection", () => {
		expect(
			resolveConnection({
				VITE_COMPASS_BASE_URL: "https://h:8443",
				VITE_COMPASS_CALLER_ID: CALLER,
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
				VITE_COMPASS_CALLER_ID: CALLER,
				VITE_COMPASS_TOKEN: value,
			});
			expect(r.token).toBeUndefined();
			expect(r.token).not.toBe("");
		});
	}
});
