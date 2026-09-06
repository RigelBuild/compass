// The product-analytics embed: a headless PostHog client wrapped behind a small
// interface so the rest of the app never touches posthog-js directly. Built once
// at boot (index.tsx → main) from the resolved AnalyticsConfig. PostHog is a
// measurement/DATA SDK here only — event capture and (later) flag/early-access
// JSON our own Solid components render. NO PostHog-rendered UI ships: autocapture,
// pageview capture, and session replay are all turned OFF at init.
//
// The config gate lives upstream (analytics/config.ts): when analytics is
// disabled (no project key) the config is `undefined` and `createAnalytics`
// returns a no-op that never CALLS posthog — flag-off means zero PostHog calls
// and zero network. (The posthog-js module is statically imported and bundled;
// constructing its singleton neither initializes it nor dials the network — only
// the enabled PostHogAnalytics path calls .init(), so nothing leaves the
// deployment when the flag is off.) When enabled, the posthog-backed impl
// delegates to the injected client. posthog is injectable via `deps` so tests
// exercise both paths against a fake without real network.

import posthog, { type PostHog } from "posthog-js";
import type { AnalyticsConfig } from "./config";

/** The app-facing analytics surface. Deliberately headless: capture events and
 *  identify the caller; no UI. `capture`'s `props` is the caller's own event
 *  properties; the implementation merges the correlation key into them, so no
 *  call site has to know about it. */
export interface Analytics {
	/** Record a product event with optional properties. */
	capture(event: string, props?: Record<string, unknown>): void;
	/** Associate subsequent events with a stable distinct id (the caller). */
	identify(distinctId: string): void;
	/** Tear down the identified session (logout / app teardown). */
	shutdown(): void;
}

/** The disabled implementation: every method is a no-op. Returned when analytics
 *  is off (no config), so the flag-off path makes ZERO posthog calls. */
class NoopAnalytics implements Analytics {
	capture(): void {}
	identify(): void {}
	shutdown(): void {}
}

/** The enabled implementation: initializes the injected posthog client with
 *  headless-safe defaults (no UI, no autocapture, no session replay) and
 *  delegates capture/identify/shutdown to it. */
class PostHogAnalytics implements Analytics {
	private readonly client: PostHog;
	/** The trace-id source, read at CAPTURE time rather than construction time:
	 *  the transport that records trace ids is built before this client exists,
	 *  so a value read once at construction would always be undefined.
	 *
	 *  A getter, not the sink object, on purpose — analytics reads one string and
	 *  has no business depending on compass-client's transport types, so the
	 *  layering stays one-directional. */
	private readonly traceId: () => string | undefined;

	constructor(
		config: AnalyticsConfig,
		client: PostHog,
		traceId: () => string | undefined,
	) {
		this.traceId = traceId;
		this.client = client;
		// Headless defaults: turn OFF everything that renders UI or captures
		// beyond explicit events — no autocapture, no automatic pageviews, no
		// session recording. This app renders any PostHog-driven surface itself.
		client.init(config.key, {
			api_host: config.host,
			autocapture: false,
			capture_pageview: false,
			capture_pageleave: false,
			disable_session_recording: true,
			disable_surveys: true,
		});
	}

	capture(event: string, props?: Record<string, unknown>): void {
		const traceId = this.traceId();
		if (traceId === undefined) {
			// No trace id ⇒ forward the caller's props untouched. Not even an
			// `$ai_trace_id: undefined` key: PostHog would ingest that as a real
			// property and it would show up as a null-valued column.
			this.client.capture(event, props);
			return;
		}
		// The caller's explicit `$ai_trace_id` WINS over the sink. The sink holds
		// the last reply's trace id, which is a good default but only a guess
		// about which call the event belongs to; a call site that passes one knows
		// the actual trace (e.g. it held the response), so overwriting it would
		// replace a fact with a heuristic.
		//
		// "Caller wins" means the caller supplied a VALUE, so the key is written
		// AFTER the spread rather than before it. A plain
		// `{ $ai_trace_id: traceId, ...props }` lets a caller who passed
		// `{ $ai_trace_id: someMaybeUndefinedVar }` spread an undefined-valued key
		// over the sink's good id — leaving the key present-and-undefined (the
		// null-column defect the no-trace-id branch above exists to avoid) while
		// also dropping correlation that was available.
		const callerTraceId = props?.$ai_trace_id;
		this.client.capture(event, {
			...props,
			$ai_trace_id: callerTraceId === undefined ? traceId : callerTraceId,
		});
	}

	identify(distinctId: string): void {
		this.client.identify(distinctId);
	}

	shutdown(): void {
		// PostHog's de-identify: reset the distinct id and start a fresh
		// anonymous session. The browser SDK batch-sends on its own; there is no
		// separate flush call, so reset() is the teardown seam.
		this.client.reset();
	}
}

/** Build the analytics client. When `config` is undefined (analytics disabled)
 *  returns a no-op that never touches posthog. When present, returns the
 *  posthog-backed impl. `deps.posthog` is injectable so tests supply a fake and
 *  assert calls without real network; it defaults to the real posthog-js client.
 *
 *  `deps.traceId` is the correlation source every captured event is stamped
 *  from. It joins the existing collaborator bag rather than becoming a third
 *  positional so the boot call site names it (`{ traceId: … }`) instead of
 *  passing `undefined` for deps, and so every existing caller keeps working
 *  unchanged. Omitted, nothing is stamped. */
export function createAnalytics(
	config: AnalyticsConfig | undefined,
	deps?: { posthog?: PostHog; traceId?: () => string | undefined },
): Analytics {
	if (!config) {
		return new NoopAnalytics();
	}
	return new PostHogAnalytics(
		config,
		deps?.posthog ?? posthog,
		deps?.traceId ?? (() => undefined),
	);
}
