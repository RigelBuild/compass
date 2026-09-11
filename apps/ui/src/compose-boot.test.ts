import { describe, expect, test } from "bun:test";
import {
	createCommsClient,
	createCompassClient,
	createRouterTransport,
	type TraceIdSink,
	type Transport,
} from "@compass/client";
import type { Analytics } from "./analytics/analytics";
import { composeBoot } from "./compose-boot";
import type { LiveClients } from "./live/client";
import type { ResolvedConnection } from "./live/provider";

// composeBoot must build analytics BEFORE the clients: the analytics `traceId`
// getter is a forward reference to `clients`, safe only because it fires at
// capture time — after the clients bind. A wrong order still typechecks and
// renders; it only fails at the network door as a missing X-POSTHOG-SESSION-ID
// header, so the order is untestable in production. These fakes record the
// observable consequences of the order and the two lazy getters.

const connection: ResolvedConnection = {
	baseUrl: "https://compass.example:8443",
	token: "tok",
};

/** A fully-implemented Analytics whose `sessionId` returns a known value, so the
 *  clients' injected `sessionId` getter can be proven to resolve through to it. */
function fakeAnalytics(sessionId: string): Analytics {
	return {
		capture: () => {},
		identify: () => {},
		sessionId: () => sessionId,
		shutdown: () => {},
	};
}

/** A real (in-memory) LiveClients with a scripted `traceId` slot, so the
 *  analytics `traceId` getter can be proven to resolve through to the clients'
 *  sink. Built over a router transport — a real client, never an `as` cast. */
function fakeClients(traceIdCurrent: string): LiveClients {
	const transport: Transport = createRouterTransport(() => {});
	const traceId: TraceIdSink = { current: traceIdCurrent };
	return {
		comms: createCommsClient(transport),
		compass: createCompassClient(transport),
		transport,
		traceId,
	};
}

describe("composeBoot order + lazy correlation", () => {
	test("builds analytics before clients and wires both lazy getters", () => {
		let clientsBuilt = false;
		let clientsExistedWhenAnalyticsBuilt = true;
		let capturedTraceId: (() => string | undefined) | undefined;
		let capturedSessionId: (() => string | undefined) | undefined;

		const analytics = fakeAnalytics("session-xyz");
		const clients = fakeClients("trace-abc");

		const built = composeBoot({
			connection,
			createAnalytics: (_config, deps) => {
				// Observable order: at analytics construction the clients factory
				// must not have run yet. Inverting the two lines flips this true.
				clientsExistedWhenAnalyticsBuilt = clientsBuilt;
				capturedTraceId = deps?.traceId;
				return analytics;
			},
			createLiveClients: (_conn, deps) => {
				clientsBuilt = true;
				capturedSessionId = deps?.sessionId;
				return clients;
			},
		});

		// Order: clients did not exist when analytics was constructed.
		expect(clientsExistedWhenAnalyticsBuilt).toBe(false);

		// composeBoot returns exactly the two built objects.
		expect(built.analytics).toBe(analytics);
		expect(built.clients).toBe(clients);

		// Outbound getter: the clients received a sessionId getter that resolves
		// to the analytics session once both are built.
		expect(capturedSessionId?.()).toBe("session-xyz");

		// Inbound getter: the analytics received a traceId getter that resolves
		// through to the clients' trace slot — the forward reference, live.
		expect(capturedTraceId?.()).toBe("trace-abc");
	});
});
