import { type Analytics, createAnalytics } from "./analytics/analytics";
import {
	type AnalyticsConfig,
	analyticsConfigFromEnv,
} from "./analytics/config";
import { createLiveClients, type LiveClients } from "./live/client";
import type { ResolvedConnection } from "./live/provider";

// A wrong boot order fails only at the network door (a missing
// X-POSTHOG-SESSION-ID header), so injectable factories are the sole way to
// pin it — and unlike index.tsx, this module imports without the render graph.
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
