import { describe, expect, test } from "bun:test";
import { isCarveOut, transform } from "./purge";

describe("transform — context-aware org-token rewrite", () => {
	test("GitHub compass slug → capital RigelBuild", () => {
		expect(transform("github.com/sealedsecurity/compass/edit/main/a.md")).toBe(
			"github.com/RigelBuild/compass/edit/main/a.md",
		);
	});

	test("bare compass slug → capital RigelBuild", () => {
		expect(transform('repo: "sealedsecurity/compass"')).toBe(
			'repo: "RigelBuild/compass"',
		);
	});

	test("go import path → capital RigelBuild", () => {
		expect(transform("github.com/sealedsecurity/compass/go/e2e")).toBe(
			"github.com/RigelBuild/compass/go/e2e",
		);
	});

	test("ghcr namespace → LOWERCASE rigelbuild (registry rule)", () => {
		expect(transform("ghcr.io/sealedsecurity/compass-agent:latest")).toBe(
			"ghcr.io/rigelbuild/compass-agent:latest",
		);
	});

	test("ghcr wins over the generic compass rule (ordering)", () => {
		// A ghcr compass-agent ref must NOT become capital RigelBuild.
		expect(
			transform("docker://ghcr.io/sealedsecurity/compass-agent:git-abc"),
		).not.toContain("RigelBuild");
	});

	test("email domain → rigel.build, local-part preserved", () => {
		expect(transform("security@sealedsecurity.com")).toBe(
			"security@rigel.build",
		);
		expect(transform("matt@sealedsecurity.com")).toBe("matt@rigel.build");
	});

	test("web subdomain host → rigel.build, subdomain preserved", () => {
		expect(transform("https://compass.sealedsecurity.com/assets/x.png")).toBe(
			"https://compass.rigel.build/assets/x.png",
		);
	});

	test("Linear workspace slug → lowercase rigelbuild", () => {
		expect(transform("https://linear.app/sealedsecurity/issue/SEA-1")).toBe(
			"https://linear.app/rigelbuild/issue/SEA-1",
		);
	});

	test("idempotent — a second pass is a no-op", () => {
		const once = transform(
			"github.com/sealedsecurity/compass and ghcr.io/sealedsecurity/x",
		);
		expect(transform(once)).toBe(once);
	});

	test("internal-monorepo demo URL → public compass repo", () => {
		expect(transform("https://github.com/sealedsecurity/sealed/pull/453")).toBe(
			"https://github.com/RigelBuild/compass/pull/453",
		);
		expect(
			transform("https://github.com/sealedsecurity/sealed/issues/1022"),
		).toBe("https://github.com/RigelBuild/compass/issues/1022");
	});

	test("quoted internal-monorepo repo-name literal → public compass repo", () => {
		expect(transform('const SEALED_REPO = "sealedsecurity/sealed";')).toContain(
			'"RigelBuild/compass"',
		);
	});

	test("colon-path internal-monorepo citation is LEFT for manual scrub (not a false swap)", () => {
		// This form cites a specific internal-monorepo file path that has no
		// compass equivalent; a blanket swap would fabricate a citation.
		const s = "sealedsecurity/sealed:apps/rigel.build/src/styles/tokens.css";
		expect(transform(s)).toBe(s);
	});

	test("English crypto 'sealed' is untouched", () => {
		const s = "the sealed payload verifies against SHA256SUMS";
		expect(transform(s)).toBe(s);
	});

	test("Warden concept is untouched", () => {
		const s = "Warden watches the action stream";
		expect(transform(s)).toBe(s);
	});
});

describe("isCarveOut", () => {
	test.each([
		["forks/devenv/src/modules/containers.nix", true],
		["forks/README.md", false], // first-party linted prose — scrubbed, not carved out
		["LICENSE-MIT", true],
		["tools/docs-migrate/migrate.test.ts", true],
		["tools/seal-purge/purge.ts", true],
		["apps/ui/src/stub-data.ts", false],
		["go/go.mod", false],
		["docs/designs/product/compass-0.6/design.md", false],
	])("%s → %p", (path, expected) => {
		expect(isCarveOut(path)).toBe(expected);
	});
});
