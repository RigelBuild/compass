import { type Analytics, createAnalytics } from "./analytics/analytics";
import {
	type AnalyticsConfig,
	analyticsConfigFromEnv,
} from "./analytics/config";
import { createLiveClients, type LiveClients } from "./live/client";
import type { ResolvedConnection } from "./live/provider";

// The analytics+clients construction pair, lifted out of `index.tsx main()`
// behind injectable factories so the boot ORDER is testable. Production only
// fails a wrong order at the network door (a missing X-POSTHOG-SESSION-ID
// header), so recording fakes substituted here are the sole way to pin it — and
// this module is importable without the App/mount render graph that index.tsx
// drags in. The order and the lazy forward reference are load-bearing.
export interface ComposeBootDeps {
	connection: ResolvedConnection;
	createAnalytics?: typeof createAnalytics;
	createLiveClients?: typeof createLiveClients;
	analyticsConfig?: () => AnalyticsConfig | undefined;
}

export function composeBoot(deps: ComposeBootDeps): {
	analytics: Analytics;
	clients: LiveClients;
} {
	const buildAnalytics = deps.createAnalytics ?? createAnalytics;
	const buildClients = deps.createLiveClients ?? createLiveClients;
	const analyticsConfig = deps.analyticsConfig ?? analyticsConfigFromEnv;

	// Analytics FIRST: the `traceId` getter is a forward reference to `clients`,
	// safe only because it runs at capture time, long after the next line binds
	const analytics = buildAnalytics(analyticsConfig(), {
		traceId: () => clients.traceId.current,
	});
	const clients = buildClients(deps.connection, {
		sessionId: () => analytics.sessionId(),
	});
	return { analytics, clients };
}
