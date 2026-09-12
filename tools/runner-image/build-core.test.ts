// Unit tests for the runner-image build lane's pure core. Each case pins a
// behaviour the image's correctness depends on, and each would fail on a
// plausible bug — not on a restatement of the implementation.

import { describe, expect, test } from "bun:test";
import {
	buildArgs,
	buildctlArgs,
	closureRoots,
	kernelImagePath,
	outputSpec,
	parseOutPaths,
	type RunnerImageOutputs,
} from "./build-core.ts";

const outputs: RunnerImageOutputs = {
	runner: "/nix/store/aaa-compass-runner",
	stack: "/nix/store/bbb-compass-stack-env",
	kernelDir: "/nix/store/ccc-linux",
	rootfs: "/nix/store/ddd-rootfs.erofs",
	initrd: "/nix/store/eee-initrd",
};

describe("parseOutPaths", () => {
	test("drops the blank trailing line nix emits", () => {
		expect(parseOutPaths("/nix/store/a\n/nix/store/b\n")).toEqual([
			"/nix/store/a",
			"/nix/store/b",
		]);
	});

	test("yields nothing for empty stdout, so a silent build failure cannot read as one path", () => {
		expect(parseOutPaths("")).toEqual([]);
		expect(parseOutPaths("\n  \n")).toEqual([]);
	});
});

describe("kernelImagePath", () => {
	// The kernel derivation is a directory (bzImage + System.map); the rootfs and
	// initrd derivations ARE files. Booting the directory would fail at the VMM.
	test("points at the bzImage inside the kernel derivation directory", () => {
		expect(kernelImagePath("/nix/store/ccc-linux")).toBe(
			"/nix/store/ccc-linux/bzImage",
		);
	});
});

describe("buildArgs", () => {
	test("maps each artifact to the env the Runner reads, kernel suffixed and the file assets bare", () => {
		expect(buildArgs(outputs)).toEqual({
			RUNNER_BIN: "/nix/store/aaa-compass-runner/bin/compass-runner",
			VMM_BIN: "/nix/store/bbb-compass-stack-env/bin/cloud-hypervisor",
			VIRTIOFSD_BIN: "/nix/store/bbb-compass-stack-env/bin/virtiofsd",
			STACK_BIN_DIR: "/nix/store/bbb-compass-stack-env/bin",
			GUEST_KERNEL: "/nix/store/ccc-linux/bzImage",
			GUEST_ROOTFS: "/nix/store/ddd-rootfs.erofs",
			GUEST_INITRD: "/nix/store/eee-initrd",
		});
	});

	test("every value is absolute, since the image bakes them as env the Runner resolves without a cwd", () => {
		for (const value of Object.values(buildArgs(outputs))) {
			expect(value.startsWith("/nix/store/")).toBe(true);
		}
	});
});

describe("closureRoots", () => {
	// The roots are derivation out-paths: `nix path-info -r` takes store paths,
	// and passing the bzImage FILE instead of its directory would stage a partial
	// closure whose binaries fail to exec.
	test("uses the kernel DIRECTORY, not the bzImage inside it", () => {
		const roots = closureRoots(outputs);
		expect(roots).toContain("/nix/store/ccc-linux");
		expect(roots).not.toContain("/nix/store/ccc-linux/bzImage");
	});

	test("covers all five artifacts, so no carried binary is staged without its dependencies", () => {
		expect(closureRoots(outputs).sort()).toEqual(
			[
				outputs.runner,
				outputs.stack,
				outputs.kernelDir,
				outputs.rootfs,
				outputs.initrd,
			].sort(),
		);
	});
});

describe("outputSpec", () => {
	test("oci writes a browsable layout, which is what the publish lane scans before pushing", () => {
		expect(outputSpec("oci", "ignored:tag", "/tmp/out")).toBe(
			"type=oci,dest=/tmp/out,tar=false",
		);
	});

	test("image names the tag for a local load", () => {
		expect(outputSpec("image", "compass-runner:dev", "/tmp/out")).toBe(
			"type=image,name=compass-runner:dev",
		);
	});

	test("oci mode never names the tag, so a dev tag cannot leak into a layout build", () => {
		expect(outputSpec("oci", "compass-runner:dev", "/tmp/out")).not.toContain(
			"compass-runner:dev",
		);
	});
});

describe("buildctlArgs", () => {
	test("passes every build-arg the Dockerfile declares", () => {
		const args = buildctlArgs(
			"/repo/runner-image",
			outputs,
			"linux/amd64",
			"type=oci,dest=/repo/runner-image/out,tar=false",
		);
		const joined = args.join(" ");
		for (const name of Object.keys(buildArgs(outputs))) {
			expect(joined).toContain(`build-arg:${name}=`);
		}
	});

	test("build-arg order is deterministic, so two identical builds produce identical argv", () => {
		const once = buildctlArgs("/ctx", outputs, "linux/amd64", "type=oci");
		const twice = buildctlArgs("/ctx", outputs, "linux/amd64", "type=oci");
		expect(once).toEqual(twice);
		const names = once
			.filter((a) => a.startsWith("build-arg:"))
			.map((a) => a.slice("build-arg:".length).split("=")[0]);
		expect(names).toEqual([...names].sort());
	});

	test("uses the dockerfile.v0 frontend against the staged context", () => {
		const args = buildctlArgs("/ctx", outputs, "linux/amd64", "type=oci");
		expect(args.slice(0, 3)).toEqual(["build", "--frontend", "dockerfile.v0"]);
		expect(args).toContain("context=/ctx");
		expect(args).toContain("dockerfile=/ctx");
	});
});
