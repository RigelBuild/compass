// Pure helpers keep path parsing and env mapping testable without nix or processes.

import { kernelFileName, type Platform } from "../runner-image/build-core.ts";

/** Split `nix build --print-out-paths` stdout into trimmed, non-empty store
 * paths, one per line. */
export function parseOutPaths(stdout: string): string[] {
	return stdout
		.split("\n")
		.map((line) => line.trim())
		.filter((line) => line !== "");
}

/** Map the three guest-image out-paths (kernel, rootfs, initrd) to the env vars
 * microvmtest.Require reads. Throws if the build yields the wrong path count. */
export function guestImageEnv(
	outPaths: readonly string[],
	platform: Platform,
): Record<string, string> {
	if (outPaths.length !== 3) {
		throw new Error(
			`guestImageEnv expects 3 out-paths (kernel, rootfs, initrd), got ${outPaths.length}`,
		);
	}
	const [kernel, rootfs, initrd] = outPaths as [string, string, string];
	return {
		COMPASS_TEST_GUEST_KERNEL: `${kernel}/${kernelFileName(platform)}`,
		COMPASS_TEST_GUEST_ROOTFS: rootfs,
		COMPASS_TEST_GUEST_INITRD: initrd,
	};
}

/** Prepend each VMM out-path's `bin/` to an existing PATH, so the freshly
 * realised cloud-hypervisor/virtiofsd/passt win over any ambient copy — the same
 * ordering ci.yml:400-406 gives $GITHUB_PATH. */
export function prependBins(
	outPaths: readonly string[],
	currentPath: string,
): string {
	const bins = outPaths.map((p) => `${p}/bin`);
	return currentPath === "" ? bins.join(":") : [...bins, currentPath].join(":");
}
