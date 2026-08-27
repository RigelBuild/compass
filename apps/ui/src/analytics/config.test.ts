import { describe, expect, test } from "bun:test";
import { resolveAnalyticsConfig } from "./config";

// config.ts resolves the product-analytics embed config (PostHog key + host)
// from a Vite-style env record. `resolveAnalyticsConfig` is pure over its input,
// so these tests exercise it directly with plain env objects — no `import.meta`.
// They defend the load-bearing gate: analytics is OFF by default, so a missing or
// blank key resolves to undefined (never a throw — analytics is optional, unlike
// the required baseUrl), and an enabled build defaults the host to the standard
// PostHog US cloud when none is supplied.

describe("resolveAnalyticsConfig", () => {
	test("resolves key + given host when both are set", () => {
		expect(
			resolveAnalyticsConfig({
				VITE_COMPASS_POSTHOG_KEY: "phc_abc",
				VITE_COMPASS_POSTHOG_HOST: "https://eu.i.posthog.com",
			}),
		).toEqual({
			key: "phc_abc",
			host: "https://eu.i.posthog.com",
		});
	});

	test("trims surrounding whitespace from both key and host", () => {
		expect(
			resolveAnalyticsConfig({
				VITE_COMPASS_POSTHOG_KEY: "  phc_abc\n",
				VITE_COMPASS_POSTHOG_HOST: "  https://eu.i.posthog.com \t",
			}),
		).toEqual({
			key: "phc_abc",
			host: "https://eu.i.posthog.com",
		});
	});

	for (const [label, value] of [
		["absent", undefined],
		["whitespace-only", "   \t\n"],
	] as const) {
		test(`defaults host to the PostHog US cloud when host is ${label}`, () => {
			expect(
				resolveAnalyticsConfig({
					VITE_COMPASS_POSTHOG_KEY: "phc_abc",
					VITE_COMPASS_POSTHOG_HOST: value,
				})?.host,
			).toBe("https://us.i.posthog.com");
		});
	}

	for (const [label, value] of [
		["absent", undefined],
		["empty", ""],
		["whitespace-only", "   \t\n"],
	] as const) {
		test(`returns undefined (analytics off) when key is ${label}`, () => {
			expect(
				resolveAnalyticsConfig({
					VITE_COMPASS_POSTHOG_KEY: value,
					VITE_COMPASS_POSTHOG_HOST: "https://eu.i.posthog.com",
				}),
			).toBeUndefined();
		});
	}
});
