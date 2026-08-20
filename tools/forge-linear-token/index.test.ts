// Unit tests for forge-linear-token (index.ts) — the per-run Linear app-token
// mint that feeds the forge live-oracle (RIG-2423).
//
// This helper hands a required CI check its Linear credential, so the suite
// defends the operational contract that makes a missing/broken credential a
// LOUD failure rather than a silent one: the client_credentials request shape,
// that a minted token is masked BEFORE it is exported, and that every failure
// mode (unset creds, a non-ok mint, an unwritable env file) exits non-zero
// instead of exporting an empty or partial LINEAR_FORGE.

import { describe, expect, test } from "bun:test";
import {
	type AppTokenCredentials,
	ENV_CLIENT_ID,
	ENV_CLIENT_SECRET,
	type MintDeps,
	mintAppToken,
	OUTPUT_ENV_NAME,
	runMint,
	SCOPES,
	TOKEN_URL,
} from "./index.ts";

const CREDS: AppTokenCredentials = {
	clientId: "cid-123",
	clientSecret: "csecret-456",
};

/** A fetch stub returning one canned Response and recording the call. */
function stubFetch(res: Response): {
	fetchFn: typeof fetch;
	calls: { url: string; init: RequestInit | undefined }[];
} {
	const calls: { url: string; init: RequestInit | undefined }[] = [];
	const fetchFn = (async (url: unknown, init?: RequestInit) => {
		calls.push({ url: String(url), init });
		return res;
	}) as typeof fetch;
	return { fetchFn, calls };
}

function jsonResponse(body: unknown, status = 200): Response {
	return new Response(JSON.stringify(body), {
		status,
		headers: { "content-type": "application/json" },
	});
}

// ---------------------------------------------------------------------------
// mintAppToken — the client_credentials request shape + token extraction.
// ---------------------------------------------------------------------------

describe("mintAppToken", () => {
	test("posts client_credentials with basic auth and returns the access_token", async () => {
		const { fetchFn, calls } = stubFetch(
			jsonResponse({ access_token: "app-token-xyz" }),
		);
		const token = await mintAppToken(CREDS, fetchFn);
		expect(token).toBe("app-token-xyz");

		expect(calls).toHaveLength(1);
		const call = calls[0];
		expect(call?.url).toBe(TOKEN_URL);
		expect(call?.init?.method).toBe("POST");

		const headers = call?.init?.headers as Record<string, string>;
		expect(headers.authorization).toBe(
			`Basic ${Buffer.from("cid-123:csecret-456").toString("base64")}`,
		);
		expect(headers["content-type"]).toBe("application/x-www-form-urlencoded");

		const params = new URLSearchParams(String(call?.init?.body));
		expect(params.get("grant_type")).toBe("client_credentials");
		expect(params.get("scope")).toBe(SCOPES);
	});

	test("throws on a non-ok token response (never returns a bad token)", async () => {
		const { fetchFn } = stubFetch(
			new Response("Remove the Bearer", { status: 400 }),
		);
		await expect(mintAppToken(CREDS, fetchFn)).rejects.toThrow(
			/token request failed: 400/,
		);
	});

	test("throws when the response carries no access_token", async () => {
		const { fetchFn } = stubFetch(jsonResponse({ token_type: "Bearer" }));
		await expect(mintAppToken(CREDS, fetchFn)).rejects.toThrow(
			/no access_token/,
		);
	});

	test("throws when the minted token contains a newline (would corrupt GITHUB_ENV)", async () => {
		const { fetchFn } = stubFetch(
			jsonResponse({ access_token: "tok\ninjected=evil" }),
		);
		await expect(mintAppToken(CREDS, fetchFn)).rejects.toThrow(/newline/);
	});
});

// ---------------------------------------------------------------------------
// runMint — the export wiring: mask-before-export, and loud failure modes.
// ---------------------------------------------------------------------------

/** Capture the log lines + env-file writes a runMint call produces. */
function harness(
	env: Record<string, string | undefined>,
	fetchFn?: typeof fetch,
): { deps: MintDeps; logs: string[]; written: string[] } {
	const logs: string[] = [];
	const written: string[] = [];
	const deps: MintDeps = {
		env,
		fetchFn,
		appendGithubEnv: (line) => {
			written.push(line);
		},
		log: (msg) => {
			logs.push(msg);
		},
	};
	return { deps, logs, written };
}

describe("runMint", () => {
	test("mints, masks the token BEFORE exporting, then writes LINEAR_FORGE", async () => {
		const { fetchFn } = stubFetch(jsonResponse({ access_token: "minted-tok" }));
		const { deps, logs, written } = harness(
			{ [ENV_CLIENT_ID]: "cid", [ENV_CLIENT_SECRET]: "csecret" },
			fetchFn,
		);
		const code = await runMint(deps);
		expect(code).toBe(0);

		// Exactly one export, under the name the harness reads.
		expect(written).toEqual([`${OUTPUT_ENV_NAME}=minted-tok`]);

		// The mask MUST be emitted, and BEFORE the token could reach the env
		// file — a later step must never see it unmasked.
		const maskIdx = logs.indexOf("::add-mask::minted-tok");
		expect(maskIdx).toBeGreaterThanOrEqual(0);
	});

	test("masks before it exports (ordering is the whole point)", async () => {
		// Interleave the two effects on one timeline: the mask log must precede
		// the env-file write.
		const events: string[] = [];
		const { fetchFn } = stubFetch(
			jsonResponse({ access_token: "ordered-tok" }),
		);
		const deps: MintDeps = {
			env: { [ENV_CLIENT_ID]: "cid", [ENV_CLIENT_SECRET]: "csecret" },
			fetchFn,
			appendGithubEnv: () => {
				events.push("write");
			},
			log: (msg) => {
				if (msg.startsWith("::add-mask::")) {
					events.push("mask");
				}
			},
		};
		await runMint(deps);
		expect(events).toEqual(["mask", "write"]);
	});

	test("exits 1 and exports nothing when a credential is unset", async () => {
		const { deps, logs, written } = harness({
			[ENV_CLIENT_ID]: "cid",
			// secret unset
		});
		const code = await runMint(deps);
		expect(code).toBe(1);
		expect(written).toEqual([]);
		expect(logs.some((l) => l.includes(ENV_CLIENT_SECRET))).toBe(true);
	});

	test("exits 1 and exports nothing when the mint request fails", async () => {
		const { fetchFn } = stubFetch(new Response("nope", { status: 401 }));
		const { deps, written } = harness(
			{ [ENV_CLIENT_ID]: "cid", [ENV_CLIENT_SECRET]: "csecret" },
			fetchFn,
		);
		const code = await runMint(deps);
		expect(code).toBe(1);
		expect(written).toEqual([]);
	});

	test("exits 1 when the env-file write fails (after masking)", async () => {
		const { fetchFn } = stubFetch(jsonResponse({ access_token: "tok" }));
		const logs: string[] = [];
		const deps: MintDeps = {
			env: { [ENV_CLIENT_ID]: "cid", [ENV_CLIENT_SECRET]: "csecret" },
			fetchFn,
			appendGithubEnv: () => {
				throw new Error("disk full");
			},
			log: (msg) => {
				logs.push(msg);
			},
		};
		const code = await runMint(deps);
		expect(code).toBe(1);
		// Masked before the failed write, so the token never leaks even on error.
		expect(logs).toContain("::add-mask::tok");
	});
});
