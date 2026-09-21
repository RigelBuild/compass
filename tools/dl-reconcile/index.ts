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
	const match = /^ {0,3}(`{3,}|~{3,})(.*)$/.exec(line);
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

function isTableLine(line: string): boolean {
	return /^ {0,3}\|/.test(line);
}

function isLedgerHeader(line: string): boolean {
	if (!isTableLine(line)) return false;
	return (
		splitLedgerCells(line.trim()).join("|") === "ID|Decision|Status|Record"
	);
}

function isLedgerSeparator(line: string): boolean {
	if (!isTableLine(line)) return false;
	const cells = splitLedgerCells(line.trim());
	return cells.length === 4 && cells.every((cell) => /^:?-{3,}:?$/.test(cell));
}

function forEachLedgerRow(
	text: string,
	callback: (line: string) => void,
): void {
	let fence: Fence | null = null;
	let tableState: "none" | "header" | "table" = "none";
	for (const line of text.split("\n")) {
		const updated = updateFence(line, fence);
		fence = updated.fence;
		if (updated.handled || fence !== null) continue;
		if (tableState === "header") {
			if (isLedgerSeparator(line)) tableState = "table";
			else tableState = isLedgerHeader(line) ? "header" : "none";
			continue;
		}
		if (tableState === "table") {
			if (isTableLine(line)) callback(line);
			else tableState = isLedgerHeader(line) ? "header" : "none";
			continue;
		}
		if (isLedgerHeader(line)) tableState = "header";
	}
	if (fence !== null)
		throw new Error("unterminated fenced block in design ledger");
}
/** Parse every decision ID row in the design ledger, preserving duplicates and order. */
export function parseLedger(text: string): LandedDecision[] {
	const landed: LandedDecision[] = [];
	forEachLedgerRow(text, (line) => {
		const trimmed = line.trim();
		const cells = splitLedgerCells(trimmed);
		if (cells.length !== 4) return;
		const id = cells[0];
		if (id !== undefined && /^DL-\d+$/.test(id))
			landed.push({ id, surface: "designs", ref: "none" });
	});
	return landed;
}

/**
 * Count unfenced ledger-shaped rows independently of the parser's table anchor.
 * This deliberately does not model the table anchor, so anchor misreads surface
 * as mismatches. An unfenced non-ledger table beginning with a DL ID reads high.
 */
export function countRawLedgerRows(text: string): number {
	let count = 0;
	let fence: Fence | null = null;
	for (const line of text.split("\n")) {
		const updated = updateFence(line, fence);
		fence = updated.fence;
		if (updated.handled || fence !== null) continue;
		if (/^ {0,3}\|\s*DL-\d+\s*\|/.test(line)) count++;
	}
	if (fence !== null)
		throw new Error("unterminated fenced block in design ledger");
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
	timeoutSignal?: (timeoutMs: number) => AbortSignal;
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
		// An empty frontier is never legitimate here, and both counters agree on
		// zero if the table header is ever renamed, so the mismatch check alone
		// would let that post as complete.
		if (body.landed.length === 0) {
			throw new Error(
				"ledger yielded no decision rows; refusing to post an empty frontier",
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
