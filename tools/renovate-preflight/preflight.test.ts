// Unit tests for the Renovate preflight's pure decision core (preflight.ts).
//
// These defend the machine-readable contract of `classify` — its `pass`
// boolean and `reason` code — across every auth outcome, plus the branch
// ORDER that decides which diagnosis wins when several signals coincide. The
// `.message` field is human prose, asserted only by substring where an
// actionable instruction MUST surface (stage / rotate / grant / wait).
//
// The probe-output fixtures are the REAL shapes GitHub's `gh api graphql`
// returns, captured live (not paraphrased): a 401 body carries "Bad
// credentials" + "HTTP 401"; a token that cannot see the repo resolves
// `repository` to null with a NOT_FOUND error and gh's "Could not resolve to a
// Repository" line; an exhausted budget returns a RATE_LIMIT /
// graphql_rate_limit error and gh's "API rate limit already exceeded" line.
// Pinning these literal signals keeps the classifier coupled to GitHub's
// stable error surface — a drift in either the classifier or GitHub's wording
// reddens the suite. Signal strings are pinned as literals, NOT derived from
// preflight.ts, because the classifier is the thing under test.

import { describe, expect, test } from "bun:test";
import {
	classify,
	type PreflightReason,
	type ProbeResult,
} from "./preflight.ts";

// --- Ground-truth fixtures: verbatim `gh api graphql` output, captured live. ---

const OK_STDOUT =
	'{"data":{"repository":{"nameWithOwner":"RigelBuild/orion","defaultBranchRef":{"name":"main"}}}}';

const BAD_CREDS_STDOUT =
	'{\n  "message": "Bad credentials",\n  "documentation_url": "https://docs.github.com/rest",\n  "status": "401"\n}';
const BAD_CREDS_STDERR = "gh: Bad credentials (HTTP 401)";

const NO_ACCESS_STDOUT =
	'{"data":{"repository":null},"errors":[{"type":"NOT_FOUND","path":["repository"],"message":"Could not resolve to a Repository with the name \'RigelBuild/orion\'."}]}';
const NO_ACCESS_STDERR =
	"gh: Could not resolve to a Repository with the name 'RigelBuild/orion'.";

const RATE_LIMIT_STDOUT =
	'{"errors":[{"type":"RATE_LIMIT","code":"graphql_rate_limit","message":"API rate limit already exceeded for user ID 288418925."}]}';
const RATE_LIMIT_STDERR =
	"gh: API rate limit already exceeded for user ID 288418925.";

function probe(overrides: Partial<ProbeResult> = {}): ProbeResult {
	// Baseline: token present, successful probe. Each test perturbs one axis.
	return {
		tokenPresent: true,
		exitCode: 0,
		stdout: OK_STDOUT,
		stderr: "",
		...overrides,
	};
}

describe("classify — success (ok)", () => {
	test("token present, exit 0 → pass/ok", () => {
		const r = classify(probe());
		expect(r.pass).toBe(true);
		expect(r.reason).toBe("ok");
	});

	test("exit 0 short-circuits before output inspection: a successful body that happens to contain error words is still ok", () => {
		// Pins branch order: `ok` is decided by the exit code, not by scanning a
		// successful response for scary strings. If signal matching ever ran
		// before the exit-0 check, this misclassifies as bad-credentials.
		const r = classify(
			probe({
				exitCode: 0,
				stdout:
					'{"data":{"repository":{"nameWithOwner":"RigelBuild/orion"}},"note":"bad credentials / HTTP 401 / NOT_FOUND / rate_limit appear in this successful body"}',
			}),
		);
		expect(r.pass).toBe(true);
		expect(r.reason).toBe("ok");
	});
});

describe("classify — no token short-circuits before any probe inspection", () => {
	test("token absent → fail/no-token; message tells the operator to stage the secret", () => {
		const r = classify(probe({ tokenPresent: false }));
		expect(r.pass).toBe(false);
		expect(r.reason).toBe("no-token");
		expect(r.message.toLowerCase()).toContain("stage");
	});

	test("no-token wins over a 401-looking probe body + exit 1 (never blames the token when none was sent)", () => {
		// The very first branch must short-circuit BEFORE output inspection: an
		// absent token with a full 401 body and non-zero exit is still no-token,
		// not bad-credentials.
		const r = classify(
			probe({
				tokenPresent: false,
				exitCode: 1,
				stdout: BAD_CREDS_STDOUT,
				stderr: BAD_CREDS_STDERR,
			}),
		);
		expect(r.pass).toBe(false);
		expect(r.reason).toBe("no-token");
	});
});

describe("classify — bad credentials (expired/invalid token, HTTP 401)", () => {
	// Two OR'd signals ("bad credentials", "http 401"), matched on the
	// case-insensitive union of stdout+stderr. Prove the reason via stdout-only
	// AND stderr-only, and isolate each signal so dropping either alternative
	// reddens the suite. The real fixtures arrive mixed-case ("Bad credentials",
	// "HTTP 401"), so a case-sensitive bug (dropped .toLowerCase) also reddens.
	const cases: Array<{ name: string; probe: ProbeResult }> = [
		{
			name: "real 401 body on stdout only",
			probe: probe({ exitCode: 1, stdout: BAD_CREDS_STDOUT, stderr: "" }),
		},
		{
			name: "gh's 401 line on stderr only (stream union)",
			probe: probe({ exitCode: 1, stdout: "", stderr: BAD_CREDS_STDERR }),
		},
		{
			name: "http 401 signal without the literal 'bad credentials' phrase (stderr only)",
			probe: probe({
				exitCode: 1,
				stdout: "",
				stderr: "gh: request failed (HTTP 401)",
			}),
		},
	];
	for (const c of cases) {
		test(`${c.name} → fail/bad-credentials; message says rotate`, () => {
			const r = classify(c.probe);
			expect(r.pass).toBe(false);
			expect(r.reason).toBe("bad-credentials");
			expect(r.message.toLowerCase()).toContain("rotate");
		});
	}
});

describe("classify — no repo access (valid token, NOT_FOUND)", () => {
	// Two OR'd signals ("not_found", "could not resolve to a repository"). The
	// real body carries both; gh's stderr line carries only the resolve phrase.
	// Prove stdout-only and stderr-only, and isolate the bare NOT_FOUND signal.
	const cases: Array<{ name: string; probe: ProbeResult }> = [
		{
			name: "real GraphQL NOT_FOUND body on stdout only",
			probe: probe({ exitCode: 1, stdout: NO_ACCESS_STDOUT, stderr: "" }),
		},
		{
			name: "gh's resolve line on stderr only (stream union)",
			probe: probe({ exitCode: 1, stdout: "", stderr: NO_ACCESS_STDERR }),
		},
		{
			name: "bare NOT_FOUND error type without the resolve phrase (stdout only)",
			probe: probe({
				exitCode: 1,
				stdout: '{"data":{"repository":null},"errors":[{"type":"NOT_FOUND"}]}',
				stderr: "",
			}),
		},
	];
	for (const c of cases) {
		test(`${c.name} → fail/no-repo-access; message says grant`, () => {
			const r = classify(c.probe);
			expect(r.pass).toBe(false);
			expect(r.reason).toBe("no-repo-access");
			expect(r.message.toLowerCase()).toContain("grant");
		});
	}
});

describe("classify — rate limited (valid token, exhausted API budget)", () => {
	// Two OR'd signals ("rate_limit", "rate limit"). The real body carries the
	// underscore form (graphql_rate_limit) AND the spaced form ("API rate limit
	// already exceeded"); gh's stderr line carries only the spaced form. Prove
	// stdout-only and stderr-only, and isolate the underscore signal.
	const cases: Array<{ name: string; probe: ProbeResult }> = [
		{
			name: "real RATE_LIMIT body on stdout only",
			probe: probe({ exitCode: 1, stdout: RATE_LIMIT_STDOUT, stderr: "" }),
		},
		{
			name: "gh's rate-limit line on stderr only (stream union)",
			probe: probe({ exitCode: 1, stdout: "", stderr: RATE_LIMIT_STDERR }),
		},
		{
			name: "bare graphql_rate_limit code without the spaced phrase (stdout only)",
			probe: probe({
				exitCode: 1,
				stdout: '{"errors":[{"code":"graphql_rate_limit"}]}',
				stderr: "",
			}),
		},
	];
	for (const c of cases) {
		test(`${c.name} → fail/rate-limited; message says wait and that it is not an auth failure`, () => {
			const r = classify(c.probe);
			expect(r.pass).toBe(false);
			expect(r.reason).toBe("rate-limited");
			const msg = r.message.toLowerCase();
			expect(msg).toContain("wait");
			// The distinguishing contract: rate-limited is NOT a credential
			// problem, so the operator must not go rotate a valid token.
			expect(msg).toContain("not an auth failure");
		});
	}
});

describe("classify — precedence within output-signal matching", () => {
	// The documented order is bad-credentials > no-repo-access > rate-limited >
	// unknown. When multiple signals coincide in one probe, the earlier reason
	// must win: a 401 rejects the whole request (rotate the token) rather than
	// sending the operator chasing repo grants or waiting on a rate limit that
	// never applied.
	const cases: Array<{
		name: string;
		probe: ProbeResult;
		reason: PreflightReason;
	}> = [
		{
			name: "bad-credentials wins over a co-present NOT_FOUND signal",
			probe: probe({
				exitCode: 1,
				stdout: `${BAD_CREDS_STDOUT}\n${NO_ACCESS_STDOUT}`,
				stderr: BAD_CREDS_STDERR,
			}),
			reason: "bad-credentials",
		},
		{
			name: "bad-credentials wins over a co-present rate_limit signal",
			probe: probe({
				exitCode: 1,
				stdout: `${BAD_CREDS_STDOUT}\n${RATE_LIMIT_STDOUT}`,
				stderr: BAD_CREDS_STDERR,
			}),
			reason: "bad-credentials",
		},
		{
			name: "bad-credentials wins over BOTH a NOT_FOUND and a rate_limit signal in the same output",
			probe: probe({
				exitCode: 1,
				stdout: `${BAD_CREDS_STDOUT}\n${NO_ACCESS_STDOUT}\n${RATE_LIMIT_STDOUT}`,
				stderr: BAD_CREDS_STDERR,
			}),
			reason: "bad-credentials",
		},
		{
			name: "no-repo-access wins over a co-present rate_limit signal",
			probe: probe({
				exitCode: 1,
				stdout: `${NO_ACCESS_STDOUT}\n${RATE_LIMIT_STDOUT}`,
				stderr: NO_ACCESS_STDERR,
			}),
			reason: "no-repo-access",
		},
	];
	for (const c of cases) {
		test(`${c.name} → ${c.reason}`, () => {
			const r = classify(c.probe);
			expect(r.pass).toBe(false);
			expect(r.reason).toBe(c.reason);
		});
	}
});

describe("classify — unrecognized failure falls through to unknown (fail closed)", () => {
	// Anything non-zero that matches no known signal must fail closed as
	// unknown, so the caller still refuses to run Renovate but says so honestly.
	const cases: Array<{ name: string; probe: ProbeResult }> = [
		{
			name: "non-zero exit with a generic API error we do not classify",
			probe: probe({
				exitCode: 1,
				stdout: "",
				stderr: "gh: Something went wrong while executing your query.",
			}),
		},
		{
			name: "timeout: gh killed, exit 124, empty streams",
			probe: probe({ exitCode: 124, stdout: "", stderr: "" }),
		},
	];
	for (const c of cases) {
		test(`${c.name} → fail/unknown`, () => {
			const r = classify(c.probe);
			expect(r.pass).toBe(false);
			expect(r.reason).toBe("unknown");
		});
	}
});
