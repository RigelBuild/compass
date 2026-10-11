import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import type { ParsedRef, RefLinkRule, RefToken } from "./grammar";
import { parseRef, refUrl, tokenizeRefs } from "./grammar";

interface AcceptedCase {
	readonly input: string;
	readonly canonical: string;
	readonly kind: "tracker" | "forge";
	readonly key?: string;
	readonly owner?: string;
	readonly repo?: string;
	readonly number: number;
}

interface ScanSpan {
	readonly start: number;
	readonly end: number;
	readonly ref: string;
}

interface ScanCase {
	readonly text: string;
	readonly spans: readonly ScanSpan[];
}

interface Fixture {
	readonly accepted: readonly AcceptedCase[];
	readonly rejected: readonly string[];
	readonly scan: readonly ScanCase[];
}

function isRecord(value: unknown): value is Record<string, unknown> {
	return typeof value === "object" && value !== null && !Array.isArray(value);
}

function isFixture(value: unknown): value is Fixture {
	if (!isRecord(value)) return false;
	const accepted = value.accepted;
	const rejected = value.rejected;
	const scan = value.scan;
	return (
		Array.isArray(accepted) &&
		accepted.every(
			(entry) =>
				isRecord(entry) &&
				typeof entry.input === "string" &&
				typeof entry.canonical === "string" &&
				(entry.kind === "tracker" || entry.kind === "forge") &&
				typeof entry.number === "number" &&
				(entry.key === undefined || typeof entry.key === "string") &&
				(entry.owner === undefined || typeof entry.owner === "string") &&
				(entry.repo === undefined || typeof entry.repo === "string"),
		) &&
		Array.isArray(rejected) &&
		rejected.every((entry) => typeof entry === "string") &&
		Array.isArray(scan) &&
		scan.every(
			(entry) =>
				isRecord(entry) &&
				typeof entry.text === "string" &&
				Array.isArray(entry.spans) &&
				entry.spans.every(
					(span) =>
						isRecord(span) &&
						typeof span.start === "number" &&
						typeof span.end === "number" &&
						typeof span.ref === "string",
				),
		)
	);
}

const parsedFixture: unknown = JSON.parse(
	readFileSync(
		join(import.meta.dir, "../../../../go/internal/refs/testdata/refs.json"),
		"utf8",
	),
);
if (!isFixture(parsedFixture)) throw new Error("invalid shared refs fixture");
const fixture = parsedFixture;

function parts(ref: ParsedRef): Record<string, string | number> {
	return ref.kind === "tracker"
		? { kind: ref.kind, key: ref.key, number: ref.number }
		: {
				kind: ref.kind,
				owner: ref.owner,
				repo: ref.repo,
				number: ref.number,
			};
}

function scanFromTokens(text: string): ScanSpan[] {
	const spans: ScanSpan[] = [];
	let offset = 0;
	for (const token of tokenizeRefs(text)) {
		if (typeof token === "string") {
			offset += token.length;
			continue;
		}
		spans.push({
			start: offset,
			end: offset + token.text.length,
			ref: token.canonical,
		});
		offset += token.text.length;
	}
	return spans;
}

function firstRef(text: string): RefToken {
	const token = tokenizeRefs(text).find(
		(candidate): candidate is RefToken => typeof candidate !== "string",
	);
	if (!token) throw new Error(`no ref in ${text}`);
	return token;
}

const rules: readonly RefLinkRule[] = [
	{
		kind: "github",
		prefix: "ghe.example",
		urlTemplate: "https://ghe.example/{owner}/{repo}/issues/{number}",
	},
	{
		kind: "github",
		prefix: "github.com",
		urlTemplate: "https://github.com/{owner}/{repo}/issues/{number}",
	},
	{
		kind: "linear",
		prefix: "RIG",
		urlTemplate: "https://linear.app/rigel/issue/{key}-{number}",
	},
];

describe("reference grammar fixture", () => {
	test("parses every accepted case into its canonical form and parts", () => {
		for (const item of fixture.accepted) {
			const parsed = parseRef(item.input);
			expect(parsed, item.input).toBeDefined();
			if (!parsed) continue;
			expect(parsed.canonical).toBe(item.canonical);
			expect(parts(parsed)).toEqual({
				kind: item.kind,
				...(item.key === undefined ? {} : { key: item.key }),
				...(item.owner === undefined ? {} : { owner: item.owner }),
				...(item.repo === undefined ? {} : { repo: item.repo }),
				number: item.number,
			});
		}
	});

	test("rejects every invalid ref", () => {
		for (const input of fixture.rejected) {
			expect(parseRef(input), input).toBeUndefined();
		}
	});

	test("tokenized references reproduce every fixture scan span", () => {
		for (const item of fixture.scan) {
			expect([...scanFromTokens(item.text)], item.text).toEqual([
				...item.spans,
			]);
		}
	});
});

describe("refUrl", () => {
	test("uses github.com before other configured forge hosts", () => {
		expect(refUrl(firstRef("RigelBuild/Compass#2141"), rules)).toBe(
			"https://github.com/rigelbuild/compass/issues/2141",
		);
	});

	test("uses the first GitHub host when github.com is not configured", () => {
		expect(refUrl(firstRef("Owner/Repo#2"), rules.slice(0, 1))).toBe(
			"https://ghe.example/owner/repo/issues/2",
		);
	});

	test("uses the exact matching Linear team key", () => {
		expect(refUrl(firstRef("RIG-7"), rules)).toBe(
			"https://linear.app/rigel/issue/RIG-7",
		);
	});

	test("does not link a key without a configured Linear team", () => {
		expect(refUrl(firstRef("OPS-7"), rules)).toBeUndefined();
	});
});
