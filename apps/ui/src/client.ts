// The daemon client for the Compass UI. The UI reaches the daemon only through
// @compass/client — the generated, owned-door client for the compass.v1
// contract (compass.md §7.2). This module binds that client to a gRPC-Web
// transport pointed at the daemon's local endpoint.
import { type CompassClient, createCompassClient } from "@compass/client";
import { createGrpcWebTransport } from "@connectrpc/connect-web";

/** Build a typed compass.v1 client over gRPC-Web at `baseUrl`. */
export function createDaemonClient(baseUrl: string): CompassClient {
	return createCompassClient(createGrpcWebTransport({ baseUrl }));
}
