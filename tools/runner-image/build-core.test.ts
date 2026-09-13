// Unit tests for the runner-image build lane's pure core. Each case pins a
// behaviour the image's correctness depends on, and each would fail on a
// plausible bug — not on a restatement of the implementation.

import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
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
			VMM_BIN: "/nix/store/bbb-compass-stack-env/bin/cloud-hypervisor",
			VIRTIOFSD_BIN: "/nix/store/bbb-compass-stack-env/bin/virtiofsd",
			STACK_BIN_DIR: "/nix/store/bbb-compass-stack-env/bin",
			GUEST_KERNEL: "/nix/store/ccc-linux/bzImage",
			GUEST_ROOTFS: "/nix/store/ddd-rootfs.erofs",
			GUEST_INITRD: "/nix/store/eee-initrd",
		});
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
			"type=oci,dest=/tmp/out,tar=false,rewrite-timestamp=true",
		);
	});

	test("image names the tag for a local load", () => {
		expect(outputSpec("image", "compass-runner:dev", "/tmp/out")).toBe(
			"type=image,name=compass-runner:dev,rewrite-timestamp=true",
		);
	});

	test("push exports to the registry, since buildctl has no push verb", () => {
		expect(outputSpec("push", "ghcr.io/x/y:git-abc", "/tmp/out")).toBe(
			"type=image,name=ghcr.io/x/y:git-abc,push=true,oci-mediatypes=true,rewrite-timestamp=true",
		);
	});

	// Media-type strings live INSIDE the manifest, so a Docker-media-type push of
	// identical blobs hashes differently from the OCI layout. The publish lane
	// gates on those digests being equal, so a push that stopped requesting OCI
	// types would red every run after the bytes were already published.
	test("push requests OCI media types, so its digest can match the layout's", () => {
		expect(outputSpec("push", "ghcr.io/x/y:t", "/tmp/out")).toContain(
			"oci-mediatypes=true",
		);
	});

	test("only push uploads — a local mode must never reach a registry", () => {
		expect(outputSpec("image", "ghcr.io/x/y:t", "/tmp/out")).not.toContain(
			"push=true",
		);
		expect(outputSpec("oci", "ghcr.io/x/y:t", "/tmp/out")).not.toContain(
			"push=true",
		);
	});

	// The digest-stability property the publish lane depends on: without this,
	// two builds of a bit-identical staged tree still export different layer
	// digests, because the context's mtimes ride into the layer tar. The PUSH
	// mode matters most — a push stamped differently than the local build
	// publishes a digest no rebuild can reproduce.
	test("every mode rewrites layer timestamps, so a rebuild is digest-stable", () => {
		for (const mode of ["oci", "image", "push"] as const) {
			expect(outputSpec(mode, "t", "/tmp/out")).toContain(
				"rewrite-timestamp=true",
			);
		}
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
	});

	// The load-bearing drift test: buildArgs is only correct RELATIVE to the
	// Dockerfile's own ARG declarations, and nothing else compares the two. It is
	// what caught a RUNNER_BIN that build-core supplied and the Dockerfile had
	// stopped consuming.
	test("supplies exactly the build-args the Dockerfile declares", () => {
		const dockerfile = readFileSync(
			join(import.meta.dir, "..", "..", "runner-image", "Dockerfile"),
			"utf8",
		);
		const declared = new Set(
			[...dockerfile.matchAll(/^ARG\s+([A-Z_][A-Z0-9_]*)/gm)].map(
				(m) => m[1] as string,
			),
		);
		// SOURCE_DATE_EPOCH is consumed by the frontend itself and supplied by
		// buildctlArgs directly, not through the artifact mapping.
		declared.delete("SOURCE_DATE_EPOCH");
		expect(new Set(Object.keys(buildArgs(outputs)))).toEqual(declared);
	});

	test("uses the dockerfile.v0 frontend against the staged context", () => {
		const args = buildctlArgs("/ctx", outputs, "linux/amd64", "type=oci");
		expect(args.slice(0, 3)).toEqual(["build", "--frontend", "dockerfile.v0"]);
		expect(args).toContain("context=/ctx");
		expect(args).toContain("dockerfile=/ctx");
	});
});
