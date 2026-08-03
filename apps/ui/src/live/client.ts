// Live client construction: a resolved Connection → the typed compass.v1 clients
// the store dials. The one place transport is chosen (compass-tauri-shell.md:
// 110-124) — the browser MVP builds gRPC-Web clients over the T3b network door
// through the shipped @compass/client factories; a hosted/Tauri transport is a
// sibling built here later without touching a caller above this seam.
//
// Two clients from one connection: the CommsClient (the channel surface — the
// T7 body-swap's data source) and the CompassClient (server liveness/version via
// GetServerInfo, the post-connect probe, plus the agent-lifecycle RPCs the
// workspace drives — StopAgentSession). Both carry the same bearer as a
// connect interceptor (the factories install it); no local-assumption leaks.

import type { CommsClient, CompassClient, Transport } from "@compass/client";
import {
	createCommsClient,
	createCompassClient,
	createCompassWebTransport,
} from "@compass/client";
import type { Connection } from "./connection";

/** The typed compass.v1 clients the app dials, built from one Connection. */
export interface LiveClients {
	/** The comms surface client — channels, messages, asks, the SubscribeComms
	 *  stream (the T7 data source). */
	readonly comms: CommsClient;
	/** The compass service client — server liveness/version (GetServerInfo) and
	 *  the agent-lifecycle RPCs (StopAgentSession). */
	readonly compass: CompassClient;
	/** The one gRPC-Web transport both clients dial over — the same instance the
	 *  query layer keys and calls by. connect-query-core embeds a Transport
	 *  reference in every query key, so cache identity (queries, invalidation,
	 *  setQueryData) requires this single shared instance (query record §A2). */
	readonly transport: Transport;
}

/** Build the live clients for a connection. Pure construction (no I/O): the
 *  gRPC-Web transport is created once here and shared by both clients, but no
 *  request is sent until a client method is called. The one place transport is
 *  chosen (see file header) — one instance, so the query layer's keys stay
 *  cache-coherent with the clients' calls. */
export function createLiveClients(conn: Connection): LiveClients {
	const transport = createCompassWebTransport(conn.baseUrl, conn.token);
	return {
		comms: createCommsClient(transport),
		compass: createCompassClient(transport),
		transport,
	};
}

/** The server's liveness/version, from the post-connect GetServerInfo probe. */
export interface ServerInfo {
	readonly version: string;
	readonly apiVersion: string;
}

/** Probe the server for liveness + version — the first round-trip the UI makes
 *  after connecting (compass.proto GetServerInfo, liveness/version only). Boot
 *  calls this to confirm the door answers and the api_version matches before
 *  opening the comms stream; it does NOT supply baseUrl/token (there is no
 *  provisioning handshake — those come from the boot config). */
export async function probeServer(client: CompassClient): Promise<ServerInfo> {
	const resp = await client.getServerInfo({});
	return { version: resp.version, apiVersion: resp.apiVersion };
}
