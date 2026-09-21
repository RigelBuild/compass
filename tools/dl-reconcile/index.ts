import { readFile } from "node:fs/promises";
import { resolve } from "node:path";
export interface LandedDecision {
	id: string;
	surface: "designs";
	ref: "none";
}

export interface ReconcileRequest {
	repo: "compass";
	landed: LandedDecision[];
}

function splitLedgerCells(line: string): string[] {
	const cells: string[] = [];
	let current = "";
	for (let i = 0; i < line.length; i++) {
		const character = line[i];
		if (character === "\\" && (line[i + 1] === "|" || line[i + 1] === "\\")) {
			current += line[i + 1];
			i++;
		} else if (character === "|") {
			cells.push(current.trim());
			current = "";
		} else {
			current += character;
		}
	}
	cells.push(current.trim());
	if (cells[0] === "") cells.shift();
	if (cells.at(-1) === "") cells.pop();
	return cells;
}

/** Parse every decision ID row in the design ledger, preserving duplicates and order. */
export function parseLedger(text: string): LandedDecision[] {
	const landed: LandedDecision[] = [];
	let inFence = false;
	for (const line of text.split("\n")) {
		if (/^\s*(```|~~~)/.test(line)) {
			inFence = !inFence;
			continue;
		}
		if (inFence) continue;
		const trimmed = line.trim();
		if (!trimmed.startsWith("|")) continue;
		const cells = splitLedgerCells(trimmed);
		if (cells.length !== 4) continue;
		const id = cells[0];
		if (id !== undefined && /^DL-\d+$/.test(id)) {
			landed.push({ id, surface: "designs", ref: "none" });
		}
	}
	return landed;
}

export function buildRequestBody(ledger: string): ReconcileRequest {
	return { repo: "compass", landed: parseLedger(ledger) };
}

/** Only the request call is injected, so the seam omits `fetch`'s extras. */
export type FetchFn = (
	input: string | URL | Request,
	init?: RequestInit,
) => Promise<Response>;

export interface ReconcileDeps {
	fetchFn?: FetchFn;
	timeoutMs?: number;
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
			signal: AbortSignal.timeout(deps.timeoutMs ?? 30_000),
		},
	);
	if (!response.ok) {
		throw new Error(`reconciliation failed with HTTP ${response.status}`);
	}
}

if (import.meta.main) {
	const token = process.env.DL_CLAIM_TOKEN ?? "";
	try {
		const ledger = await readFile(
			resolve(import.meta.dir, "../../docs/designs/DECISIONS.md"),
			"utf8",
		);
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
