import { describe, expect, test } from "bun:test";
import { safeHref } from "./safe-url";

describe("safeHref", () => {
	// Safe schemes return the ORIGINAL href verbatim (not sanitize-url's
	// normalized form — a bare host must NOT gain a trailing slash).
	test.each([
		"https://example.com",
		"https://example.com/safe",
		"http://localhost:5173/x",
		"mailto:hello@example.com",
	])("keeps a safe href verbatim: %s", (href) => {
		expect(safeHref(href)).toBe(href);
	});

	// Dangerous / obfuscated schemes → null (no navigable link).
	test.each([
		"javascript:alert(1)",
		"jAvAsCrIpT:alert(1)",
		"  javascript:alert(1)",
		"java\tscript:alert(1)",
		"data:text/html,<script>alert(1)</script>",
		"vbscript:msgbox(1)",
		"file:///etc/passwd",
	])("rejects a dangerous scheme: %s", (href) => {
		expect(safeHref(href)).toBeNull();
	});

	// Empty / undefined / malformed → null.
	test.each([undefined, "", "not a url", "://nope"])(
		"rejects empty/malformed: %s",
		(href) => {
			expect(safeHref(href as string | undefined)).toBeNull();
		},
	);
});
