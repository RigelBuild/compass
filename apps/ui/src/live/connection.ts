// The live-daemon connection config: where the UI dials the Compass server and the bearer
// it presents. The door URL comes from the Vite env, else the page's own origin. The caller's
// account id is NOT part of this: it is learned via WhoAmI after connect. In browser mode the
// bearer may also come from the token screen (boot-browser.ts); this resolves only the env one.

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

/** Resolve the connection from a Vite-style env record and the page origin. Pure
 *  over its inputs so it is unit-testable without `import.meta` or `location`.
 *
 *  `baseUrl` is the env door URL, else `origin` (a bundle served by the door's
 *  own host). With neither there is nothing to dial, so this throws rather than
 *  guessing. `token` is optional and normalized: absent or all-whitespace →
 *  undefined, never a blank string the client factory would reject. */
export function resolveConnection(
	env: CompassEnv,
	origin?: string,
): Connection {
	const baseUrl = env.VITE_COMPASS_BASE_URL?.trim() || origin?.trim();
	if (!baseUrl) {
		throw new Error(
			"VITE_COMPASS_BASE_URL is required to reach the Compass server; " +
				"set it to the gRPC-Web door URL (e.g. https://host:8443).",
		);
	}
	const rawToken = env.VITE_COMPASS_TOKEN?.trim();
	// Absent or whitespace-only → a no-auth client (undefined), NOT a blank
	// bearer: the client factory treats "" as a misconfigured credential.
	const token = rawToken ? rawToken : undefined;
	return { baseUrl, token };
}

/** Resolve the connection from the running app's Vite env and page origin. */
export function connectionFromEnv(): Connection {
	// `null` is the origin of an opaque page (file:, sandboxed); it is not dialable.
	const origin = globalThis.location?.origin;
	return resolveConnection(
		import.meta.env as CompassEnv,
		origin && origin !== "null" ? origin : undefined,
	);
}
