export interface RefLinkRule {
	readonly kind: "github" | "linear";
	readonly prefix: string;
	readonly urlTemplate: string;
}

export type ParsedRef =
	| {
			readonly kind: "tracker";
			readonly key: string;
			readonly number: number;
			readonly canonical: string;
	  }
	| {
			readonly kind: "forge";
			readonly owner: string;
			readonly repo: string;
			readonly number: number;
			readonly canonical: string;
	  };

export type RefToken = ParsedRef & { readonly text: string };

export const MaxNumber = Number.MAX_SAFE_INTEGER;
const MAX_KEY_LEN = 10;

function isUpper(char: string): boolean {
	return char >= "A" && char <= "Z";
}

function isDigit(char: string): boolean {
	return char >= "0" && char <= "9";
}

function isWord(char: string): boolean {
	return (
		isDigit(char) ||
		isUpper(char) ||
		(char >= "a" && char <= "z") ||
		char === "_"
	);
}

function isNameByte(char: string): boolean {
	return isWord(char) || char === "." || char === "-";
}

function isSpace(char: string): boolean {
	return char === " " || char === "\t" || char === "\n" || char === "\r";
}

function blocksStart(char: string): boolean {
	return isNameByte(char) || char === "/" || char === "#";
}

function blocksEnd(char: string): boolean {
	return isWord(char) || char === "/" || char === "#" || char === "-";
}

function run(
	text: string,
	start: number,
	accept: (char: string) => boolean,
): number {
	let end = start;
	while (end < text.length && accept(text[end] ?? "")) end++;
	return end;
}

function parseNumber(
	text: string,
	start: number,
): { number: number; end: number } | undefined {
	const end = run(text, start, isDigit);
	if (end === start) return undefined;
	const number = Number(text.slice(start, end));
	if (!Number.isSafeInteger(number) || number > MaxNumber) return undefined;
	return { number, end };
}

function matchForge(
	text: string,
	start: number,
): { ref: ParsedRef; end: number } | undefined {
	const ownerEnd = run(text, start, isNameByte);
	if (ownerEnd === start || ownerEnd >= text.length || text[ownerEnd] !== "/") {
		return undefined;
	}
	const repoStart = ownerEnd + 1;
	const repoEnd = run(text, repoStart, isNameByte);
	if (
		repoEnd === repoStart ||
		repoEnd >= text.length ||
		text[repoEnd] !== "#"
	) {
		return undefined;
	}
	const parsedNumber = parseNumber(text, repoEnd + 1);
	if (!parsedNumber) return undefined;
	const owner = text.slice(start, ownerEnd).toLowerCase();
	const repo = text.slice(repoStart, repoEnd).toLowerCase();
	return {
		ref: {
			kind: "forge",
			owner,
			repo,
			number: parsedNumber.number,
			canonical: `${owner}/${repo}#${parsedNumber.number}`,
		},
		end: parsedNumber.end,
	};
}

function matchTracker(
	text: string,
	start: number,
): { ref: ParsedRef; end: number } | undefined {
	if (start >= text.length || !isUpper(text[start] ?? "")) return undefined;
	const keyEnd = run(text, start + 1, (char) => isUpper(char) || isDigit(char));
	const keyLength = keyEnd - start;
	if (
		keyLength < 2 ||
		keyLength > MAX_KEY_LEN ||
		keyEnd >= text.length ||
		text[keyEnd] !== "-"
	) {
		return undefined;
	}
	const parsedNumber = parseNumber(text, keyEnd + 1);
	if (!parsedNumber) return undefined;
	const key = text.slice(start, keyEnd);
	return {
		ref: {
			kind: "tracker",
			key,
			number: parsedNumber.number,
			canonical: `${key}-${parsedNumber.number}`,
		},
		end: parsedNumber.end,
	};
}

function matchAt(
	text: string,
	start: number,
): { ref: ParsedRef; end: number } | undefined {
	return matchForge(text, start) ?? matchTracker(text, start);
}

export function parseRef(text: string): ParsedRef | undefined {
	const match = matchAt(text, 0);
	return match?.end === text.length ? match.ref : undefined;
}

interface RefSpan {
	readonly start: number;
	readonly end: number;
	readonly ref: ParsedRef;
}

// A whitespace-delimited token holding "://" is a URL and is skipped whole.
function urlTokenEnd(text: string, start: number): number | undefined {
	if (start > 0 && !isSpace(text[start - 1] ?? "")) return undefined;
	const end = run(text, start, (char) => !isSpace(char));
	return text.slice(start, end).includes("://") ? end : undefined;
}

function scanRefs(text: string): RefSpan[] {
	const spans: RefSpan[] = [];
	for (let start = 0; start < text.length; start++) {
		const urlEnd = urlTokenEnd(text, start);
		if (urlEnd !== undefined) {
			start = urlEnd;
			continue;
		}
		if (start > 0 && blocksStart(text[start - 1] ?? "")) continue;
		const match = matchAt(text, start);
		if (
			!match ||
			(match.end < text.length && blocksEnd(text[match.end] ?? ""))
		) {
			continue;
		}
		spans.push({ start, end: match.end, ref: match.ref });
		start = match.end - 1;
	}
	return spans;
}

export function tokenizeRefs(text: string): Array<string | RefToken> {
	const spans = scanRefs(text);
	if (spans.length === 0) return text ? [text] : [];
	const tokens: Array<string | RefToken> = [];
	let cursor = 0;
	for (const span of spans) {
		if (cursor < span.start) tokens.push(text.slice(cursor, span.start));
		tokens.push({ ...span.ref, text: text.slice(span.start, span.end) });
		cursor = span.end;
	}
	if (cursor < text.length) tokens.push(text.slice(cursor));
	return tokens;
}

export function refUrl(
	token: RefToken,
	rules: readonly RefLinkRule[],
): string | undefined {
	const rule =
		token.kind === "forge"
			? (rules.find(
					(candidate) =>
						candidate.kind === "github" && candidate.prefix === "github.com",
				) ?? rules.find((candidate) => candidate.kind === "github"))
			: rules.find(
					(candidate) =>
						candidate.kind === "linear" && candidate.prefix === token.key,
				);
	if (!rule) return undefined;
	return rule.urlTemplate
		.replaceAll("{owner}", token.kind === "forge" ? token.owner : "")
		.replaceAll("{repo}", token.kind === "forge" ? token.repo : "")
		.replaceAll("{key}", token.kind === "tracker" ? token.key : "")
		.replaceAll("{number}", String(token.number));
}
