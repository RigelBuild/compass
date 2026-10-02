// The browser-mode bearer, persisted in localStorage per door URL. Keyed by the door so a
// bearer pasted for one server is never presented to another. Best-effort: a missing or
// throwing store (privacy mode) means the token lives only for this page load.

// Origin-only, so a trailing slash or env-vs-origin switch for one door finds the same token.
function keyFor(baseUrl: string): string {
	try {
		return `compass.token.${new URL(baseUrl).origin}`;
	} catch {
		return `compass.token.${baseUrl}`;
	}
}

function storage(): Storage | undefined {
	try {
		return globalThis.localStorage;
	} catch {
		return undefined;
	}
}

/** The stored bearer for `baseUrl`, or undefined when none is stored. */
export function loadToken(baseUrl: string): string | undefined {
	try {
		return storage()?.getItem(keyFor(baseUrl))?.trim() || undefined;
	} catch {
		return undefined;
	}
}

/** Store the bearer for `baseUrl`. */
export function saveToken(baseUrl: string, token: string): void {
	try {
		storage()?.setItem(keyFor(baseUrl), token);
	} catch {
		// Quota or privacy lock: the token still works for this page load.
	}
}

/** Forget the bearer for `baseUrl` (the server rejected it). */
export function clearToken(baseUrl: string): void {
	try {
		storage()?.removeItem(keyFor(baseUrl));
	} catch {
		// Nothing stored can be removed; nothing to do.
	}
}
