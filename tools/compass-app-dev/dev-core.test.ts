import { describe, expect, test } from "bun:test";
import {
	assetsDir,
	buildEnv,
	buildTags,
	devBinPath,
	goBuildArgv,
	gtkBuildEnv,
	nixClosureAttrs,
	parseMode,
	parseOutPaths,
	runEnv,
	spawnOutcome,
} from "./dev-core.ts";

describe("buildTags", () => {
	test("darwin needs no build tag (system WebKit, bare go build)", () => {
		expect(buildTags("darwin")).toEqual([]);
	});

	test("linux needs the gtk4 tag", () => {
		expect(buildTags("linux")).toEqual(["gtk4"]);
	});

	test("throws on an unsupported platform (win32 names it)", () => {
		expect(() => buildTags("win32")).toThrow("win32");
	});
});

describe("goBuildArgv", () => {
	test("darwin: no -tags flag; -trimpath and the compass-app package", () => {
		expect(goBuildArgv("darwin", "/out/compass-app")).toEqual([
			"go",
			"-C",
			"go",
			"build",
			"-trimpath",
			"-o",
			"/out/compass-app",
			"./cmd/compass-app",
		]);
	});

	test("linux: injects -tags gtk4", () => {
		expect(goBuildArgv("linux", "/out/compass-app")).toEqual([
			"go",
			"-C",
			"go",
			"build",
			"-trimpath",
			"-tags",
			"gtk4",
			"-o",
			"/out/compass-app",
			"./cmd/compass-app",
		]);
	});

	test("propagates the unsupported-platform failure", () => {
		expect(() => goBuildArgv("win32", "/out/compass-app")).toThrow("win32");
	});
});

describe("nixClosureAttrs", () => {
	test("linux realizes the pkg-config closure + cc.out", () => {
		expect(nixClosureAttrs("linux")).toEqual({ pc: "pkgConfig", cc: "cc.out" });
	});

	test("darwin realizes nothing (system WebKit + platform clang)", () => {
		expect(nixClosureAttrs("darwin")).toBeNull();
	});

	test("throws on an unsupported platform", () => {
		expect(() => nixClosureAttrs("win32")).toThrow("win32");
	});
});

describe("gtkBuildEnv", () => {
	test("maps the realized store paths to PKG_CONFIG_PATH/CC/CXX (both .pc subdirs)", () => {
		expect(gtkBuildEnv("/nix/pc", "/nix/cc")).toEqual({
			PKG_CONFIG_PATH: "/nix/pc/lib/pkgconfig:/nix/pc/share/pkgconfig",
			CC: "/nix/cc/bin/cc",
			CXX: "/nix/cc/bin/c++",
		});
	});
});

describe("parseOutPaths", () => {
	test("splits, trims, and drops blank lines", () => {
		expect(parseOutPaths("/nix/store/a\n/nix/store/b\n")).toEqual([
			"/nix/store/a",
			"/nix/store/b",
		]);
		expect(parseOutPaths("  /nix/store/a  \n\n /nix/store/b \n")).toEqual([
			"/nix/store/a",
			"/nix/store/b",
		]);
	});

	test("empty stdout yields no paths (the single-path guard rejects it)", () => {
		expect(parseOutPaths("")).toEqual([]);
		expect(parseOutPaths("\n  \n")).toEqual([]);
	});
});

describe("devBinPath / assetsDir", () => {
	test("dev binary lands under the tool dir", () => {
		expect(devBinPath("/repo")).toBe("/repo/tools/compass-app-dev/compass-app");
	});

	test("assets dir is the built UI dist", () => {
		expect(assetsDir("/repo")).toBe("/repo/apps/ui/dist");
	});
});

describe("buildEnv", () => {
	test("turns cgo on without dropping the base env (no overlay)", () => {
		expect(buildEnv({ HOME: "/h", PATH: "/b" })).toEqual({
			HOME: "/h",
			PATH: "/b",
			CGO_ENABLED: "1",
		});
	});

	test("layers the gtk overlay over cgo + base", () => {
		expect(buildEnv({ HOME: "/h" }, { CC: "/nix/cc/bin/cc" })).toEqual({
			HOME: "/h",
			CGO_ENABLED: "1",
			CC: "/nix/cc/bin/cc",
		});
	});
});

describe("runEnv", () => {
	test("points the resolver at the built UI dist, preserving the base env", () => {
		expect(runEnv({ HOME: "/h" }, "/repo")).toEqual({
			HOME: "/h",
			COMPASS_ASSETS_DIR: "/repo/apps/ui/dist",
		});
	});
});

describe("parseMode", () => {
	test("accepts build and run", () => {
		expect(parseMode("build")).toBe("build");
		expect(parseMode("run")).toBe("run");
	});

	test("rejects anything else (index.ts turns null into a usage error)", () => {
		expect(parseMode("start")).toBeNull();
		expect(parseMode("")).toBeNull();
		expect(parseMode(undefined)).toBeNull();
	});
});

describe("spawnOutcome", () => {
	test("a spawn that could not run (ENOENT: error set, null status) → named error, exit 1", () => {
		expect(
			spawnOutcome(
				{ status: null, error: new Error("spawn go ENOENT") },
				"go build",
			),
		).toEqual({
			action: "error",
			code: 1,
			message: "go build could not run: spawn go ENOENT",
		});
	});

	test("a nonzero exit status propagates as exit with that code", () => {
		expect(spawnOutcome({ status: 2 }, "go build")).toEqual({
			action: "exit",
			code: 2,
		});
	});

	test("a null status with no error still fails closed as exit 1", () => {
		expect(spawnOutcome({ status: null }, "go build")).toEqual({
			action: "exit",
			code: 1,
		});
	});

	test("status 0 is ok (fall through)", () => {
		expect(spawnOutcome({ status: 0 }, "go build")).toEqual({ action: "ok" });
	});
});
