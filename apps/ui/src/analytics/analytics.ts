// The product-analytics embed: a headless PostHog client wrapped behind a small interface
// so the rest of the app never touches posthog-js directly. A measurement/DATA SDK only —
// NO PostHog-rendered UI ships (autocapture, pageview, replay OFF). Disabled (no project
// key) → a no-op that never calls posthog (zero network); posthog is injectable for tests.

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
	/** Return the current PostHog session id, when one exists. */
	sessionId(): string | undefined;
	/** Tear down the identified session (logout / app teardown). */
	shutdown(): void;
}

/** The disabled implementation: every method is a no-op. Returned when analytics
 *  is off (no config), so the flag-off path makes ZERO posthog calls. */
class NoopAnalytics implements Analytics {
	capture(): void {}
	identify(): void {}
	sessionId(): string | undefined {
		return undefined;
	}
	shutdown(): void {}
}

/** The enabled implementation: initializes the injected posthog client with
 *  headless-safe defaults (no UI, no autocapture, no session replay) and
 *  delegates capture/identify/shutdown to it. */
class PostHogAnalytics implements Analytics {
	private readonly client: PostHog;
	/** The trace-id source, read at CAPTURE time rather than construction time:
	 *  boot builds analytics BEFORE the transport, so this getter closes over a
	 *  `clients` binding that is not yet initialized — reading it at construction
	 *  would throw a ReferenceError, while reading it at capture time is long
	 *  after boot bound it.
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
		// The caller's explicit `$ai_trace_id` WINS over the sink (the sink's last-reply id
		// is only a guess; a caller that passes one holds the actual trace). "Caller wins"
		// means a VALUE, so the key is written AFTER the spread — a plain `{id, ...props}`
		// would let a caller's undefined-valued key overwrite the sink's good id.
		const callerTraceId = props?.$ai_trace_id;
		this.client.capture(event, {
			...props,
			$ai_trace_id: callerTraceId === undefined ? traceId : callerTraceId,
		});
	}

	identify(distinctId: string): void {
		this.client.identify(distinctId);
	}

	sessionId(): string | undefined {
		const sessionId = this.client.get_session_id();
		return sessionId === "" ? undefined : sessionId;
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
