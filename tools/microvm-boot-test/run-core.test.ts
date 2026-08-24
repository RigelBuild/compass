import { describe, expect, test } from "bun:test";
import { guestImageEnv, parseOutPaths, prependBins } from "./run-core.ts";

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

	test("empty stdout yields no paths (the fail-closed empty case run.ts rejects)", () => {
		expect(parseOutPaths("")).toEqual([]);
		expect(parseOutPaths("\n  \n")).toEqual([]);
	});
});

describe("guestImageEnv", () => {
	test("maps kernel/rootfs/initrd to the env vars Require reads, kernel gets /bzImage", () => {
		expect(
			guestImageEnv(["/nix/store/k", "/nix/store/r", "/nix/store/i"]),
		).toEqual({
			COMPASS_TEST_GUEST_KERNEL: "/nix/store/k/bzImage",
			COMPASS_TEST_GUEST_ROOTFS: "/nix/store/r",
			COMPASS_TEST_GUEST_INITRD: "/nix/store/i",
		});
	});

	test("throws on a wrong out-path count (build-drift is a named failure)", () => {
		expect(() => guestImageEnv(["/a", "/b"])).toThrow();
		expect(() => guestImageEnv(["/a", "/b", "/c", "/d"])).toThrow();
	});
});

describe("prependBins", () => {
	test("prepends each out-path's bin/ ahead of the existing PATH", () => {
		expect(
			prependBins(["/nix/store/ch", "/nix/store/vf"], "/usr/bin:/bin"),
		).toBe("/nix/store/ch/bin:/nix/store/vf/bin:/usr/bin:/bin");
	});

	test("empty current PATH yields just the bins", () => {
		expect(prependBins(["/nix/store/ch"], "")).toBe("/nix/store/ch/bin");
	});
});
