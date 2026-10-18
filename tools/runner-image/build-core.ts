// Pure mappings turn realized store paths into staged paths and build arguments.

/** The fixed build epoch. Nix normalises every store mtime to 1, so using the
 * same value keeps the rewritten layer timestamps equal to what the staged tree
 * already carries. */
export const SOURCE_DATE_EPOCH = 1;

/** The six artifacts the image carries, as realised store paths. */
export interface RunnerImageOutputs {
	/** The `compass-runner` package out-path (the binary is at `bin/compass-runner`). */
	runner: string;
	/** The `compass-stack-env` symlinkJoin (cloud-hypervisor, virtiofsd, passt under `bin/`). */
	stack: string;
	/** The guest kernel DERIVATION dir; the bootable artifact is `<dir>/bzImage`. */
	kernelDir: string;
	/** The guest rootfs image — the derivation IS the file. */
	rootfs: string;
	/** The guest initramfs — the derivation IS the file. */
	initrd: string;
}

/** Split `nix build --print-out-paths` stdout into trimmed, non-empty store
 * paths, one per line. Mirrors the microvm-boot-test lane's parser: the same
 * command shape deserves the same reader, not a second convention. */
export function parseOutPaths(stdout: string): string[] {
	return stdout
		.split("\n")
		.map((line) => line.trim())
		.filter((line) => line !== "");
}

/**
 * The absolute in-image path of the bootable kernel. The kernel derivation is a
 * DIRECTORY (it also carries System.map); cloud-hypervisor direct-boots the
 * bzImage inside it, with no bootloader. The rootfs and initrd derivations are
 * files, so only this one gains a suffix — the asymmetry is the derivations',
 * not a convention.
 */
export function kernelImagePath(kernelDir: string): string {
	return `${kernelDir}/bzImage`;
}

/**
 * The `--opt build-arg:` values the Dockerfile bakes into ENV. Every value is an
 * absolute /nix/store path resolved at build time, because each carries a
 * content hash that moves whenever its input is rebuilt — hardcoding any of them
 * in the Dockerfile would pin a stale artifact that the staged closure no longer
 * contains.
 *
 * These land on the documented `COMPASS_MICROVM_*` env fallbacks for the
 * matching `--microvm-*` flags, so an operator can still override any one at
 * runtime without rebuilding.
 */
export function buildArgs(outputs: RunnerImageOutputs): Record<string, string> {
	return {
		VMM_BIN: `${outputs.stack}/bin/cloud-hypervisor`,
		VIRTIOFSD_BIN: `${outputs.stack}/bin/virtiofsd`,
		// passt is exec'd by NAME rather than by a configured path (it has no
		// --microvm-* flag), so the image needs its bin dir on PATH.
		STACK_BIN_DIR: `${outputs.stack}/bin`,
		GUEST_KERNEL: kernelImagePath(outputs.kernelDir),
		GUEST_ROOTFS: outputs.rootfs,
		GUEST_INITRD: outputs.initrd,
	};
}

/**
 * The closure ROOTS whose transitive dependencies must be staged into the build
 * context.
 *
 * The roots are the derivation out-paths, NOT the inner file paths: `nix
 * path-info -r` takes store paths, and the kernel's root is its directory even
 * though the image references the bzImage inside it.
 *
 * Staging the transitive closure — rather than just these five — is
 * load-bearing. Each carried binary names an absolute /nix/store interpreter and
 * resolves every NEEDED library through its own RPATH, so a context missing the
 * transitive glibc produces an image whose binaries fail to exec with "missing
 * dynamic library". That is the measured negative control behind the base-image
 * choice, not a hypothetical.
 */
export function closureRoots(outputs: RunnerImageOutputs): string[] {
	return [
		outputs.runner,
		outputs.stack,
		outputs.kernelDir,
		outputs.rootfs,
		outputs.initrd,
	];
}

/** The buildctl `--output` spec for each supported output mode. `oci` writes a
 * browsable local layout (what the publish lane scans BEFORE deciding to push);
 * `image` names a tagged image for a local dogfood load; `push` uploads to a
 * registry BY DIGEST ONLY, so `tag` is the bare repo and no tag is written. The
 * publish lane tags the digest only after it verifies it.
 *
 * `push` is an EXPORTER, not a separate command — buildctl has no `push` verb,
 * so publishing re-runs this build with a different output. That is why the
 * rewrite-timestamp below must be identical across modes: a push whose layers
 * were stamped differently would publish a digest no local build reproduces. */
export function outputSpec(
	mode: "oci" | "image" | "push",
	tag: string,
	ociDir: string,
): string {
	// Normalize layer mtimes so identical inputs produce the same manifest digest.
	const rewrite = "rewrite-timestamp=true";
	if (mode === "oci") return `type=oci,dest=${ociDir},tar=false,${rewrite}`;
	// Pin OCI media types so registry and local layout digests match.
	if (mode === "push") {
		return `type=image,name=${tag},push=true,push-by-digest=true,oci-mediatypes=true,${rewrite}`;
	}
	return `type=image,name=${tag},${rewrite}`;
}

/**
 * The full `buildctl` argv (after the binary). Rootless BuildKit is the ruled
 * mechanism for a prebuilt-application image; this lane never shells out to
 * `docker build` and never mounts a host docker socket, which would hand the
 * build the daemon's blast radius.
 */
export function buildctlArgs(
	contextDir: string,
	outputs: RunnerImageOutputs,
	platform: string,
	output: string,
): string[] {
	const args = [
		"build",
		"--frontend",
		"dockerfile.v0",
		"--local",
		`context=${contextDir}`,
		"--local",
		`dockerfile=${contextDir}`,
		"--opt",
		"filename=Dockerfile",
		"--opt",
		`platform=${platform}`,
		// Pairs with rewrite-timestamp: this fixes the image config's `created`
		// field and is the epoch every layer mtime is rewritten to. 1, not 0,
		// matching the mtime nix normalises its store paths to.
		"--opt",
		`build-arg:SOURCE_DATE_EPOCH=${SOURCE_DATE_EPOCH}`,
	];
	// Sorted, so the argv is deterministic across runs: an unstable arg order
	// would make two otherwise-identical builds diff in logs for no reason.
	for (const [name, value] of Object.entries(buildArgs(outputs)).sort(
		([a], [b]) => a.localeCompare(b),
	)) {
		args.push("--opt", `build-arg:${name}=${value}`);
	}
	args.push("--output", output);
	return args;
}
