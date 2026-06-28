// @compass/client — the generated TypeScript client for the compass.v1
// contract. The sole sanctioned way for UI code to reach the daemon; raw gRPC
// stub/socket access is fenced off by lint (the owned door).

import { type Client, createClient, type Transport } from "@connectrpc/connect";
import { createGrpcWebTransport } from "@connectrpc/connect-web";
import { CompassService } from "./gen/compass/v1/compass_pb";

/** A typed client for the Compass daemon over a given transport. */
export type CompassClient = Client<typeof CompassService>;

/** Create a typed compass.v1 client bound to `transport`. */
export function createCompassClient(transport: Transport): CompassClient {
	return createClient(CompassService, transport);
}

/**
 * Create a compass.v1 client over gRPC-Web at `baseUrl` — the door the web UI
 * uses. Bundles the transport so UI code imports only `@compass/client`.
 */
export function createCompassWebClient(baseUrl: string): CompassClient {
	return createCompassClient(createGrpcWebTransport({ baseUrl }));
}

export type {
	DaemonStatus,
	Event,
	GetDaemonInfoRequest,
	GetDaemonInfoResponse,
	ResyncRequired,
	SubscribeEventsRequest,
} from "./gen/compass/v1/compass_pb";
export { CompassService, DaemonState } from "./gen/compass/v1/compass_pb";
