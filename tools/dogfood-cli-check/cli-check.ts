export interface CliCheckInput {
	readonly binaryExists: boolean;
	readonly binaryExecutable: boolean;
	readonly binaryMtimeMs: number | null;
	readonly newestSourceMtimeMs: number;
	readonly runExitCode: number | null;
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
	if (
		input.binaryMtimeMs === null ||
		input.binaryMtimeMs <= input.newestSourceMtimeMs
	) {
		return {
			ok: false,
			message:
				"operator CLI is stale: its mtime is not newer than go/cmd/compass sources",
		};
	}
	if (input.runExitCode !== 0) {
		return {
			ok: false,
			message: `operator CLI failed to run --help (exit ${input.runExitCode ?? "unknown"})`,
		};
	}
	return { ok: true };
}
