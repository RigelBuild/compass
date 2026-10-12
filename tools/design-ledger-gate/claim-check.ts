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

/** Delta-seconds or an HTTP-date; anything else falls back to 60 s. */
function readRetryAfter(response: Response, nowMs: number): number {
	const header = response.headers.get("retry-after")?.trim();
	if (header === undefined || header === "") return 60_000;
	if (/^\d+$/.test(header)) return Number(header) * 1_000;
	const at = Date.parse(header);
	return Number.isNaN(at) ? 60_000 : Math.max(0, at - nowMs);
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
