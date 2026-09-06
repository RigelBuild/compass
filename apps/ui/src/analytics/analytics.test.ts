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

describe("$ai_trace_id stamping", () => {
	const traceId = "4bf92f3577b34da6a3ce929d0e0e4736";
	const config = { key: "phc_abc", host: "https://us.i.posthog.com" };

	// The props of the single capture call, as the fake saw them. Narrowed rather
	// than cast: a capture that forwarded a non-object (or forwarded nothing)
	// must fail here, not silently read back as an empty prop bag.
	function capturedProps(fake: FakePostHog): Record<string, unknown> {
		const captures = fake.calls.filter((c) => c.method === "capture");
		expect(captures).toHaveLength(1);
		const props = captures[0]?.args[1];
		if (typeof props !== "object" || props === null) {
			throw new Error(`capture forwarded non-object props: ${String(props)}`);
		}
		return { ...props };
	}

	test("a trace id is stamped onto captured props", () => {
		const fake = makeFake();
		const analytics = createAnalytics(config, {
			posthog: fake as unknown as PostHog,
			traceId: () => traceId,
		});

		analytics.capture("evt", { a: 1 });

		expect(capturedProps(fake)).toEqual({ a: 1, $ai_trace_id: traceId });
	});

	test("no trace id ⇒ props forwarded byte-identically, with NO $ai_trace_id key", () => {
		const fake = makeFake();
		const analytics = createAnalytics(config, {
			posthog: fake as unknown as PostHog,
			traceId: () => undefined,
		});

		analytics.capture("evt", { a: 1 });

		const props = capturedProps(fake);
		// The key must be ABSENT, not present-and-undefined: PostHog ingests an
		// undefined-valued property as a real null column.
		expect("$ai_trace_id" in props).toBe(false);
		expect(props).toEqual({ a: 1 });
	});

	test("no trace id and no props ⇒ the client is called with props undefined", () => {
		const fake = makeFake();
		const analytics = createAnalytics(config, {
			posthog: fake as unknown as PostHog,
			traceId: () => undefined,
		});

		analytics.capture("evt");

		const captures = fake.calls.filter((c) => c.method === "capture");
		expect(captures[0]?.args).toEqual(["evt", undefined]);
	});

	test("a caller-supplied $ai_trace_id WINS over the sink's value", () => {
		// The call site knows the actual trace; the sink only holds the last
		// reply's. Overwriting the caller would replace a fact with a heuristic.
		const fake = makeFake();
		const analytics = createAnalytics(config, {
			posthog: fake as unknown as PostHog,
			traceId: () => traceId,
		});

		analytics.capture("evt", { $ai_trace_id: "caller-owned" });

		expect(capturedProps(fake).$ai_trace_id).toBe("caller-owned");
	});

	test("a caller-supplied $ai_trace_id of UNDEFINED does not beat the sink", () => {
		// `{ $ai_trace_id: someMaybeUndefinedVar }` is trivial to write, and its
		// key is PRESENT with an undefined value. Caller-wins must mean the caller
		// supplied a VALUE: an undefined one would otherwise land a null-valued
		// column in PostHog AND throw away correlation that was available.
		const fake = makeFake();
		const analytics = createAnalytics(config, {
			posthog: fake as unknown as PostHog,
			traceId: () => traceId,
		});

		analytics.capture("evt", { a: 1, $ai_trace_id: undefined });

		const props = capturedProps(fake);
		expect(props.$ai_trace_id).toBe(traceId);
		expect(props).toEqual({ a: 1, $ai_trace_id: traceId });
	});

	test("a trace id arriving AFTER createAnalytics is picked up by a later capture", () => {
		// The whole reason the source is a getter over a mutable slot: boot builds
		// the transport before analytics, so the first trace id lands later. A
		// read-once-at-construction regression would capture undefined forever.
		const fake = makeFake();
		let current: string | undefined;
		const analytics = createAnalytics(config, {
			posthog: fake as unknown as PostHog,
			traceId: () => current,
		});

		analytics.capture("before", { a: 1 });
		current = traceId;
		analytics.capture("after", { a: 2 });

		const captures = fake.calls.filter((c) => c.method === "capture");
		expect(captures).toHaveLength(2);
		expect(captures[0]?.args).toEqual(["before", { a: 1 }]);
		expect(captures[1]?.args).toEqual([
			"after",
			{ a: 2, $ai_trace_id: traceId },
		]);
	});

	test("disabled config makes ZERO posthog calls even with a live trace id", () => {
		// The off-by-default gate outranks correlation: a trace id must not drag
		// the disabled path into calling posthog.
		const fake = makeFake();
		const analytics = createAnalytics(undefined, {
			posthog: fake as unknown as PostHog,
			traceId: () => traceId,
		});

		analytics.capture("evt", { a: 1 });
		analytics.identify("acct-1");
		analytics.shutdown();

		expect(fake.calls).toHaveLength(0);
	});
});
