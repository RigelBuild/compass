// Unit tests for compass-preview-deploy's pure state machine + dispatch shell
// (index.ts).
//
// This tool is a deploy oracle: `decide()` defines who owns the shared preview
// env and — the load-bearing bit — that a fork NEVER deploys or displaces, and
// a stale displacement event never tears down the live incumbent. This suite
// defends that contract exhaustively, plus the Deployment-record status
// sequencing (in_progress → success/failure, inactive-on-release) that surfaces
// state on the PR.
//
// Conventions (mirroring tools/cx-token-gate/index.test.ts and
// tools/design-ledger-gate/index.test.ts):
// - Literal label/environment strings, NOT values derived from the module
//   constants (PREVIEW_LABEL / PREVIEW_ENVIRONMENT): those constants ARE the
//   thing under test, so deriving inputs from them would let a drifted constant
//   pass silently.
// - `ctx()` yields a valid same-repo baseline so each test perturbs one axis.

import { describe, expect, test } from "bun:test";
import {
	type ActiveDeployment,
	type Deployer,
	type DeploymentState,
	type Deps,
	decide,
	deploymentPayload,
	deploymentPr,
	type EventContext,
	type GitHubApi,
	isEventAction,
	parseLabels,
	runOnce,
} from "./index.ts";

const BASE = "RigelBuild/compass";
const FORK = "attacker/compass";

/** A valid same-repo `labeled` baseline; perturb one axis per test. */
function ctx(over: Partial<EventContext> = {}): EventContext {
	return {
		action: "labeled",
		prNumber: 10,
		headRepo: BASE,
		baseRepo: BASE,
		headSha: "abc123",
		changedLabel: "preview",
		prLabels: ["preview"],
		currentHolders: [10],
		activePreviewPr: null,
		...over,
	};
}

// ---------------------------------------------------------------------------
// decide — the label lifecycle state machine.
// ---------------------------------------------------------------------------

describe("decide — labeled", () => {
	test("a same-repo claim with no other holder deploys, displacing none", () => {
		const d = decide(ctx({ currentHolders: [10] }));
		expect(d).toEqual({ kind: "claim", displaced: [] });
	});

	test("a same-repo claim displaces every OTHER current holder", () => {
		const d = decide(ctx({ prNumber: 10, currentHolders: [7, 10, 12] }));
		expect(d).toEqual({ kind: "claim", displaced: [7, 12] });
	});

	test("a FORK claim is rejected up front — never claim, never displace", () => {
		const d = decide(ctx({ headRepo: FORK, currentHolders: [3, 10] }));
		expect(d).toEqual({ kind: "reject-fork" });
	});

	test("a non-preview label is a no-op", () => {
		const d = decide(ctx({ changedLabel: "bug" }));
		expect(d.kind).toBe("noop");
	});
});

describe("decide — synchronize", () => {
	test("the current holder redeploys", () => {
		const d = decide(
			ctx({ action: "synchronize", changedLabel: null, prLabels: ["preview"] }),
		);
		expect(d).toEqual({ kind: "redeploy" });
	});

	test("a synchronize on an UNLABELED PR is a no-op", () => {
		const d = decide(
			ctx({ action: "synchronize", changedLabel: null, prLabels: ["bug"] }),
		);
		expect(d.kind).toBe("noop");
	});

	test("a synchronize on a FORK PR never deploys", () => {
		const d = decide(
			ctx({
				action: "synchronize",
				headRepo: FORK,
				changedLabel: null,
				prLabels: ["preview"],
			}),
		);
		expect(d.kind).toBe("noop");
	});
});

describe("decide — unlabeled / closed release", () => {
	test("unlabeling the ACTIVE holder releases the env", () => {
		const d = decide(
			ctx({ action: "unlabeled", prNumber: 10, activePreviewPr: 10 }),
		);
		expect(d).toEqual({ kind: "release" });
	});

	test("unlabeling a NON-active PR is a no-op (displaced-loser race safety)", () => {
		// The winner (#12) is active; stripping the loser's (#10) label fires
		// `unlabeled` on #10, which must NOT tear down #12's live env.
		const d = decide(
			ctx({ action: "unlabeled", prNumber: 10, activePreviewPr: 12 }),
		);
		expect(d.kind).toBe("noop");
	});

	test("unlabeling a non-preview label is a no-op", () => {
		const d = decide(
			ctx({
				action: "unlabeled",
				changedLabel: "bug",
				prNumber: 10,
				activePreviewPr: 10,
			}),
		);
		expect(d.kind).toBe("noop");
	});

	test("closing the ACTIVE holder releases the env", () => {
		const d = decide(
			ctx({
				action: "closed",
				changedLabel: null,
				prNumber: 10,
				activePreviewPr: 10,
			}),
		);
		expect(d).toEqual({ kind: "release" });
	});

	test("closing a NON-active PR is a no-op", () => {
		const d = decide(
			ctx({
				action: "closed",
				changedLabel: null,
				prNumber: 10,
				activePreviewPr: 12,
			}),
		);
		expect(d.kind).toBe("noop");
	});
});

// ---------------------------------------------------------------------------
// isEventAction / parseLabels — the env-parse boundary.
// ---------------------------------------------------------------------------

describe("isEventAction", () => {
	test("accepts the four handled actions", () => {
		for (const a of ["labeled", "synchronize", "unlabeled", "closed"]) {
			expect(isEventAction(a)).toBe(true);
		}
	});
	test("rejects an unhandled action", () => {
		expect(isEventAction("opened")).toBe(false);
		expect(isEventAction("")).toBe(false);
	});
});

describe("parseLabels", () => {
	test("splits, trims, and drops empties", () => {
		expect(parseLabels("preview, bug ,")).toEqual(["preview", "bug"]);
	});
	test("undefined / empty → no labels", () => {
		expect(parseLabels(undefined)).toEqual([]);
		expect(parseLabels("")).toEqual([]);
	});
});

// ---------------------------------------------------------------------------
// runOnce — dispatch, the Deployment status sequence, and the fork posture.
// ---------------------------------------------------------------------------

/** A recording fake pair for the injected side effects. */
function fakes(over?: {
	deployThrows?: boolean;
	releaseThrows?: boolean;
	labelApiThrows?: boolean;
	active?: ActiveDeployment | null;
}) {
	const calls: string[] = [];
	const outputs: Record<string, string> = {};
	let nextDeploymentId = 100;

	const gh: GitHubApi = {
		removeLabel: async (pr, label) => {
			calls.push(`removeLabel(${pr},${label})`);
			if (over?.labelApiThrows) throw new Error("403 read-only fork token");
		},
		postComment: async (pr) => {
			calls.push(`postComment(${pr})`);
			if (over?.labelApiThrows) throw new Error("403 read-only fork token");
		},
		createDeployment: async (pr, ref) => {
			calls.push(`createDeployment(${pr},${ref})`);
			return nextDeploymentId++;
		},
		setDeploymentStatus: async (
			id: number,
			state: DeploymentState,
			url?: string,
		) => {
			calls.push(`status(${id},${state}${url ? `,${url}` : ""})`);
		},
		findActiveDeployment: async () => over?.active ?? null,
	};

	const deployer: Deployer = {
		deploy: async () => {
			calls.push("deploy");
			if (over?.deployThrows) throw new Error("boom");
			return { environmentUrl: "https://mattfw/" };
		},
		release: async () => {
			calls.push("release");
			if (over?.releaseThrows) throw new Error("boom");
		},
	};

	const deps = (c: EventContext): Deps => ({
		ctx: c,
		gh,
		deployer,
		emitOutput: (n, v) => {
			outputs[n] = v;
		},
		log: () => {},
		err: () => {},
	});

	return { calls, outputs, deps };
}

describe("runOnce — claim", () => {
	test("a clean claim: no displace, create → in_progress → success, link emitted", async () => {
		const f = fakes();
		const code = await runOnce(f.deps(ctx({ currentHolders: [10] })));
		expect(code).toBe(0);
		expect(f.calls).toEqual([
			"createDeployment(10,abc123)",
			"status(100,in_progress)",
			"deploy",
			"status(100,success,https://mattfw/)",
		]);
		expect(f.outputs.preview_url).toBe("https://mattfw/");
		expect(f.outputs.preview_pr).toBe("10");
	});

	test("a claim displacing another holder strips its label + comments FIRST", async () => {
		const f = fakes();
		const code = await runOnce(
			f.deps(ctx({ prNumber: 10, currentHolders: [7, 10] })),
		);
		expect(code).toBe(0);
		expect(f.calls.slice(0, 2)).toEqual([
			"removeLabel(7,preview)",
			"postComment(7)",
		]);
		expect(f.calls).toContain("deploy");
	});

	test("a failed deploy marks the Deployment failure and exits 1", async () => {
		const f = fakes({ deployThrows: true });
		const code = await runOnce(f.deps(ctx({ currentHolders: [10] })));
		expect(code).toBe(1);
		expect(f.calls).toEqual([
			"createDeployment(10,abc123)",
			"status(100,in_progress)",
			"deploy",
			"status(100,failure)",
		]);
		expect(f.outputs.preview_url).toBeUndefined();
	});
});

describe("runOnce — fork rejection", () => {
	test("a fork claim strips the label + comments, and NEVER deploys or displaces", async () => {
		const f = fakes({ active: { id: 55, pr: 9 } });
		const code = await runOnce(
			f.deps(ctx({ headRepo: FORK, prNumber: 10, currentHolders: [9, 10] })),
		);
		expect(code).toBe(0);
		expect(f.calls).toEqual(["removeLabel(10,preview)", "postComment(10)"]);
		// The incumbent (#9) is untouched: no displace, no deploy, no status flip.
		expect(f.calls).not.toContain("deploy");
		expect(f.calls.some((c) => c.startsWith("createDeployment"))).toBe(false);
		expect(f.calls).not.toContain("removeLabel(9,preview)");
	});

	test("a fork claim tolerates a throwing label API (best-effort) and still returns 0", async () => {
		// On a real fork the `on: pull_request` token is read-only, so removeLabel
		// + postComment 403. realGitHubApi swallows via .nothrow(); this pins that
		// runOnce's fork-reject path also tolerates an injected API that throws.
		const f = fakes({ labelApiThrows: true });
		const code = await runOnce(
			f.deps(ctx({ headRepo: FORK, prNumber: 10, currentHolders: [10] })),
		);
		expect(code).toBe(0);
		expect(f.calls).not.toContain("deploy");
		expect(f.calls.some((c) => c.startsWith("createDeployment"))).toBe(false);
	});
});

describe("runOnce — release", () => {
	test("releasing the active holder stops the service and marks it inactive", async () => {
		const f = fakes({ active: { id: 77, pr: 10 } });
		const code = await runOnce(
			f.deps(ctx({ action: "unlabeled", prNumber: 10, activePreviewPr: 10 })),
		);
		expect(code).toBe(0);
		expect(f.calls).toEqual(["release", "status(77,inactive)"]);
	});

	test("a release whose active deployment is a DIFFERENT PR does not flip it", async () => {
		// decide already no-ops a non-active unlabel; this guards runOnce too when
		// the active record races to a different PR between decide and dispatch.
		const f = fakes({ active: { id: 77, pr: 12 } });
		const code = await runOnce(
			f.deps(
				ctx({
					action: "closed",
					changedLabel: null,
					prNumber: 10,
					activePreviewPr: 10,
				}),
			),
		);
		expect(code).toBe(0);
		expect(f.calls).toEqual(["release"]);
		expect(f.calls.some((c) => c.startsWith("status"))).toBe(false);
	});

	test("a failed release exits 1 without marking inactive", async () => {
		const f = fakes({ releaseThrows: true, active: { id: 77, pr: 10 } });
		const code = await runOnce(
			f.deps(ctx({ action: "unlabeled", prNumber: 10, activePreviewPr: 10 })),
		);
		expect(code).toBe(1);
		expect(f.calls).toEqual(["release"]);
	});
});

describe("runOnce — no-op", () => {
	test("a synchronize on a non-holder does nothing", async () => {
		const f = fakes();
		const code = await runOnce(
			f.deps(
				ctx({ action: "synchronize", changedLabel: null, prLabels: ["bug"] }),
			),
		);
		expect(code).toBe(0);
		expect(f.calls).toEqual([]);
	});
});

// ---------------------------------------------------------------------------
// deploymentPr — the payload PR recovery, robust to string OR object payload.
// ---------------------------------------------------------------------------

describe("deploymentPr", () => {
	test("a JSON STRING payload recovers the pr (the regression case)", () => {
		// The Deployments API returns payload as a string; the read side MUST
		// parse it, else activePreviewPr is always null and release never fires.
		expect(deploymentPr('{"pr":42}')).toBe(42);
	});
	test("an already-parsed object payload recovers the pr", () => {
		expect(deploymentPr({ pr: 42 })).toBe(42);
	});
	test("a malformed JSON string is undefined (never throws)", () => {
		expect(deploymentPr("{pr:42}")).toBeUndefined();
		expect(deploymentPr("not json")).toBeUndefined();
	});
	test("a missing pr is undefined", () => {
		expect(deploymentPr("{}")).toBeUndefined();
		expect(deploymentPr({})).toBeUndefined();
	});
	test("a non-numeric pr is undefined", () => {
		expect(deploymentPr('{"pr":"42"}')).toBeUndefined();
	});
	test("a non-object payload is undefined", () => {
		expect(deploymentPr(null)).toBeUndefined();
		expect(deploymentPr(undefined)).toBeUndefined();
		expect(deploymentPr(42)).toBeUndefined();
	});
});

// ---------------------------------------------------------------------------
// deploymentPayload — the WRITE side of the payload seam (pinned with the read
// side, since this is the seam that regressed under bun's quote-stripping).
// ---------------------------------------------------------------------------

describe("deploymentPayload", () => {
	test("emits valid JSON carrying the pr", () => {
		expect(JSON.parse(deploymentPayload(42))).toEqual({ pr: 42 });
	});
	test("round-trips through deploymentPr (write + read pinned together)", () => {
		expect(deploymentPr(deploymentPayload(42))).toBe(42);
	});
});
