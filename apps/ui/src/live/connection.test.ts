import { describe, expect, test } from "bun:test";
import { resolveConnection } from "./connection";

// Require a base URL and normalize blank tokens to undefined.

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
		test(`throws (mentioning VITE_COMPASS_BASE_URL) when baseUrl is ${label} and there is no origin`, () => {
			expect(() =>
				resolveConnection({
					VITE_COMPASS_BASE_URL: value,
				}),
			).toThrow(/VITE_COMPASS_BASE_URL/);
		});

		test(`falls back to the page origin when baseUrl is ${label}`, () => {
			expect(
				resolveConnection(
					{ VITE_COMPASS_BASE_URL: value },
					"https://mattfw.example",
				).baseUrl,
			).toBe("https://mattfw.example");
		});
	}

	test("a configured baseUrl wins over the page origin", () => {
		expect(
			resolveConnection(
				{ VITE_COMPASS_BASE_URL: "https://h:8443" },
				"https://mattfw.example",
			).baseUrl,
		).toBe("https://h:8443");
	});

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
