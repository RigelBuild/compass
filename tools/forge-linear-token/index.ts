// Mint a fresh Linear app token for each run; stored tokens expire without a
// refresh token. Mask the result before exporting it to later workflow steps.
import { appendFile } from "node:fs/promises";

/** Auth material for the client_credentials grant. Static; no refresh token. */
export interface AppTokenCredentials {
	readonly clientId: string;
	readonly clientSecret: string;
}

/** Linear's OAuth token endpoint. */
export const TOKEN_URL = "https://api.linear.app/oauth/token";
/** The scopes the live-oracle's Linear legs need (read + write + app assignment into TEST). */
export const SCOPES = "read,write,app:assignable";

/** The two custodied client-credential Actions secrets this helper reads. */
export const ENV_CLIENT_ID = "LINEAR_FORGE_CLIENT_ID";
export const ENV_CLIENT_SECRET = "LINEAR_FORGE_CLIENT_SECRET";
/** The env var the forge live-oracle harness reads the minted token from. */
export const OUTPUT_ENV_NAME = "LINEAR_FORGE";

/**
 * Mint an `app`-actor bearer token from the static client id + secret via
 * Linear's client_credentials grant. Returns the raw access token. Pure but for
 * the injected `fetchFn`, so tests drive it without a network. Fails closed
 * fast: a hung token request would otherwise block the job until the runner's
 * timeout. Mirrors the platform linear-auto-done job's mintAppToken (design
 * record linear-auto-done-auth.md / RIG-1087).
 */
export async function mintAppToken(
	creds: AppTokenCredentials,
	fetchFn: typeof fetch = fetch,
): Promise<string> {
	const body = new URLSearchParams({
		grant_type: "client_credentials",
		scope: SCOPES,
	});
	const basic = Buffer.from(`${creds.clientId}:${creds.clientSecret}`).toString(
		"base64",
	);
	const controller = new AbortController();
	// biome-ignore lint/style/noRestrictedGlobals: fetch-abort timeout (aborts a hung token request after 10s), cleared in finally; not a sleep
	const timer = setTimeout(() => controller.abort(), 10_000);
	let res: Response;
	try {
		res = await fetchFn(TOKEN_URL, {
			method: "POST",
			headers: {
				"content-type": "application/x-www-form-urlencoded",
				authorization: `Basic ${basic}`,
			},
			body: body.toString(),
			signal: controller.signal,
		});
	} finally {
		clearTimeout(timer);
	}
	if (!res.ok) {
		const txt = await res.text();
		throw new Error(
			`client_credentials token request failed: ${res.status} ${res.statusText}\n${txt}`,
		);
	}
	const json = (await res.json()) as { access_token?: string };
	const token = json.access_token;
	if (!token) {
		throw new Error(
			`client_credentials request returned no access_token: ${JSON.stringify(json)}`,
		);
	}
	// A token with a newline would corrupt the `NAME=value` GITHUB_ENV line and
	// silently truncate the credential — fail loud instead.
	if (token.includes("\n")) {
		throw new Error("minted access_token contained a newline");
	}
	return token;
}

/** I/O seams so the run wiring is unit-testable without a network or disk. */
export interface MintDeps {
	readonly env: Record<string, string | undefined>;
	readonly fetchFn?: typeof fetch;
	/** Append one line (a trailing newline is added) to the Actions env file. */
	readonly appendGithubEnv: (line: string) => Promise<void> | void;
	/** Emit a workflow-command / diagnostic line to stdout. */
	readonly log: (msg: string) => void;
}

/**
 * Read the client credentials, mint the token, mask it out of the logs, and
 * export it as LINEAR_FORGE for the steps that follow. Returns a process exit
 * code; never throws for an operational failure (a missing credential, a mint
 * error), turning each into a loud exit-1 instead.
 */
export async function runMint(deps: MintDeps): Promise<number> {
	const clientId = deps.env[ENV_CLIENT_ID];
	const clientSecret = deps.env[ENV_CLIENT_SECRET];
	if (!clientId || !clientSecret) {
		const missing = [
			clientId ? undefined : ENV_CLIENT_ID,
			clientSecret ? undefined : ENV_CLIENT_SECRET,
		].filter((n): n is string => n !== undefined);
		deps.log(
			`::error::forge-linear-token: ${missing.join(" and ")} unset — cannot mint the Linear app token`,
		);
		return 1;
	}

	let token: string;
	try {
		token = await mintAppToken({ clientId, clientSecret }, deps.fetchFn);
	} catch (error) {
		deps.log(
			`::error::forge-linear-token: minting the Linear app token failed: ${error instanceof Error ? error.message : String(error)}`,
		);
		return 1;
	}

	// Mask BEFORE exporting so the token never lands unmasked in any later step's
	// log, then hand it to the suite as LINEAR_FORGE.
	deps.log(`::add-mask::${token}`);
	try {
		await deps.appendGithubEnv(`${OUTPUT_ENV_NAME}=${token}`);
	} catch (error) {
		deps.log(
			`::error::forge-linear-token: writing ${OUTPUT_ENV_NAME} to the env file failed: ${error instanceof Error ? error.message : String(error)}`,
		);
		return 1;
	}
	deps.log(
		`forge-linear-token: minted a fresh Linear app-actor token and exported it as ${OUTPUT_ENV_NAME}`,
	);
	return 0;
}

/**
 * Resolve the Actions env file the minted token is appended to. Returns the
 * GITHUB_ENV path, or exit code 1 (after logging) when it is unset — this tool
 * only runs inside GitHub Actions, where GITHUB_ENV is always set, so an unset
 * value means it was invoked outside that context and must fail loud.
 */
export function resolveGithubEnv(
	githubEnv: string | undefined,
	log: (msg: string) => void,
): string | number {
	if (!githubEnv) {
		log(
			"::error::forge-linear-token: GITHUB_ENV is unset — this tool runs inside GitHub Actions",
		);
		return 1;
	}
	return githubEnv;
}

if (import.meta.main) {
	const log = (msg: string) => {
		console.log(msg);
	};
	const githubEnv = resolveGithubEnv(process.env.GITHUB_ENV, log);
	if (typeof githubEnv === "number") {
		process.exit(githubEnv);
	}
	const code = await runMint({
		env: process.env,
		appendGithubEnv: (line) => appendFile(githubEnv, `${line}\n`),
		log,
	});
	process.exit(code);
}
