// The Renovate preflight's decision core: a pure function over the result of a
// single GitHub GraphQL repo probe, with no I/O, so the whole classification is
// exhaustively unit-testable. The entry point (index.ts) runs the probe via
// `gh` and feeds the result here; the CI meta job (ci/workflows/meta.ts) runs
// the entry immediately before `bunx renovate`.
//
// Why this exists: when RENOVATE_TOKEN is expired or lacks access to the repo,
// Renovate's own initRepo fails with the opaque `platform-unknown-error` (it
// only logs the underlying GraphQL error at debug level, which the cron run
// does not capture). This preflight turns that silent, cryptic failure into an
// actionable one-line diagnosis BEFORE Renovate starts, so an unattended cron
// run leaves a log line an operator can act on ("rotate the token", "grant the
// repo") instead of a dead-end error.

/** How the token authenticated against the repo. */
export type PreflightReason =
	| "ok" // token authenticates and can see the repo
	| "no-token" // RENOVATE_TOKEN is unset/empty — nothing to probe with
	| "bad-credentials" // token is expired or invalid (GitHub 401)
	| "no-repo-access" // token is valid but cannot see the repo (GraphQL NOT_FOUND)
	| "rate-limited" // token is valid but GitHub's API rate limit is exhausted
	| "unknown"; // probe failed for a reason we do not specifically classify

/** The result of the `gh api graphql` repo probe, as captured by index.ts. */
export interface ProbeResult {
	/** RENOVATE_TOKEN was present and non-empty when the probe ran. */
	tokenPresent: boolean;
	/** The gh process exit code (0 on success). */
	exitCode: number;
	/** Combined stdout from gh (the GraphQL JSON response body). */
	stdout: string;
	/** Combined stderr from gh (gh's human-readable error line). */
	stderr: string;
}

export interface PreflightResult {
	/** True only when the token authenticates and can see the repo. */
	pass: boolean;
	reason: PreflightReason;
	/** Human-readable, actionable one-liner for the CI log. */
	message: string;
}

/**
 * Classify a repo probe into an actionable diagnosis. Order matters: an absent
 * token short-circuits before any output inspection, then the specific auth
 * failures (bad credentials, no repo access) are matched on GitHub's stable
 * error signals, and anything else falls through to `unknown` (fail closed —
 * the caller still refuses to run Renovate, but says so honestly).
 *
 * The signals are matched case-insensitively on the union of stdout+stderr so
 * the classifier is robust to which stream gh writes a given message to.
 */
export function classify(probe: ProbeResult): PreflightResult {
	if (!probe.tokenPresent) {
		return {
			pass: false,
			reason: "no-token",
			message:
				"RENOVATE_TOKEN is not set. Stage it as a Woodpecker secret (events: cron, manual) so the renovate job can authenticate to GitHub.",
		};
	}

	if (probe.exitCode === 0) {
		return {
			pass: true,
			reason: "ok",
			message: "RENOVATE_TOKEN authenticates and can access the repository.",
		};
	}

	const haystack = `${probe.stdout}\n${probe.stderr}`.toLowerCase();

	// GitHub returns "Bad credentials" (HTTP 401) for an expired or invalid
	// token — the whole request is rejected before any data resolves.
	if (haystack.includes("bad credentials") || haystack.includes("http 401")) {
		return {
			pass: false,
			reason: "bad-credentials",
			message:
				"RENOVATE_TOKEN is invalid or expired (GitHub returned 401 Bad credentials). Rotate the PAT and re-stage the Woodpecker secret.",
		};
	}

	// A valid token that cannot see the repo resolves the repository to null
	// with a NOT_FOUND GraphQL error — indistinguishable from a truly missing
	// repo over GraphQL, and the common shape when a fine-grained PAT has not
	// been granted access to this repository.
	if (
		haystack.includes("not_found") ||
		haystack.includes("could not resolve to a repository")
	) {
		return {
			pass: false,
			reason: "no-repo-access",
			message:
				"RENOVATE_TOKEN cannot access the repository (GitHub returned NOT_FOUND). Grant the PAT access to this repo (fine-grained: add the repo + Contents/Pull-requests write; classic: the `repo` scope).",
		};
	}

	// A valid token whose GitHub API budget is exhausted — not an auth failure.
	// Renovate handles this as its own PLATFORM_RATE_LIMIT_EXCEEDED, so surface
	// it distinctly: the fix is to wait/back off or raise the limit, not to
	// touch the token.
	if (haystack.includes("rate_limit") || haystack.includes("rate limit")) {
		return {
			pass: false,
			reason: "rate-limited",
			message:
				"GitHub's API rate limit is exhausted for RENOVATE_TOKEN (not an auth failure). Wait for the limit to reset or reduce Renovate's run frequency; the token itself is valid.",
		};
	}

	return {
		pass: false,
		reason: "unknown",
		message:
			"RENOVATE_TOKEN failed the GitHub preflight for an unrecognized reason. Inspect the probe output below and check the token's validity and scopes.",
	};
}
