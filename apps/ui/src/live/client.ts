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
import type { ResolvedConnection } from "./provider";

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

/** Build the live clients for a resolved connection. Pure construction (no I/O):
 *  the gRPC-Web transport is created once here and shared by both clients, but no
 *  request is sent until a client method is called. The one place transport is
 *  chosen (see file header) — one instance, so the query layer's keys stay
 *  cache-coherent with the clients' calls. `conn.fetchImpl` threads the resolved
 *  transport fetch through: undefined (browser dev) uses the platform fetch; a
 *  shell-provided fetch tunnels over IPC — the seam is invisible above here. */
export function createLiveClients(conn: ResolvedConnection): LiveClients {
	const transport = createCompassWebTransport(conn.baseUrl, conn.token, {
		fetch: conn.fetchImpl,
	});
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

/** The caller's own account id, resolved server-side from the connection's
 *  credential (compass.proto WhoAmI). Called right after the transport is up,
 *  the same post-connect round-trip family as probeServer; the returned id
 *  scopes every listing and drives rail membership, so boot cannot proceed
 *  without it — an empty/blank id the server didn't resolve is rejected, not
 *  returned, so an unknown "me" never silently scopes the store to no caller. */
export async function resolveCaller(client: CompassClient): Promise<string> {
	const resp = await client.whoAmI({});
	const accountId = resp.accountId?.trim();
	if (!accountId) {
		throw new Error(
			"WhoAmI returned an empty account id: the server did not resolve a " +
				"caller for this connection's credential.",
		);
	}
	return accountId;
}
