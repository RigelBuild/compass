export interface ClaimArgs {
	ref: string;
	lane: string;
	count: number;
}

export interface ClaimRequest {
	repo: "compass";
	surface: "designs";
	ref: string;
	lane: string;
	count: number;
}

export interface ClaimedId {
	id: string;
	date: string;
}

const USAGE =
	"Usage: bun tools/dl-claim --ref <RIG-n|none> --lane <branch> [--count 1..10] [--repo compass]";
const ACCEPTED_FLAGS = ["--ref", "--lane", "--count", "--repo"];

function parseFlag(arg: string): { flag: string; value: string | undefined } {
	const equalIndex = arg.indexOf("=");
	return equalIndex < 0
		? { flag: arg, value: undefined }
		: { flag: arg.slice(0, equalIndex), value: arg.slice(equalIndex + 1) };
}

function readFlagValue(
	argv: string[],
	index: number,
	flag: string,
	inlineValue: string | undefined,
): { value: string; nextIndex: number } {
	const value = inlineValue ?? argv[index + 1];
	if (value === undefined || value.startsWith("--")) {
		throw new Error(`${USAGE}\nMissing value for ${flag}`);
	}
	return { value, nextIndex: inlineValue === undefined ? index + 1 : index };
}

function setClaimArg(
	flag: string,
	value: string,
	state: { ref: string | undefined; lane: string | undefined; count: number },
): void {
	switch (flag) {
		case "--ref":
			state.ref = value;
			break;
		case "--lane":
			state.lane = value;
			break;
		case "--count":
			if (!/^\d+$/.test(value))
				throw new Error(`${USAGE}\nInvalid count: ${value}`);
			state.count = Number(value);
			break;
		case "--repo":
			if (value !== "compass") {
				throw new Error("This tool claims only the compass partition");
			}
			break;
	}
}

export function parseArgs(argv: string[]): ClaimArgs {
	const state: {
		ref: string | undefined;
		lane: string | undefined;
		count: number;
	} = {
		ref: undefined,
		lane: undefined,
		count: 1,
	};
	for (let index = 0; index < argv.length; index += 1) {
		const arg = argv[index] ?? "";
		const { flag, value: inlineValue } = parseFlag(arg);
		if (!ACCEPTED_FLAGS.includes(flag))
			throw new Error(`${USAGE}\nUnknown flag: ${arg}`);
		const parsed = readFlagValue(argv, index, flag, inlineValue);
		index = parsed.nextIndex;
		setClaimArg(flag, parsed.value, state);
	}
	const { ref, lane, count } = state;
	if (ref === undefined) throw new Error(`${USAGE}\n--ref is required`);
	if (!/^(RIG-\d+|none)$/.test(ref)) {
		throw new Error(`${USAGE}\nInvalid ref: ${ref}`);
	}
	if (lane === undefined || lane.trim().length === 0) {
		throw new Error(`${USAGE}\n--lane must be non-empty`);
	}
	if (!Number.isInteger(count) || count < 1 || count > 10) {
		throw new Error(`${USAGE}\nCount must be an integer from 1 to 10`);
	}
	return { ref, lane, count };
}

export function buildClaimBody(args: ClaimArgs): ClaimRequest {
	return { repo: "compass", surface: "designs", ...args };
}

/** The seam injects only the request call used by the CLI. */
type FetchFn = (
	input: string | URL | Request,
	init?: RequestInit,
) => Promise<Response>;

export interface ClaimDeps {
	fetchFn?: FetchFn;
	timeoutMs?: number;
	timeoutSignal?: (timeoutMs: number) => AbortSignal;
}

async function readServiceError(
	response: Response,
): Promise<string | undefined> {
	try {
		const payload: unknown = await response.json();
		return typeof payload === "object" &&
			payload !== null &&
			"error" in payload &&
			typeof payload.error === "string"
			? payload.error
			: undefined;
	} catch {
		return undefined;
	}
}

// 400/401/429 are rejected before the counter advances; any other failure
// after sending may have consumed ids, so a blind rerun burns more.
const MAYBE_MINTED =
	" Ids may already be consumed; do not rerun. Find this lane's claims with `curl -H \"Authorization: Bearer $DL_CLAIM_TOKEN\" 'https://dl.rigel.build/status?repo=compass'`.";

function formatServiceError(status: number, code: string | undefined): Error {
	const hint =
		status === 401
			? " Token missing or wrong."
			: status === 429
				? " Rate limited; retry later."
				: status === 503 && code === "not_hydrated"
					? " The reconcile workflow has not hydrated the compass partition yet."
					: status === 400
						? ""
						: MAYBE_MINTED;
	return new Error(
		`claim failed with HTTP ${status}${code ? ` (${code})` : ""}.${hint}`,
	);
}

function isClaimedId(value: unknown): value is ClaimedId {
	return (
		typeof value === "object" &&
		value !== null &&
		"id" in value &&
		typeof value.id === "string" &&
		/^DL-\d{3,}$/.test(value.id) &&
		"date" in value &&
		typeof value.date === "string" &&
		/^\d{4}-\d{2}-\d{2}$/.test(value.date)
	);
}

async function readClaimedIds(
	response: Response,
	count: number,
): Promise<ClaimedId[]> {
	let payload: unknown;
	try {
		payload = await response.json();
	} catch {
		throw new Error(`claim response is not valid JSON.${MAYBE_MINTED}`);
	}
	const ids =
		typeof payload === "object" &&
		payload !== null &&
		"ids" in payload &&
		Array.isArray(payload.ids)
			? payload.ids
			: undefined;
	const returned = (ids ?? [])
		.map((entry: unknown) =>
			typeof entry === "object" && entry !== null && "id" in entry
				? String(entry.id)
				: "",
		)
		.filter((id: string) => /^DL-\d+$/.test(id));
	const seen = returned.length > 0 ? ` Returned: ${returned.join(", ")}.` : "";
	if (ids === undefined || ids.length !== count) {
		throw new Error(
			`claim response must contain the requested number of ids.${seen}${MAYBE_MINTED}`,
		);
	}
	if (!ids.every(isClaimedId)) {
		throw new Error(
			`claim response contains a malformed id.${seen}${MAYBE_MINTED}`,
		);
	}
	return ids;
}

export async function claim(
	body: ClaimRequest,
	token: string,
	deps: ClaimDeps = {},
): Promise<ClaimedId[]> {
	const trimmedToken = token.trim();
	if (trimmedToken.length === 0) throw new Error("DL_CLAIM_TOKEN is required");
	const timeoutMs =
		deps.timeoutMs !== undefined && deps.timeoutMs > 0
			? deps.timeoutMs
			: 30_000;
	let response: Response;
	try {
		response = await (deps.fetchFn ?? fetch)("https://dl.rigel.build/claim", {
			method: "POST",
			redirect: "error",
			headers: {
				Authorization: `Bearer ${trimmedToken}`,
				"Content-Type": "application/json",
			},
			body: JSON.stringify(body),
			signal: (deps.timeoutSignal ?? AbortSignal.timeout)(timeoutMs),
		});
	} catch (error) {
		const reason = error instanceof Error ? error.message : String(error);
		throw new Error(`claim request failed: ${reason}.${MAYBE_MINTED}`);
	}
	if (!response.ok) {
		throw formatServiceError(response.status, await readServiceError(response));
	}
	return readClaimedIds(response, body.count);
}

export function formatClaimed(ids: ClaimedId[]): string {
	return ids.map(({ id, date }) => `${id} (claimed ${date})`).join("\n");
}

if (import.meta.main) {
	const argv = process.argv.slice(2);
	if (argv.includes("--help")) {
		console.log(USAGE);
	} else {
		try {
			const args = parseArgs(argv);
			const ids = await claim(
				buildClaimBody(args),
				process.env.DL_CLAIM_TOKEN ?? "",
			);
			console.log(formatClaimed(ids));
		} catch (error) {
			console.error(
				`dl-claim failed: ${error instanceof Error ? error.message : String(error)}`,
			);
			process.exitCode = 1;
		}
	}
}
