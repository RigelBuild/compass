// The connection-provider seam: the one place boot mode differs. The UI above the transport
// boundary never knows browser-dev from native shell — it asks a ConnectionProvider to
// `resolve()`. The browser path wraps `connectionFromEnv()` (platform `fetch`); the native
// app supplies a shell Connection + a gRPC-Web-over-IPC `fetchImpl`, no Wails dep reaching here.

import { type Connection, connectionFromEnv } from "./connection";

/** A resolved connection plus the transport `fetch` to dial it with. `fetchImpl`
 *  undefined means the platform `fetch` (the browser dev path); a shell-provided
 *  provider sets it to the fetch that tunnels over its IPC. A superset of
 *  `Connection`, so a caller wanting only baseUrl+token still reads it cleanly. */
export type ResolvedConnection = Connection & { fetchImpl?: typeof fetch };

/** The boot seam: resolve the connection (and its transport fetch) for the
 *  running mode. `resolve()` is async because a shell provider may hand off the
 *  connection over an IPC round-trip; the env provider resolves synchronously
 *  under the hood but presents the same async contract so boot has one path. */
export interface ConnectionProvider {
	resolve(): Promise<ResolvedConnection>;
}

/** The default (browser dev) provider: wraps the Vite-env resolver unchanged and
 *  leaves `fetchImpl` undefined so the transport uses the platform `fetch`. The
 *  env-required-var throw is preserved — `connectionFromEnv()` still throws on a
 *  missing VITE_COMPASS_BASE_URL, and boot catches that at the same boundary it
 *  always has (bootConnection → the failure screen). Nothing native leaks here. */
export function envConnectionProvider(): ConnectionProvider {
	return {
		async resolve(): Promise<ResolvedConnection> {
			return { ...connectionFromEnv(), fetchImpl: undefined };
		},
	};
}
