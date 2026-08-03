import { sanitizeUrl } from "@braintree/sanitize-url";

/** Link schemes safe to render as a live `href` and hand to the external
 *  opener. */
const SAFE_LINK_SCHEMES: ReadonlySet<string> = new Set([
	"http:",
	"https:",
	"mailto:",
]);

/**
 * The original `href` if it is safe to render as a live attribute and open
 * externally, else `null` (the caller renders the link inert).
 *
 * Two layers: `@braintree/sanitize-url` first decodes obfuscation tricks
 * (HTML-entity / control-char / whitespace-escaped / mixed-case `javascript:`,
 * `data:`, null-byte) that a bare `new URL().protocol` check misses, mapping
 * anything dangerous to `about:blank`. We then apply a fail-closed scheme
 * allow-list on the sanitized result — `sanitizeUrl` is deny-list (it lets
 * unknown custom schemes through), so the allow-list is what keeps an unknown
 * scheme out. On success we return the ORIGINAL href, not sanitize-url's
 * normalized form (which appends a trailing slash to a bare host), so the
 * rendered attribute is exactly what the source carried.
 */
export function safeHref(href: string | undefined): string | null {
	if (!href) return null;
	const sanitized = sanitizeUrl(href);
	try {
		if (!SAFE_LINK_SCHEMES.has(new URL(sanitized).protocol)) return null;
	} catch {
		return null;
	}
	// The scheme was validated on the SANITIZED form, which strips control
	// characters the browser leaves intact. Require the ORIGINAL to parse as an
	// absolute URL too, so an href that only becomes valid after sanitizing
	// (e.g. an embedded null byte) can never reach the rendered attribute.
	return URL.canParse(href) ? href : null;
}
