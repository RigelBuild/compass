import { describe, expect, test } from "bun:test";
import { findForbiddenEnv } from "./env-check.ts";

// A representative clean image env: the container user's HOME and USER (set by
// containers.nix AFTER the DEVENV_ filter) plus ordinary PATH/locale. None of
// these is devenv-internal, so a clean image must produce zero findings.
const CLEAN_ENV = [
	"PATH=/usr/bin:/bin",
	"HOME=/home/agent",
	"USER=agent",
	"LANG=C.UTF-8",
	"SSL_CERT_FILE=/etc/ssl/certs/ca-bundle.crt",
];

describe("findForbiddenEnv", () => {
	test("clean image env has no findings", () => {
		expect(findForbiddenEnv(CLEAN_ENV, { builderHome: "/home/mattw" })).toEqual(
			[],
		);
	});

	test("defect 1: builder home baked into DEVENV_ROOT is caught", () => {
		const env = [...CLEAN_ENV, "DEVENV_ROOT=/home/mattw/agents/ws/agent-image"];
		const found = findForbiddenEnv(env, { builderHome: "/home/mattw" });
		expect(found).toHaveLength(1);
		expect(found[0]?.key).toBe("DEVENV_ROOT");
		// Reported once, by its root cause (the leaked prefix), not twice — even
		// though the value ALSO embeds the builder home.
		expect(found[0]?.reason).toContain("imageEnv filter");
	});

	test("defect 2: DEVENV_ROOT=/env (a path the image lacks) is caught", () => {
		const found = findForbiddenEnv([...CLEAN_ENV, "DEVENV_ROOT=/env"]);
		expect(found).toHaveLength(1);
		expect(found[0]?.key).toBe("DEVENV_ROOT");
	});

	test("defect 3: a stale DEVENV_CONTAINER is caught", () => {
		const found = findForbiddenEnv([...CLEAN_ENV, "DEVENV_CONTAINER=agent"]);
		expect(found).toHaveLength(1);
		expect(found[0]?.key).toBe("DEVENV_CONTAINER");
	});

	test("every DEVENV_-prefixed key is flagged, each once", () => {
		const env = [
			...CLEAN_ENV,
			"DEVENV_ROOT=/env",
			"DEVENV_STATE=/env/.devenv/state",
			"DEVENV_PROFILE=/nix/store/xxx-devenv-profile",
		];
		const found = findForbiddenEnv(env);
		expect(found.map((f) => f.key).sort()).toEqual([
			"DEVENV_PROFILE",
			"DEVENV_ROOT",
			"DEVENV_STATE",
		]);
	});

	test("a bare builder-home leak in a non-DEVENV_ key is caught", () => {
		const env = [...CLEAN_ENV, "NIX_BUILD_TOP=/home/mattw/tmp/nix-build"];
		const found = findForbiddenEnv(env, { builderHome: "/home/mattw" });
		expect(found).toHaveLength(1);
		expect(found[0]?.key).toBe("NIX_BUILD_TOP");
		expect(found[0]?.reason).toContain("build-host home");
	});

	test("the container's own HOME is never a builder-home false positive", () => {
		// The container HOME (/home/agent) differs from the builder home
		// (/home/mattw), so it must not trip the path check.
		expect(
			findForbiddenEnv(["HOME=/home/agent"], { builderHome: "/home/mattw" }),
		).toEqual([]);
	});

	test("without builderHome, only DEVENV_ keys are enforced", () => {
		const env = ["NIX_BUILD_TOP=/home/mattw/tmp", "DEVENV_ROOT=/env"];
		const found = findForbiddenEnv(env);
		expect(found.map((f) => f.key)).toEqual(["DEVENV_ROOT"]);
	});

	test("an entry with no '=' is treated as a bare key", () => {
		expect(findForbiddenEnv(["DEVENV_BARE"])).toHaveLength(1);
		expect(findForbiddenEnv(["PLAINKEY"])).toEqual([]);
	});

	test("empty env is clean", () => {
		expect(findForbiddenEnv([])).toEqual([]);
	});
});
