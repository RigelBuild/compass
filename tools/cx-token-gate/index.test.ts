// Unit tests for the cx-token-gate's pure core + I/O wiring (index.ts).
//
// This gate is a CI oracle: it defines whether component/base CSS is accepted
// against the D2/D9 consumption rule, so this suite defends the machine-readable
// contract — the four banned categories, the mark allowlist's exact scope, and
// the WARN-vs-ERROR exit posture.
//
// Conventions (mirroring tools/design-ledger-gate/index.test.ts):
// - Literal paths, NOT values derived from the module constants (UI_SRC_DIR /
//   TOKENS_REL): those constants ARE the thing under test.
// - `.message` is human prose, asserted only by its identifying substring.

import { describe, expect, test } from "bun:test";
import {
	type Deps,
	type Finding,
	isMarkFile,
	resolveMode,
	runOnce,
	scanAll,
	scanCss,
} from "./index.ts";

const COMPONENT = "apps/ui/src/design/components/button.css";
const MARK = "apps/ui/src/design/components/mark.css";
const kinds = (fs: Finding[]) => fs.map((f) => f.kind);

// ---------------------------------------------------------------------------
// scanCss — the four banned categories.
// ---------------------------------------------------------------------------

describe("scanCss", () => {
	test("a clean file consuming only --cx-* tokens passes", () => {
		const css = `.btn {
	background: var(--cx-bg-raised);
	color: var(--cx-text);
	border-radius: var(--cx-radius-md);
	transition: color var(--cx-motion-fast) var(--cx-ease-out);
}`;
		expect(scanCss(COMPONENT, css)).toEqual([]);
	});

	test("a raw hex colour literal is flagged", () => {
		const fs = scanCss(COMPONENT, ".btn { color: #45505f; }");
		expect(kinds(fs)).toEqual(["hex"]);
		expect(fs[0]?.match).toBe("#45505f");
	});

	test("short hex (#rgb) is flagged", () => {
		const fs = scanCss(COMPONENT, ".btn { color: #abc; }");
		expect(kinds(fs)).toEqual(["hex"]);
	});

	test("a --rigel-* reference is flagged in a non-mark file", () => {
		const fs = scanCss(COMPONENT, ".btn { color: var(--rigel-blue); }");
		expect(kinds(fs)).toEqual(["rigel"]);
		expect(fs[0]?.match).toBe("--rigel-blue");
	});

	test("a literal duration (ms and s) is flagged", () => {
		const fs = scanCss(
			COMPONENT,
			".btn { transition: color 140ms; animation-duration: 1.6s; }",
		);
		expect(kinds(fs)).toEqual(["duration", "duration"]);
		expect(fs.map((f) => f.match)).toEqual(["140ms", "1.6s"]);
	});

	test("a cubic-bezier() literal is flagged as easing", () => {
		const fs = scanCss(
			COMPONENT,
			".btn { transition-timing-function: cubic-bezier(0.16, 1, 0.3, 1); }",
		);
		expect(kinds(fs)).toEqual(["easing"]);
	});

	test("a named easing keyword is flagged", () => {
		const fs = scanCss(COMPONENT, ".btn { transition: color 0.1 ease-out; }");
		expect(fs.some((f) => f.kind === "easing" && f.match === "ease-out")).toBe(
			true,
		);
	});

	test("linear-gradient does not trip the bare 'linear' easing keyword", () => {
		const fs = scanCss(
			COMPONENT,
			".btn { background: linear-gradient(var(--cx-bg), var(--cx-bg-raised)); }",
		);
		expect(kinds(fs)).toEqual([]);
	});
});

// ---------------------------------------------------------------------------
// The mark allowlist — path-scoped, name-scoped.
// ---------------------------------------------------------------------------

describe("mark allowlist", () => {
	test("isMarkFile matches mark*.css by basename only", () => {
		expect(isMarkFile("apps/ui/src/design/components/mark.css")).toBe(true);
		expect(isMarkFile("apps/ui/src/design/components/mark-lit.css")).toBe(true);
		expect(isMarkFile("apps/ui/src/design/components/button.css")).toBe(false);
	});

	test("--rigel-purple (and -hi/-lit) pass in the mark file", () => {
		const css = `.mark {
	color: var(--rigel-purple);
	--hi: var(--rigel-purple-hi);
	--lit: var(--rigel-purple-lit);
}`;
		expect(scanCss(MARK, css)).toEqual([]);
	});

	test("--rigel-purple is flagged in a NON-mark file", () => {
		const fs = scanCss(COMPONENT, ".x { color: var(--rigel-purple); }");
		expect(kinds(fs)).toEqual(["rigel"]);
	});

	test("a non-purple --rigel-* is still flagged inside the mark file", () => {
		const fs = scanCss(MARK, ".mark { color: var(--rigel-blue); }");
		expect(kinds(fs)).toEqual(["rigel"]);
		expect(fs[0]?.match).toBe("--rigel-blue");
	});
});

// ---------------------------------------------------------------------------
// scanAll — aggregation excludes tokens.css.
// ---------------------------------------------------------------------------

describe("scanAll", () => {
	test("tokens.css is never a subject even when passed", () => {
		const fs = scanAll([
			{
				path: "apps/ui/src/design/tokens.css",
				text: ":root { --rigel-blue: #82aaff; }",
			},
			{ path: COMPONENT, text: ".btn { color: var(--cx-text); }" },
		]);
		expect(fs).toEqual([]);
	});

	test("findings are sorted by file then line", () => {
		const fs = scanAll([
			{
				path: "apps/ui/src/design/components/z.css",
				text: ".z { color: #fff; }",
			},
			{
				path: "apps/ui/src/design/components/a.css",
				text: "\n.a { color: #000; }",
			},
		]);
		expect(fs.map((f) => f.file)).toEqual([
			"apps/ui/src/design/components/a.css",
			"apps/ui/src/design/components/z.css",
		]);
	});
});

// ---------------------------------------------------------------------------
// resolveMode — default WARN; flag + env overrides.
// ---------------------------------------------------------------------------

describe("resolveMode", () => {
	test("defaults to warn", () => {
		expect(resolveMode({}, [])).toBe("warn");
	});
	test("CX_GATE_MODE=error selects error", () => {
		expect(resolveMode({ CX_GATE_MODE: "error" }, [])).toBe("error");
	});
	test("--error flag wins over env", () => {
		expect(resolveMode({ CX_GATE_MODE: "warn" }, ["--error"])).toBe("error");
	});
	test("--warn flag wins over env", () => {
		expect(resolveMode({ CX_GATE_MODE: "error" }, ["--warn"])).toBe("warn");
	});
});

// ---------------------------------------------------------------------------
// runOnce — exit-code posture (WARN exits 0 with findings; ERROR exits 1).
// ---------------------------------------------------------------------------

const deps = (files: Record<string, string>, mode: "warn" | "error"): Deps => ({
	root: "/fake",
	mode,
	listCssFiles: async () => Object.keys(files),
	readText: async (_r, rel) => files[rel] ?? null,
	log: () => {},
	err: () => {},
});

describe("runOnce", () => {
	test("clean tree exits 0", async () => {
		const code = await runOnce(
			deps({ [COMPONENT]: ".btn { color: var(--cx-text); }" }, "warn"),
		);
		expect(code).toBe(0);
	});

	test("findings in WARN mode still exit 0 (non-blocking)", async () => {
		const code = await runOnce(
			deps({ [COMPONENT]: ".btn { color: #45505f; }" }, "warn"),
		);
		expect(code).toBe(0);
	});

	test("findings in ERROR mode exit 1", async () => {
		const code = await runOnce(
			deps({ [COMPONENT]: ".btn { color: #45505f; }" }, "error"),
		);
		expect(code).toBe(1);
	});

	test("tokens.css is excluded from the live scan", async () => {
		const code = await runOnce(
			deps(
				{ "apps/ui/src/design/tokens.css": ":root{--rigel-blue:#82aaff}" },
				"error",
			),
		);
		expect(code).toBe(0);
	});
});
