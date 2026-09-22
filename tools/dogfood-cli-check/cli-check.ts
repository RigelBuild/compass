export type CliRunResult =
	| { readonly kind: "exit"; readonly code: number }
	| { readonly kind: "timeout" }
	| { readonly kind: "spawn-failure" };

export interface CliCheckInput {
	readonly binaryExists: boolean;
	readonly binaryExecutable: boolean;
	readonly binaryMtimeMs: number | null;
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
	if (input.sourceError !== null || input.newestSourceMtimeMs === null) {
		return {
			ok: false,
			message: `operator CLI freshness cannot be checked: ${input.sourceError ?? "the linked source set is empty"}`,
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
	if (input.run?.kind !== "exit" || input.run.code !== 0) {
		return {
			ok: false,
			message: `operator CLI failed to run --help (exit ${input.run?.kind === "exit" ? input.run.code : "unknown"})`,
		};
	}
	return { ok: true };
}
