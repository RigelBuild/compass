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
 *  identify the caller; no UI. `capture`'s `props` is forwarded as-is — a later
 *  slice merges a correlation key (trace/session id) into it, so the parameter
 *  is present now but nothing correlation-related is wired here. */
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

	constructor(config: AnalyticsConfig, client: PostHog) {
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
		this.client.capture(event, props);
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
 *  assert calls without real network; it defaults to the real posthog-js client. */
export function createAnalytics(
	config: AnalyticsConfig | undefined,
	deps?: { posthog?: PostHog },
): Analytics {
	if (!config) {
		return new NoopAnalytics();
	}
	return new PostHogAnalytics(config, deps?.posthog ?? posthog);
}
