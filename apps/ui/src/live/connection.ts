// The live-daemon connection config: where the UI dials the Compass server and
// the bearer it presents. Read once at boot from the Vite env and handed to the
// store's client factory (index.tsx → createAppStore). The caller's own account
// id is NOT part of this config: it is learned from the server via the WhoAmI
// RPC right after the transport is up (live/client.ts resolveCaller), not read
// from env — so no operator ever supplies an account id.
//
// Transport is chosen at client construction (compass-tauri-shell.md:110-124):
// the browser MVP dials the T3b authenticated network door over gRPC-Web with a
// bearer token (the T3-door account token → `authorization: Bearer`). The
// hosted/handshake modes (GetServerInfo, a Wails custom-fetch to a UDS) are
// sibling transports layered at this same seam later — this module is the one
// place the baseUrl+token are resolved, so adding a mode never leaks a
// local-assumption above the transport boundary.

/** The resolved connection to the Compass server: the gRPC-Web door URL and the
 *  optional bearer. `token` undefined is a deliberate no-auth client (the dev
 *  door); an empty string is a misconfiguration the client factory rejects
 *  loudly rather than silently degrading (compass-client bearerInterceptors). */
export interface Connection {
	/** The gRPC-Web door base URL, e.g. "https://compass.example:8443". */
	readonly baseUrl: string;
	/** The T3-door account bearer, or undefined for a no-auth (dev) door. */
	readonly token?: string;
}

/** The Vite env shape this module reads. Declared locally (not a global
 *  augmentation) so the keys the UI depends on are named in one place and a
 *  typo surfaces as a type error here rather than a silent undefined. */
interface CompassEnv {
	readonly VITE_COMPASS_BASE_URL?: string;
	readonly VITE_COMPASS_TOKEN?: string;
}

/** Resolve the connection from a Vite-style env record. Pure over its input so
 *  it is unit-testable without `import.meta` — `connectionFromEnv()` (below)
 *  passes the real `import.meta.env`.
 *
 *  `baseUrl` is required: a live build with no door URL is a misconfiguration,
 *  so this throws rather than dialing a wrong default. The caller's account id
 *  is no longer resolved here — it is learned from the server via WhoAmI after
 *  connect (live/client.ts resolveCaller). `token` is optional and normalized:
 *  absent or all-whitespace → undefined (the no-auth dev door), never a blank
 *  string the client factory would reject as a bad credential. */
export function resolveConnection(env: CompassEnv): Connection {
	const baseUrl = env.VITE_COMPASS_BASE_URL?.trim();
	if (!baseUrl) {
		throw new Error(
			"VITE_COMPASS_BASE_URL is required to reach the Compass server; " +
				"set it to the gRPC-Web door URL (e.g. https://host:8443).",
		);
	}
	const rawToken = env.VITE_COMPASS_TOKEN?.trim();
	// Absent or whitespace-only → a no-auth client (undefined), NOT a blank
	// bearer: the client factory treats "" as a misconfigured credential and
	// fails loud, so normalize it away here at the single resolution point.
	const token = rawToken ? rawToken : undefined;
	return { baseUrl, token };
}

/** Resolve the connection from the running app's Vite env. The thin wrapper over
 *  `resolveConnection` that reads `import.meta.env`; kept separate so the pure
 *  resolver stays testable. */
export function connectionFromEnv(): Connection {
	return resolveConnection(import.meta.env as CompassEnv);
}
