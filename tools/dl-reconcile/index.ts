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
type HtmlBlock = { end: RegExp | null; blankTerminated: boolean };

const HTML_BLOCK_TAGS = [
	"address",
	"article",
	"aside",
	"base",
	"basefont",
	"blockquote",
	"body",
	"caption",
	"center",
	"col",
	"colgroup",
	"dd",
	"details",
	"dialog",
	"dir",
	"div",
	"dl",
	"dt",
	"fieldset",
	"figcaption",
	"figure",
	"footer",
	"form",
	"frame",
	"frameset",
	"h[1-6]",
	"head",
	"header",
	"hr",
	"html",
	"iframe",
	"legend",
	"li",
	"link",
	"main",
	"menu",
	"menuitem",
	"meta",
	"nav",
	"noframes",
	"ol",
	"optgroup",
	"option",
	"p",
	"param",
	"search",
	"section",
	"summary",
	"table",
	"tbody",
	"td",
	"tfoot",
	"th",
	"thead",
	"title",
	"tr",
	"track",
	"ul",
].join("|");

const HTML_BLOCKS: {
	start: RegExp;
	end: RegExp | null;
	blankTerminated: boolean;
}[] = [
	{
		start: /^ {0,3}<(?:script|pre|style|textarea)(?:\s|>|\/)/i,
		end: /<\/(?:script|pre|style|textarea)>/i,
		blankTerminated: false,
	},
	{ start: /^ {0,3}<!--/, end: /-->/, blankTerminated: false },
	{ start: /^ {0,3}<\?/, end: /\?>/, blankTerminated: false },
	{ start: /^ {0,3}<![A-Z]/, end: />/, blankTerminated: false },
	{ start: /^ {0,3}<!\[CDATA\[/, end: /\]\]>/, blankTerminated: false },
	{
		start: new RegExp(`^ {0,3}</(?:${HTML_BLOCK_TAGS})(?:\\s|/?>)`, "i"),
		end: null,
		blankTerminated: true,
	},
	{
		start: new RegExp(`^ {0,3}<(?:${HTML_BLOCK_TAGS})(?:\\s|/?>)`, "i"),
		end: null,
		blankTerminated: true,
	},
];

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

/**
 * Classify every line once, resolving fenced code blocks and HTML comment
 * blocks, and blank out whatever is not real document content.
 *
 * Blanked, never dropped: deleting a line splices its neighbours into
 * contiguity, which is how a parked row joins a live table run the renderer
 * would have broken. Mapping over the lines keeps that one-for-one by
 * construction, and a blank can match no header, table line, or DL row.
 *
 * Both ledger counters read this one pass, so neither can model a markdown
 * construct the other misses. The POST is an authoritative full snapshot, so
 * an unterminated block throws here, before either counter reads a line.
 */
function isLedgerRowLine(line: string): boolean {
	return /^ {0,3}\|\s*DL-\d+\s*\|/.test(line);
}

function classifyHtmlLine(
	line: string,
	index: number,
	html: HtmlBlock | null,
): {
	html: HtmlBlock | null;
	content: string;
} | null {
	if (html === null) return null;
	if (isLedgerRowLine(line))
		throw new Error(`ledger row on line ${index + 1} is inside an HTML block`);
	if (html.end?.test(line) || (html.blankTerminated && line.trim() === ""))
		return { html: null, content: "" };
	return { html, content: "" };
}

function findHtmlBlock(line: string): HtmlBlock | null {
	const block = HTML_BLOCKS.find(({ start }) => start.test(line));
	return block
		? { end: block.end, blankTerminated: block.blankTerminated }
		: null;
}

function classifyHtmlStart(
	line: string,
	index: number,
	fence: Fence | null,
): {
	html: HtmlBlock | null;
	content: string;
} | null {
	if (fence !== null || /^ {0,3}<!--/.test(line)) return null;
	const html = findHtmlBlock(line);
	if (html === null) return null;
	if (isLedgerRowLine(line))
		throw new Error(`ledger row on line ${index + 1} is inside an HTML block`);
	return { html: html.end?.test(line) ? null : html, content: "" };
}

type LineState = {
	fence: Fence | null;
	html: HtmlBlock | null;
	inComment: boolean;
	listItem: boolean;
	barePipeInterrupted: boolean;
};

function classifyLine(
	line: string,
	index: number,
	lines: string[],
	state: LineState,
): string {
	if (state.inComment) {
		if (line.includes("-->")) state.inComment = false;
		return "";
	}
	if (state.fence === null && /^ {0,3}<!--/.test(line)) {
		state.inComment = !line.includes("-->");
		return "";
	}
	const existingHtml = classifyHtmlLine(line, index, state.html);
	if (existingHtml !== null) {
		state.html = existingHtml.html;
		return existingHtml.content;
	}
	const htmlStart = classifyHtmlStart(line, index, state.fence);
	if (htmlStart !== null) {
		state.html = htmlStart.html;
		return htmlStart.content;
	}
	const updated = updateFence(line, state.fence);
	state.fence = updated.fence;
	if (updated.handled || state.fence !== null) return "";
	if (isLedgerRowLine(line))
		return classifyLedgerRow(line, index, lines, state);
	if (line.trim() === "|") state.barePipeInterrupted = true;
	if (line.trim() === "") {
		state.listItem = false;
		state.barePipeInterrupted = false;
	} else if (
		/^ {0,3}(?:(?:\*\s*){3,}|(?:-\s*){3,}|(?:_\s*){3,})$/.test(line) ||
		/^ {0,3}#{1,6}(?:\s|$)/.test(line)
	)
		state.listItem = false;
	else if (/^ {0,3}(?:[-+*]|\d+[.)])\s/.test(line)) state.listItem = true;
	return line;
}

function classifyLedgerRow(
	line: string,
	index: number,
	lines: string[],
	state: LineState,
): string {
	if (lines[index - 1]?.trim() === "|")
		throw new Error(
			`ledger row on line ${index + 1} follows an interrupted table`,
		);
	if (state.listItem)
		throw new Error(
			`ledger row on line ${index + 1} is absorbed by a list item`,
		);
	if (state.barePipeInterrupted)
		throw new Error(
			`ledger row on line ${index + 1} follows an interrupted table`,
		);
	return line;
}

function blankNonContentLines(text: string): string[] {
	const state: LineState = {
		fence: null,
		html: null,
		inComment: false,
		listItem: false,
		barePipeInterrupted: false,
	};
	const lines = text.split("\n");
	const classified = lines.map((line, index) =>
		classifyLine(line, index, lines, state),
	);
	if (state.fence !== null)
		throw new Error("unterminated fenced block in design ledger");
	if (state.inComment)
		throw new Error("unterminated HTML comment in design ledger");
	return classified;
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
	let tableState: "none" | "header" | "table" = "none";
	for (const line of blankNonContentLines(text)) {
		if (tableState === "header" && isLedgerSeparator(line)) {
			tableState = "table";
			continue;
		}
		if (tableState === "table" && isTableLine(line)) {
			callback(line);
			continue;
		}
		// Any other line ends the run; only an exact header re-anchors one.
		tableState = isLedgerHeader(line) ? "header" : "none";
	}
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
 * Count ledger-shaped rows independently of the parser's table anchor, so an
 * anchor misread surfaces as a mismatch. An unfenced non-ledger table
 * beginning with a DL ID reads high.
 */
export function countRawLedgerRows(text: string): number {
	let count = 0;
	for (const line of blankNonContentLines(text)) {
		if (/^ {0,3}\|\s*DL-\d+\s*\|/.test(line)) count++;
	}
	return count;
}

export function buildRequestBody(ledger: string): ReconcileRequest {
	return { repo: "compass", landed: parseLedger(ledger) };
}

export function assertReconcilableLedger(ledger: string): ReconcileRequest {
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
	return body;
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
		const body = assertReconcilableLedger(ledger);
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
