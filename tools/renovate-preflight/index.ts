// The Renovate preflight CI entry point (see preflight.ts for the pure decision
// core). Run by the `renovate` meta job (ci/workflows/meta.ts) immediately
// before `bunx renovate`: it probes GitHub with RENOVATE_TOKEN via a single
// GraphQL repo query, classifies the outcome, prints an actionable one-liner,
// and exits non-zero on failure so an auth problem stops the run with a clear
// message instead of Renovate's opaque `platform-unknown-error`.
//
// Inputs (env):
//   REPO           - owner/name (from github.repository).
//   RENOVATE_TOKEN - the bot PAT Renovate itself authenticates with.
//
// Exit codes:
//   0 - token authenticates and can see the repo (proceed to Renovate).
//   1 - preflight failed (no token / bad credentials / no access / unknown).
//   2 - could not evaluate (missing REPO env) — fail closed.

import { $ } from "bun";
import { classify, type ProbeResult } from "./preflight.ts";

/**
 * Probe the repo with the same credential Renovate uses. Mirrors Renovate's own
 * initRepo GraphQL query (owner/name → repository) so a pass here means a pass
 * there. `timeout` bounds the call so a network hang fails closed rather than
 * blocking CI. gh reads GH_TOKEN, which index sets from RENOVATE_TOKEN.
 */
async function probe(
	owner: string,
	name: string,
	tokenPresent: boolean,
): Promise<ProbeResult> {
	const query =
		"query($owner:String!,$name:String!){repository(owner:$owner,name:$name){nameWithOwner defaultBranchRef{name}}}";
	// `.nothrow().quiet()` so a non-zero gh exit is captured, not thrown, and
	// its streams are ours to classify rather than being echoed raw. `timeout`
	// (coreutils, always present in the Linux orion-ci-publish image this runs in)
	// bounds the call so a network hang fails closed (exit 124 → unknown) rather
	// than blocking CI. Bun's ShellPromise has no .timeout() in the pinned bun.
	const res =
		await $`timeout 30 gh api graphql -F owner=${owner} -F name=${name} -f query=${query}`
			.nothrow()
			.quiet();
	return {
		tokenPresent,
		exitCode: res.exitCode,
		stdout: res.stdout.toString(),
		stderr: res.stderr.toString(),
	};
}

async function main(): Promise<number> {
	const repo = process.env.REPO;
	if (!repo) {
		console.error(
			"renovate-preflight: REPO must be set (from github.repository).",
		);
		return 2;
	}

	// A malformed REPO (no owner/name split) would send an undefined GraphQL
	// variable and fail with a cryptic error — exactly the opaque failure this
	// preflight exists to prevent. Fail closed with a clear message instead.
	const [owner, name] = repo.split("/");
	if (!owner || !name) {
		console.error(
			`renovate-preflight: REPO must be in owner/name format (from github.repository), got: ${repo}`,
		);
		return 2;
	}

	const token = process.env.RENOVATE_TOKEN;
	const tokenPresent = !!token && token.length > 0;

	// Only probe when there is a token to probe with; classify() handles the
	// no-token case without an API call. Keep the ProbeResult so the unknown
	// branch can honor its "inspect the probe output below" instruction.
	const probeResult = tokenPresent
		? await probe(owner, name, true)
		: { tokenPresent: false, exitCode: 0, stdout: "", stderr: "" };
	const result = classify(probeResult);

	const icon = result.pass ? "✓" : "✗";
	console.log(
		`renovate-preflight: ${icon} [${result.reason}] ${result.message}`,
	);
	// The unknown fallback tells the operator to inspect the probe output; it is
	// only useful if we actually print it. Echo whatever the probe captured.
	if (result.reason === "unknown") {
		const { exitCode, stdout, stderr } = probeResult;
		console.log(`  probe exit code: ${exitCode}`);
		if (stdout.trim()) console.log(`  probe stdout: ${stdout.trim()}`);
		if (stderr.trim()) console.log(`  probe stderr: ${stderr.trim()}`);
	}
	return result.pass ? 0 : 1;
}

// Fail closed on any unexpected throw (gh binary missing, probe timeout, or an
// unforeseen error) with a single actionable line — never a raw stack trace,
// which is the opaque failure this preflight exists to replace.
try {
	process.exit(await main());
} catch (err) {
	console.error(
		"renovate-preflight: ✗ [error] unexpected failure probing GitHub — check that gh is available and RENOVATE_TOKEN is valid:",
		err,
	);
	process.exit(2);
}
