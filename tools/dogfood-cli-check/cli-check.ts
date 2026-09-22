/**
 * Keep the in-repo build inputs from `go list`'s one-path-per-line output.
 * Embeds count: a changed embedded asset rebuilds the binary but touches no
 * .go file. External deps are dropped — go.mod/go.sum already pin them, so a
 * change there shows up as a module-input mtime instead.
 */
export function parseListedInputs(
	stdout: string,
	repoRoot: string,
): readonly string[] {
	return stdout
		.split("\n")
		.map((line) => line.trimEnd())
		.filter((line) => line.startsWith(`${repoRoot}/`));
}

export type CliRunResult =
	| { readonly kind: "exit"; readonly code: number }
	| { readonly kind: "timeout" }
	| { readonly kind: "signal"; readonly signal: string }
	| { readonly kind: "spawn-failure" };

export interface CliCheckInput {
	readonly binaryExists: boolean;
	readonly binaryExecutable: boolean;
	readonly binaryMtimeMs: number | null;
	/** Null only alongside a `sourceError`; the scan never half-succeeds. */
	readonly newestSourceMtimeMs: number | null;
	readonly sourceError: string | null;
	readonly run: CliRunResult | null;
}

export type CliCheckResult =
	| { readonly ok: true }
	| { readonly ok: false; readonly message: string };

export function assertCliArtifact(input: CliCheckInput): CliCheckResult {
	if (!input.binaryExists) {
		return {
			ok: false,
			message:
				"operator CLI is absent: the dogfood:build-cli task never ran (or failed before producing the binary)",
		};
	}
	if (!input.binaryExecutable) {
		return {
			ok: false,
			message: "operator CLI exists but is not executable",
		};
	}
	if (input.sourceError !== null) {
		return {
			ok: false,
			message: `operator CLI freshness cannot be checked: ${input.sourceError}`,
		};
	}
	if (input.newestSourceMtimeMs === null) {
		return {
			ok: false,
			message:
				"operator CLI freshness cannot be checked: no build inputs resolved",
		};
	}
	if (
		input.binaryMtimeMs === null ||
		input.binaryMtimeMs <= input.newestSourceMtimeMs
	) {
		return {
			ok: false,
			message:
				"operator CLI is stale: its mtime is not newer than linked Go sources or module inputs",
		};
	}
	if (input.run?.kind === "timeout") {
		return {
			ok: false,
			message: "operator CLI timed out while running --help",
		};
	}
	if (input.run?.kind === "spawn-failure") {
		return {
			ok: false,
			message:
				"operator CLI could not be started: artifact is corrupt or unrunnable",
		};
	}
	if (input.run?.kind === "signal") {
		return {
			ok: false,
			message: `operator CLI crashed on --help (killed by ${input.run.signal})`,
		};
	}
	if (input.run?.kind !== "exit" || input.run.code !== 0) {
		return {
			ok: false,
			message: `operator CLI failed to run --help (exit ${input.run?.kind === "exit" ? input.run.code : "unknown"})`,
		};
	}
	return { ok: true };
}
