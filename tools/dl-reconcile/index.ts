import { readFile } from "node:fs/promises";
import { basename, resolve } from "node:path";
import { parseDecisionFile } from "../design-ledger-gate/decision-files.ts";

export interface LandedDecision {
	id: string;
	surface: "designs";
	ref: "none";
}

export interface ReconcileRequest {
	repo: "compass";
	landed: LandedDecision[];
}

interface DecisionFileText {
	path: string;
	text: string;
}

interface DecisionFileDeps {
	listDecisionFiles?: (root: string) => Promise<readonly string[]>;
	readDecisionFile?: (root: string, path: string) => Promise<string>;
}

const DECISION_GLOB = "docs/designs/decisions/*/DL-*.md";

async function listDecisionFiles(root: string): Promise<string[]> {
	const glob = new Bun.Glob(DECISION_GLOB);
	const paths: string[] = [];
	for await (const path of glob.scan({ cwd: root, onlyFiles: true })) {
		paths.push(path.replaceAll("\\", "/"));
	}
	return paths;
}

function buildRequestBody(
	files: readonly DecisionFileText[],
): ReconcileRequest {
	if (files.length === 0) {
		throw new Error(
			"decision file list is empty; refusing to post an empty frontier",
		);
	}
	const landed = files.map(({ path, text }) => {
		const parsed = parseDecisionFile(path, text);
		if (!parsed.ok) {
			throw new Error(
				`malformed decision file ${path}: line ${parsed.error.line}: ${parsed.error.reason}`,
			);
		}
		return {
			path,
			decision: {
				id: basename(path, ".md"),
				surface: "designs" as const,
				ref: "none" as const,
			},
		};
	});
	landed.sort(
		(a, b) =>
			Number(a.decision.id.slice(3)) - Number(b.decision.id.slice(3)) ||
			a.path.localeCompare(b.path),
	);
	return { repo: "compass", landed: landed.map(({ decision }) => decision) };
}

export async function assertReconcilableDecisionFiles(
	root: string,
	deps: DecisionFileDeps = {},
): Promise<ReconcileRequest> {
	const paths = await (deps.listDecisionFiles ?? listDecisionFiles)(root);
	if (paths.length === 0) return buildRequestBody([]);
	const readDecisionFile =
		deps.readDecisionFile ??
		((repoRoot: string, path: string) =>
			readFile(resolve(repoRoot, path), "utf8"));
	const files = await Promise.all(
		paths.map(async (path) => ({
			path,
			text: await readDecisionFile(root, path),
		})),
	);
	return buildRequestBody(files);
}

/** Only the request call is injected, so the seam omits `fetch`'s extras. */
export type FetchFn = (
	input: string | URL | Request,
	init?: RequestInit,
) => Promise<Response>;

export interface ReconcileDeps {
	fetchFn?: FetchFn;
	timeoutMs?: number;
	/**
	 * Test seam, not a supported knob: production always uses
	 * `AbortSignal.timeout`. Injected so a test can observe the deadline the
	 * caller actually requested.
	 */
	timeoutSignal?: (timeoutMs: number) => AbortSignal;
}

/** POST the decision-file reconciliation payload to the deployment service. */
export async function reconcile(
	body: ReconcileRequest,
	token: string,
	deps: ReconcileDeps = {},
): Promise<void> {
	const trimmedToken = token.trim();
	if (trimmedToken.length === 0) throw new Error("DL_CLAIM_TOKEN is required");
	const timeoutMs =
		deps.timeoutMs !== undefined && deps.timeoutMs > 0
			? deps.timeoutMs
			: 30_000;
	const response = await (deps.fetchFn ?? fetch)(
		"https://dl.rigel.build/reconcile",
		{
			method: "POST",
			redirect: "error",
			headers: {
				Authorization: `Bearer ${trimmedToken}`,
				"Content-Type": "application/json",
			},
			body: JSON.stringify(body),
			signal: (deps.timeoutSignal ?? AbortSignal.timeout)(timeoutMs),
		},
	);
	if (!response.ok) {
		throw new Error(`reconciliation failed with HTTP ${response.status}`);
	}
}

if (import.meta.main) {
	const token = process.env.DL_CLAIM_TOKEN ?? "";
	try {
		const root = resolve(import.meta.dir, "../..");
		const body = await assertReconcilableDecisionFiles(root);
		if (process.argv.includes("--check")) {
			console.log(
				`Design ledger parse check passed (${body.landed.length} decision files).`,
			);
			process.exitCode = 0;
		} else {
			await reconcile(body, token);
			console.log("Design ledger reconciliation completed.");
		}
	} catch (error) {
		console.error(
			"Design ledger reconciliation failed:",
			error instanceof Error ? error.message : String(error),
		);
		process.exitCode = 1;
	}
}
