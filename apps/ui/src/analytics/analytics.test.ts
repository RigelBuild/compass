import { describe, expect, test } from "bun:test";
import type { PostHog } from "posthog-js";
import { createAnalytics } from "./analytics";

// analytics.ts wraps posthog-js behind a small headless interface. These tests
// inject a FAKE posthog client (recording calls) so both paths are exercised
// without real network. The load-bearing contract: the disabled path (undefined
// config) makes ZERO posthog calls — the off-by-default gate — and the enabled
// path initializes once and delegates capture/identify through to the client.

interface FakePostHog {
	calls: Array<{ method: string; args: unknown[] }>;
	init: (...args: unknown[]) => void;
	capture: (...args: unknown[]) => void;
	identify: (...args: unknown[]) => void;
	reset: (...args: unknown[]) => void;
}

function makeFake(): FakePostHog {
	const calls: FakePostHog["calls"] = [];
	const record =
		(method: string) =>
		(...args: unknown[]) => {
			calls.push({ method, args });
		};
	return {
		calls,
		init: record("init"),
		capture: record("capture"),
		identify: record("identify"),
		reset: record("reset"),
	};
}

describe("createAnalytics", () => {
	test("disabled (undefined config) makes ZERO posthog calls", () => {
		const fake = makeFake();
		const analytics = createAnalytics(undefined, {
			posthog: fake as unknown as PostHog,
		});

		analytics.capture("evt", { a: 1 });
		analytics.identify("acct-1");
		analytics.shutdown();

		expect(fake.calls).toHaveLength(0);
	});

	test("disabled with NO deps (the production shape) is a callable no-op", () => {
		// index.tsx calls createAnalytics(analyticsConfigFromEnv()) with no deps,
		// so the disabled production path is createAnalytics(undefined) — the real
		// posthog default export must never be dereferenced. Pin that exact shape:
		// it returns without throwing and its methods are callable no-ops.
		const analytics = createAnalytics(undefined);
		expect(() => {
			analytics.capture("evt", { a: 1 });
			analytics.identify("acct-1");
			analytics.shutdown();
		}).not.toThrow();
	});

	test("enabled path initializes posthog once with headless-safe options", () => {
		const fake = makeFake();
		createAnalytics(
			{ key: "phc_abc", host: "https://eu.i.posthog.com" },
			{ posthog: fake as unknown as PostHog },
		);

		const inits = fake.calls.filter((c) => c.method === "init");
		expect(inits).toHaveLength(1);
		expect(inits[0]?.args[0]).toBe("phc_abc");
		const options = inits[0]?.args[1] as Record<string, unknown>;
		expect(options.api_host).toBe("https://eu.i.posthog.com");
		expect(options.autocapture).toBe(false);
		expect(options.capture_pageview).toBe(false);
		expect(options.disable_session_recording).toBe(true);
		// The two options that gate PostHog-RENDERED UI (design §T6: "NO
		// PostHog-rendered UI ships — no surveys widget"). A regression that
		// dropped either would silently ship a PostHog-rendered surface, so both
		// are asserted, not just session replay.
		expect(options.disable_surveys).toBe(true);
		expect(options.capture_pageleave).toBe(false);
	});

	test("enabled capture forwards event and props to the client", () => {
		const fake = makeFake();
		const analytics = createAnalytics(
			{ key: "phc_abc", host: "https://us.i.posthog.com" },
			{ posthog: fake as unknown as PostHog },
		);

		analytics.capture("evt", { a: 1 });

		const captures = fake.calls.filter((c) => c.method === "capture");
		expect(captures).toHaveLength(1);
		expect(captures[0]?.args).toEqual(["evt", { a: 1 }]);
	});

	test("enabled identify forwards the distinct id to the client", () => {
		const fake = makeFake();
		const analytics = createAnalytics(
			{ key: "phc_abc", host: "https://us.i.posthog.com" },
			{ posthog: fake as unknown as PostHog },
		);

		analytics.identify("acct-1");

		const identifies = fake.calls.filter((c) => c.method === "identify");
		expect(identifies).toHaveLength(1);
		expect(identifies[0]?.args).toEqual(["acct-1"]);
	});
});
