// Live client construction: a resolved Connection → the typed compass.v1 clients
// the store dials. The one place transport is chosen (compass-tauri-shell.md:
// 110-124) — the browser MVP builds gRPC-Web clients over the T3b network door
// through the shipped @compass/client factories; a hosted/Tauri transport is a
// sibling built here later without touching a caller above this seam.
//
// Two clients from one connection: the CommsClient (the channel surface — the
// T7 body-swap's data source) and the CompassClient (server liveness/version via
// GetServerInfo, the post-connect probe). Both carry the same bearer as a
// connect interceptor (the factories install it); no local-assumption leaks.

import type { CommsClient, CompassClient } from "@compass/client";
import { createCommsWebClient, createCompassWebClient } from "@compass/client";
import type { Connection } from "./connection";

/** The typed compass.v1 clients the app dials, built from one Connection. */
export interface LiveClients {
	/** The comms surface client — channels, messages, asks, the SubscribeComms
	 *  stream (the T7 data source). */
	readonly comms: CommsClient;
	/** The compass service client — server liveness/version (GetServerInfo). */
	readonly compass: CompassClient;
}

/** Build the live clients for a connection. Pure construction (no I/O): the
 *  gRPC-Web transports are created but no request is sent until a client method
 *  is called. */
export function createLiveClients(conn: Connection): LiveClients {
	return {
		comms: createCommsWebClient(conn.baseUrl, conn.token),
		compass: createCompassWebClient(conn.baseUrl, conn.token),
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
