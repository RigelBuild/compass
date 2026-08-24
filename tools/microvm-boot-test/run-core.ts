// The pure core of the local microVM boot-test lane (RIG-2591): parse nix
// out-paths and map them to the env the tagged Go suite (microvmtest.Require)
// reads. No I/O — every function is a total map over its inputs — so run.test.ts
// can drive each mapping (and its fail-closed edges) without nix or a subprocess.

/** Split `nix build --print-out-paths` stdout into trimmed, non-empty store
 * paths, one per line. */
export function parseOutPaths(stdout: string): string[] {
	return stdout
		.split("\n")
		.map((line) => line.trim())
		.filter((line) => line !== "");
}

/** Map the three guest-image out-paths (in the order kernel, rootfs, initrd —
 * the attr order run.ts builds them in) to the env vars microvmtest.Require
 * reads. The kernel path gains `/bzImage`, mirroring ci.yml:392. Throws if not
 * given exactly three paths, so a build-count drift is a named failure. */
export function guestImageEnv(
	outPaths: readonly string[],
): Record<string, string> {
	if (outPaths.length !== 3) {
		throw new Error(
			`guestImageEnv expects 3 out-paths (kernel, rootfs, initrd), got ${outPaths.length}`,
		);
	}
	const [kernel, rootfs, initrd] = outPaths as [string, string, string];
	return {
		COMPASS_TEST_GUEST_KERNEL: `${kernel}/bzImage`,
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
