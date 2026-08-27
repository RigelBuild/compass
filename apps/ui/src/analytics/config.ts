// The product-analytics embed config: whether the UI ships events to a PostHog
// project, and which host it dials. Read once at boot from the Vite env and
// handed to the analytics factory (index.tsx → createAnalytics). Unlike the live
// connection, analytics is OPTIONAL: the deployment default is OFF, so an
// unconfigured build emits ZERO analytics rather than failing to boot.
//
// PostHog here is a measurement/DATA SDK only — headless event capture, no
// PostHog-rendered UI (no surveys widget, no toolbar, no session-replay UI). A
// managed Compass points key+host at Rigel's PostHog; a self-host either leaves
// it off (the default) or points host at its own PostHog. The enable gate is the
// presence of a non-blank project key: no key ⇒ disabled, full stop.

/** The resolved analytics target: the PostHog project key and the ingestion
 *  host. Present only when analytics is enabled (a non-blank key was set); its
 *  absence upstream (`resolveAnalyticsConfig` → undefined) is the off state. */
export interface AnalyticsConfig {
	/** The PostHog project (public) API key. */
	readonly key: string;
	/** The PostHog ingestion host, e.g. "https://us.i.posthog.com". */
	readonly host: string;
}

/** The Vite env shape this module reads. Declared locally (not a global
 *  augmentation) so the keys the UI depends on are named in one place and a
 *  typo surfaces as a type error here rather than a silent undefined. */
interface AnalyticsEnv {
	readonly VITE_COMPASS_POSTHOG_KEY?: string;
	readonly VITE_COMPASS_POSTHOG_HOST?: string;
}

/** Resolve the analytics config from a Vite-style env record. Pure over its
 *  input so it is unit-testable without `import.meta` — `analyticsConfigFromEnv()`
 *  (below) passes the real `import.meta.env`.
 *
 *  The key is the enable gate: absent or all-whitespace → undefined (analytics
 *  disabled — the off-by-default posture), so an unconfigured deployment emits
 *  nothing. This deliberately does NOT throw the way the required baseUrl does
 *  (live/connection.ts): analytics is optional, so a missing key is a valid,
 *  quiet "off", not a misconfiguration. When a key is present, `host` is
 *  normalized: absent or all-whitespace → the standard PostHog US cloud host, so
 *  a managed deployment need only supply a key. */
export function resolveAnalyticsConfig(
	env: AnalyticsEnv,
): AnalyticsConfig | undefined {
	const key = env.VITE_COMPASS_POSTHOG_KEY?.trim();
	// No key ⇒ analytics off. The single, deliberate gate.
	if (!key) {
		return undefined;
	}
	const rawHost = env.VITE_COMPASS_POSTHOG_HOST?.trim();
	// Absent or whitespace-only host ⇒ the standard PostHog US cloud host, so an
	// enabled deployment only has to supply a key to reach managed PostHog.
	const host = rawHost ? rawHost : "https://us.i.posthog.com";
	return { key, host };
}

/** Resolve the analytics config from the running app's Vite env. The thin
 *  wrapper over `resolveAnalyticsConfig` that reads `import.meta.env`; kept
 *  separate so the pure resolver stays testable. */
export function analyticsConfigFromEnv(): AnalyticsConfig | undefined {
	return resolveAnalyticsConfig(import.meta.env as AnalyticsEnv);
}
