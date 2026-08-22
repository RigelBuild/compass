import { describe, expect, test } from "bun:test";
import { findForbiddenEnv } from "./env-check.ts";

// A representative clean image env: the container user's HOME and USER (set by
// containers.nix), ordinary PATH/locale, and the devenv-internal DEVENV_ vars
// the fork forces to NON-store container paths during a build (home/tmp). None
// of these names a /nix/store path, so a clean image produces zero findings.
const CLEAN_ENV = [
	"PATH=/usr/bin:/bin",
	"HOME=/home/agent",
	"USER=agent",
	"LANG=C.UTF-8",
	"SSL_CERT_FILE=/etc/ssl/certs/ca-bundle.crt",
	// devenv-internal, but forced off store paths during the container build —
	// harmless container/ephemeral paths, no closure dragged.
	"DEVENV_ROOT=/home/agent",
	"DEVENV_STATE=/home/agent/.devenv/state",
	"DEVENV_RUNTIME=/tmp/devenv",
	"DEVENV_DOTFILE=/home/agent/.devenv",
	"DEVENV_TASKS=",
];

describe("findForbiddenEnv", () => {
	test("clean image env has no findings", () => {
		expect(findForbiddenEnv(CLEAN_ENV, { builderHome: "/home/mattw" })).toEqual(
			[],
		);
	});

	test("a DEVENV_ var naming a /nix/store path is caught (closure root)", () => {
		// DEVENV_PROFILE is the 266-path dev profile; naming it in the image env
		// drags its whole closure into content layers via config.json.
		const env = [...CLEAN_ENV, "DEVENV_PROFILE=/nix/store/xxx-devenv-profile"];
		const found = findForbiddenEnv(env);
		expect(found).toHaveLength(1);
		expect(found[0]?.key).toBe("DEVENV_PROFILE");
		expect(found[0]?.reason).toContain("/nix/store");
	});

	test("DEVENV_TASK_FILE naming a store path is caught (second closure root)", () => {
		const env = [...CLEAN_ENV, "DEVENV_TASK_FILE=/nix/store/yyy-tasks.json"];
		const found = findForbiddenEnv(env);
		expect(found).toHaveLength(1);
		expect(found[0]?.key).toBe("DEVENV_TASK_FILE");
	});

	test("both store-path leaks are flagged, each once", () => {
		const env = [
			...CLEAN_ENV,
			"DEVENV_PROFILE=/nix/store/xxx-devenv-profile",
			"DEVENV_TASK_FILE=/nix/store/yyy-tasks.json",
		];
		const found = findForbiddenEnv(env);
		expect(found.map((f) => f.key).sort()).toEqual([
			"DEVENV_PROFILE",
			"DEVENV_TASK_FILE",
		]);
	});

	test("a DEVENV_ var with a non-store container path is NOT flagged", () => {
		// The container-path DEVENV_ vars expand no closure — they must not trip
		// the gate, or the fork's minimal-module design (RIG-2404) can never pass.
		const found = findForbiddenEnv([
			"DEVENV_ROOT=/home/agent",
			"DEVENV_RUNTIME=/tmp/devenv",
			"DEVENV_TASKS=",
		]);
		expect(found).toEqual([]);
	});

	test("builder home baked into DEVENV_ROOT is caught (non-reproducible)", () => {
		// A store-path check would miss this (it's a /home path), but the builder
		// home leak is a separate invariant that catches it.
		const env = [...CLEAN_ENV, "DEVENV_ROOT=/home/mattw/agents/ws/agent-image"];
		const found = findForbiddenEnv(env, { builderHome: "/home/mattw" });
		expect(found).toHaveLength(1);
		expect(found[0]?.key).toBe("DEVENV_ROOT");
		expect(found[0]?.reason).toContain("build-host home");
	});

	test("a builder-home leak in a non-DEVENV_ key is caught", () => {
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

	test("a store-path DEVENV_ leak is reported once, not also as a builder leak", () => {
		// When a DEVENV_ store path ALSO happens to sit under the builder home,
		// it reports once by its root cause (the store-path closure root).
		const env = ["DEVENV_PROFILE=/nix/store/xxx-devenv-profile"];
		const found = findForbiddenEnv(env, { builderHome: "/nix/store" });
		expect(found).toHaveLength(1);
		expect(found[0]?.reason).toContain("/nix/store");
	});

	test("without builderHome, only store-path DEVENV_ keys are enforced", () => {
		const env = [
			"NIX_BUILD_TOP=/home/mattw/tmp",
			"DEVENV_ROOT=/home/agent",
			"DEVENV_PROFILE=/nix/store/xxx-devenv-profile",
		];
		const found = findForbiddenEnv(env);
		expect(found.map((f) => f.key)).toEqual(["DEVENV_PROFILE"]);
	});

	test("a DEVENV_ entry with no '=' is a bare key with an empty value (not a store path)", () => {
		expect(findForbiddenEnv(["DEVENV_BARE"])).toEqual([]);
		expect(findForbiddenEnv(["PLAINKEY"])).toEqual([]);
	});

	test("empty env is clean", () => {
		expect(findForbiddenEnv([])).toEqual([]);
	});
});
