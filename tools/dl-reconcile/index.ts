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

type Fence = { marker: "`" | "~"; length: number };

function updateFence(
	line: string,
	fence: Fence | null,
): { fence: Fence | null; handled: boolean } {
	const match = /^\s*(`{3,}|~{3,})(.*)$/.exec(line);
	if (!match) return { fence, handled: false };
	const marker = match[1]?.[0];
	const length = match[1]?.length ?? 0;
	const info = match[2]?.trim() ?? "";
	if (fence === null && (marker === "`" || marker === "~"))
		return { fence: { marker, length }, handled: true };
	// CommonMark forbids an info string on a closing fence, so a fence line
	// carrying one is content, not a closer.
	if (
		fence !== null &&
		marker === fence.marker &&
		length >= fence.length &&
		info === ""
	)
		return { fence: null, handled: true };
	return { fence, handled: true };
}

/** Parse every decision ID row in the design ledger, preserving duplicates and order. */
export function parseLedger(text: string): LandedDecision[] {
	const landed: LandedDecision[] = [];
	let fence: Fence | null = null;
	for (const line of text.split("\n")) {
		const updated = updateFence(line, fence);
		fence = updated.fence;
		if (updated.handled || fence !== null) continue;
		const trimmed = line.trim();
		if (!trimmed.startsWith("|")) continue;
		const cells = splitLedgerCells(trimmed);
		if (cells.length !== 4) continue;
		const id = cells[0];
		if (id !== undefined && /^DL-\d+$/.test(id))
			landed.push({ id, surface: "designs", ref: "none" });
	}
	if (fence !== null)
		throw new Error("unterminated fenced block in design ledger");
	return landed;
}

/**
 * Count the rows the parser should have produced. Fence tracking is shared with
 * parseLedger deliberately: only ROW recognition is independent, so this catches
 * a row-parsing regression without disagreeing about what is inside a fence.
 */
export function countRawLedgerRows(text: string): number {
	let fence: Fence | null = null;
	let count = 0;
	for (const line of text.split("\n")) {
		const updated = updateFence(line, fence);
		fence = updated.fence;
		if (updated.handled || fence !== null) continue;
		if (/^\s*\|\s*DL-\d+\s*\|/.test(line)) count++;
	}
	return count;
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
			signal: AbortSignal.timeout(timeoutMs),
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
		const body = buildRequestBody(ledger);
		const rawCount = countRawLedgerRows(ledger);
		if (body.landed.length !== rawCount) {
			throw new Error(
				`ledger parse mismatch: parsed ${body.landed.length} rows, found ${rawCount} raw rows`,
			);
		}
		if (process.argv.includes("--check")) {
			console.log(
				`Design ledger parse check passed (${body.landed.length} rows).`,
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
