// The pure core of the runner-image build lane (R1). Every function here is a
// total map over its inputs with no I/O, so build-core.test.ts can drive each
// mapping — and each fail-closed edge — without nix, buildkit, or a subprocess.
//
// The lane's job is to turn realised nix store paths into the two things the
// container build needs: the set of paths to stage into the build context, and
// the build-args that bake those absolute paths into the image's env.

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
		RUNNER_BIN: `${outputs.runner}/bin/compass-runner`,
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
 * browsable local layout (what R2's publish lane scans BEFORE deciding to push);
 * `image` names a tagged image for a local dogfood load. */
export function outputSpec(
	mode: "oci" | "image",
	tag: string,
	ociDir: string,
): string {
	return mode === "oci"
		? `type=oci,dest=${ociDir},tar=false`
		: `type=image,name=${tag}`;
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
