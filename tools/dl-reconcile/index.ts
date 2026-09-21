import { readFile } from "node:fs/promises";

export interface LandedDecision {
	id: string;
	surface: "designs";
	ref: "none";
}

export interface ReconcileRequest {
	repo: "compass";
	landed: LandedDecision[];
}

/** Parse every decision ID row in the design ledger, preserving duplicates and order. */
export function parseLedger(text: string): LandedDecision[] {
	const landed: LandedDecision[] = [];
	for (const line of text.split("\n")) {
		const match = /^\s*\|\s*(DL-\d+)\s*\|/.exec(line);
		if (match?.[1] !== undefined) {
			landed.push({ id: match[1], surface: "designs", ref: "none" });
		}
	}
	return landed;
}

export function buildRequestBody(ledger: string): ReconcileRequest {
	return { repo: "compass", landed: parseLedger(ledger) };
}

/** Only the request call is injected, so the seam omits `fetch`'s extras. */
export type FetchFn = (
	input: URL | RequestInfo,
	init?: RequestInit,
) => Promise<Response>;

export interface ReconcileDeps {
	fetchFn?: FetchFn;
}

/** POST the ledger reconciliation payload to the deployment service. */
export async function reconcile(
	body: ReconcileRequest,
	token: string,
	deps: ReconcileDeps = {},
): Promise<void> {
	if (token.length === 0) throw new Error("DL_CLAIM_TOKEN is required");
	const response = await (deps.fetchFn ?? fetch)(
		"https://dl.rigel.build/reconcile",
		{
			method: "POST",
			headers: {
				Authorization: `Bearer ${token}`,
				"Content-Type": "application/json",
			},
			body: JSON.stringify(body),
		},
	);
	if (!response.ok) {
		throw new Error(`reconciliation failed with HTTP ${response.status}`);
	}
}

if (import.meta.main) {
	const token = process.env.DL_CLAIM_TOKEN ?? "";
	try {
		const ledger = await readFile("docs/designs/DECISIONS.md", "utf8");
		await reconcile(buildRequestBody(ledger), token);
		console.log("Design ledger reconciliation completed.");
	} catch (error) {
		console.error(
			"Design ledger reconciliation failed:",
			error instanceof Error ? error.message : String(error),
		);
		process.exitCode = 1;
	}
}
