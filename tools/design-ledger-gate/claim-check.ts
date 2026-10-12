import { $ } from "bun";
import { KEY_LINE, type Violation } from "./index.ts";

export type ClaimState = "lane" | "not-lane";

export interface FetchClaimsDeps {
	fetchFn?: (
		input: string | URL | Request,
		init?: RequestInit,
	) => Promise<Response>;
	sleep?: (milliseconds: number) => Promise<void>;
	/** Monotonic clock in milliseconds. */
	now?: () => number;
}

const CLAIMS_URL = "https://dl.rigel.build";
const MAX_BATCH_SIZE = 100;
const MAX_RATE_LIMIT_WAIT_MS = 65_000;
const RETRY_DELAYS_MS = [1_000, 3_000] as const;

function serviceError(reason: string): Error {
	return new Error(
		`design-ledger-gate: cannot reach the DL claim service at ${CLAIMS_URL} (${reason}). Re-run the pipeline when it answers. Do not hand-pick an id: an unclaimed id fails.`,
	);
}

function rateLimitError(): Error {
	return new Error(
		"design-ledger-gate: the DL claim service rate-limited this runner for 65 s. Re-run the pipeline in a minute.",
	);
}

const responseReason = (status: number) => `HTTP ${status}`;

const MONTHS = "Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec";
const DAYS = ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"];
const LONG_DAYS = [
	"Sunday",
	"Monday",
	"Tuesday",
	"Wednesday",
	"Thursday",
	"Friday",
	"Saturday",
];
/** The three HTTP-date forms of RFC 9110 5.6.7: IMF-fixdate, rfc850, asctime. */
const HTTP_DATE_FORMS = [
	{
		re: new RegExp(
			`^(\\w{3}), (\\d{2}) (${MONTHS}) (\\d{4}) (\\d{2}):(\\d{2}):(\\d{2}) GMT$`,
		),
		order: ["wday", "day", "mon", "year", "h", "m", "s"],
		days: DAYS,
	},
	{
		re: new RegExp(
			`^(\\w+), (\\d{2})-(${MONTHS})-(\\d{2}) (\\d{2}):(\\d{2}):(\\d{2}) GMT$`,
		),
		order: ["wday", "day", "mon", "year", "h", "m", "s"],
		days: LONG_DAYS,
	},
	{
		re: new RegExp(
			`^(\\w{3}) (${MONTHS}) ([ \\d]\\d) (\\d{2}):(\\d{2}):(\\d{2}) (\\d{4})$`,
		),
		order: ["wday", "mon", "day", "h", "m", "s", "year"],
		days: DAYS,
	},
] as const;

interface DateParts {
	wday: string;
	year: number;
	month: number;
	day: number;
	h: number;
	m: number;
	s: number;
}

/** Epoch ms for parts that name a real UTC instant with the right weekday, else null. */
function exactUtc(p: DateParts, days: readonly string[]): number | null {
	// A leap second (:60) is only valid at 23:59; it maps to the next second.
	const leap = p.s === 60 && p.h === 23 && p.m === 59;
	const s = leap ? 59 : p.s;
	const date = new Date(Date.UTC(p.year, p.month, p.day, p.h, p.m, s));
	const exact =
		date.getUTCFullYear() === p.year &&
		date.getUTCMonth() === p.month &&
		date.getUTCDate() === p.day &&
		date.getUTCHours() === p.h &&
		date.getUTCMinutes() === p.m &&
		date.getUTCSeconds() === s &&
		days[date.getUTCDay()] === p.wday;
	return exact ? date.getTime() + (leap ? 1_000 : 0) : null;
}

/** Epoch ms for a valid HTTP-date in UTC, or null; impossible dates are rejected. */
export function parseHttpDate(text: string, nowMs: number): number | null {
	for (const form of HTTP_DATE_FORMS) {
		const match = form.re.exec(text);
		if (match === null) continue;
		const get = (name: (typeof form.order)[number]) =>
			match[form.order.indexOf(name) + 1]?.trim() ?? "";
		const yearText = get("year");
		const parts: DateParts = {
			wday: get("wday"),
			year: Number(yearText),
			month: MONTHS.split("|").indexOf(get("mon")),
			day: Number(get("day")),
			h: Number(get("h")),
			m: Number(get("m")),
			s: Number(get("s")),
		};
		if (yearText.length !== 2) return exactUtc(parts, form.days);
		// RFC 9110: an rfc850 date more than 50 years in the future is in the past century.
		const century = Math.floor(new Date(nowMs).getUTCFullYear() / 100) * 100;
		const cutoff = new Date(nowMs);
		cutoff.setUTCFullYear(cutoff.getUTCFullYear() + 50);
		const sameCentury = { ...parts, year: parts.year + century };
		const provisional = Date.UTC(
			sameCentury.year,
			parts.month,
			parts.day,
			parts.h,
			parts.m,
			Math.min(parts.s, 59),
		);
		const year =
			provisional > cutoff.getTime()
				? sameCentury.year - 100
				: sameCentury.year;
		return exactUtc({ ...parts, year }, form.days);
	}
	return null;
}

/** Delta-seconds or a valid HTTP-date; anything else falls back to 60 s. */
function readRetryAfter(response: Response, nowMs: number): number {
	const header = response.headers.get("retry-after")?.trim() ?? "";
	if (/^\d+$/.test(header)) return Number(header) * 1_000;
	const at = parseHttpDate(header, nowMs);
	return at === null ? 60_000 : Math.max(0, at - nowMs);
}

async function readVerdicts(
	response: Response,
	ids: readonly string[],
): Promise<Map<string, ClaimState>> {
	let payload: unknown;
	try {
		payload = await response.json();
	} catch {
		throw serviceError("malformed response (invalid JSON)");
	}
	if (
		typeof payload !== "object" ||
		payload === null ||
		!("claims" in payload) ||
		!Array.isArray(payload.claims)
	) {
		throw serviceError("malformed response");
	}

	const verdicts = new Map<string, ClaimState>();
	for (const entry of payload.claims) {
		if (
			typeof entry !== "object" ||
			entry === null ||
			!("id" in entry) ||
			typeof entry.id !== "string" ||
			!("state" in entry) ||
			typeof entry.state !== "string" ||
			!ids.includes(entry.id) ||
			verdicts.has(entry.id)
		) {
			throw serviceError("malformed response");
		}
		verdicts.set(entry.id, entry.state === "lane" ? "lane" : "not-lane");
	}

	for (const id of ids) {
		if (!verdicts.has(id)) throw serviceError(`missing ${id}`);
	}
	return verdicts;
}

type BatchDeps = Required<FetchClaimsDeps>;
/** One deadline for all rate-limit handling in a gate run, so 429s cannot spin. */
interface RateLimitState {
	deadline: number;
}

async function fetchResponse(
	lane: string,
	ids: readonly string[],
	fetchFn: BatchDeps["fetchFn"],
): Promise<Response> {
	const url = new URL("/claims", CLAIMS_URL);
	url.searchParams.set("repo", "compass");
	url.searchParams.set("lane", lane);
	url.searchParams.set("ids", ids.join(","));
	return await fetchFn(url, { signal: AbortSignal.timeout(10_000) });
}

async function waitForRateLimit(
	response: Response,
	deps: BatchDeps,
	state: RateLimitState,
): Promise<void> {
	const waitMs = readRetryAfter(response, Date.now());
	if (deps.now() + waitMs >= state.deadline) throw rateLimitError();
	await deps.sleep(waitMs);
}

async function fetchWithRetries(
	lane: string,
	ids: readonly string[],
	deps: BatchDeps,
): Promise<Response> {
	let reason = "no attempt made";
	for (const delayMs of [...RETRY_DELAYS_MS, null]) {
		try {
			const response = await fetchResponse(lane, ids, deps.fetchFn);
			if (response.status < 500) return response;
			reason = responseReason(response.status);
		} catch (error) {
			reason = error instanceof Error ? error.message : String(error);
		}
		if (delayMs !== null) await deps.sleep(delayMs);
	}
	throw serviceError(reason);
}

const fetchBatch = async (
	lane: string,
	ids: readonly string[],
	deps: BatchDeps,
	rateLimitState: RateLimitState,
): Promise<Map<string, ClaimState>> => {
	while (true) {
		const response = await fetchWithRetries(lane, ids, deps);
		if (response.status === 429) {
			await waitForRateLimit(response, deps, rateLimitState);
			continue;
		}
		if (!response.ok) throw serviceError(responseReason(response.status));
		return readVerdicts(response, ids);
	}
};

export async function fetchClaims(
	lane: string,
	ids: readonly string[],
	deps: FetchClaimsDeps = {},
): Promise<Map<string, ClaimState>> {
	const requestDeps: BatchDeps = {
		fetchFn: deps.fetchFn ?? fetch,
		sleep: deps.sleep ?? Bun.sleep,
		now: deps.now ?? (() => performance.now()),
	};
	const verdicts = new Map<string, ClaimState>();
	const rateLimitState: RateLimitState = {
		deadline: requestDeps.now() + MAX_RATE_LIMIT_WAIT_MS,
	};
	for (let offset = 0; offset < ids.length; offset += MAX_BATCH_SIZE) {
		const batch = ids.slice(offset, offset + MAX_BATCH_SIZE);
		const batchVerdicts = await fetchBatch(
			lane,
			batch,
			requestDeps,
			rateLimitState,
		);
		for (const [id, state] of batchVerdicts) verdicts.set(id, state);
	}
	return verdicts;
}

export function claimCheckViolations(
	addedIds: string[],
	verdicts: Map<string, ClaimState>,
	lane: string,
	pathById: Map<string, string>,
): Violation[] {
	const violations: Violation[] = [];
	for (const id of addedIds) {
		if (verdicts.get(id) === "lane") continue;
		const file = pathById.get(id);
		if (file === undefined) continue;
		violations.push({
			file,
			line: KEY_LINE.id,
			message: `${id}: not claimed for lane ${lane}. Claim a fresh id with \`bun tools/dl-claim --ref <RIG-n|none> --lane ${lane}\` and rename this decision file and every citation of it.`,
		});
	}
	return violations;
}

/**
 * DL ids at the merge base of HEAD and origin/main: the main commit the
 * checked-out tree contains, even after origin/main has moved on.
 */
export async function mergeBaseDecisionIds(root: string): Promise<Set<string>> {
	const base = await $`git merge-base HEAD origin/main`
		.cwd(root)
		.nothrow()
		.quiet();
	const listing =
		base.exitCode === 0
			? await $`git ls-tree -r --name-only ${base.stdout.toString().trim()} -- docs/designs/decisions`
					.cwd(root)
					.nothrow()
					.quiet()
			: base;
	if (listing.exitCode !== 0) {
		throw new Error(
			"design-ledger-gate: cannot list decision ids at the merge base with origin/main; origin/main must resolve in this checkout.",
		);
	}
	const ids = new Set<string>();
	for (const path of listing.stdout.toString().split("\n")) {
		const id = /(?:^|\/)(DL-(?:\d{3}|[1-9]\d{3,}))\.md$/.exec(path)?.[1];
		if (id !== undefined) ids.add(id);
	}
	return ids;
}
