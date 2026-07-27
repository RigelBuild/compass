import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import * as fs from "node:fs/promises";
import * as os from "node:os";
import * as path from "node:path";
import { AuthStorage, SqliteAuthCredentialStore } from "@oh-my-pi/pi-ai";
import { type AuthBrokerServerHandle, startAuthBroker } from "@oh-my-pi/pi-ai/auth-broker";
import type { UsageReport } from "@oh-my-pi/pi-ai/usage";
import { removeWithRetries } from "../../utils/src/temp";

const MASTER = "master-bearer";
const SCRAPE = "scrape-token";

function sampleReports(): UsageReport[] {
	return [
		{
			provider: "openai-codex",
			fetchedAt: 1_700_000_000_000,
			metadata: { accountId: "acct-1" },
			resetCredits: { availableCount: 2 },
			limits: [
				{
					id: "openai-codex:primary",
					label: "5 Hour",
					scope: { provider: "openai-codex", windowId: "5h" },
					window: { id: "5h", label: "5 Hour", resetsAt: 1_700_000_900_000 },
					amount: { usedFraction: 0.25, unit: "percent" },
					status: "ok",
				},
			],
		},
	];
}

describe("auth-broker GET /metrics route", () => {
	let tempDir: string | undefined;
	let store: SqliteAuthCredentialStore | undefined;
	let storage: AuthStorage | undefined;
	let handle: AuthBrokerServerHandle | undefined;
	let usageImpl: (signal?: AbortSignal) => Promise<UsageReport[] | null> = async () => sampleReports();

	beforeEach(async () => {
		tempDir = await fs.mkdtemp(path.join(os.tmpdir(), "auth-broker-metrics-"));
		store = await SqliteAuthCredentialStore.open(path.join(tempDir, "agent.db"));
		storage = new AuthStorage(store, { fetchUsageReports: signal => usageImpl(signal) });
		await storage.reload();
		handle = startAuthBroker({
			storage,
			bind: "127.0.0.1:0",
			bearerTokens: [MASTER],
			metricsTokens: [SCRAPE],
			disableRefresher: true,
		});
	});

	afterEach(async () => {
		await handle?.close();
		storage?.close();
		store?.close();
		if (tempDir) await removeWithRetries(tempDir);
		usageImpl = async () => sampleReports();
	});

	test("scrape token yields the Prometheus exposition", async () => {
		const res = await fetch(`${handle!.url}/metrics`, { headers: { authorization: `Bearer ${SCRAPE}` } });
		expect(res.status).toBe(200);
		expect(res.headers.get("content-type")).toBe("text/plain; version=0.0.4; charset=utf-8");
		const body = await res.text();
		expect(body).toContain(
			'llm_usage_limit_used_fraction{provider="openai-codex",account="acct-1",email="",limit_id="openai-codex:primary",window="5h"} 0.25',
		);
		expect(body).toContain('llm_usage_reset_credits_available{provider="openai-codex",account="acct-1",email=""} 2');
	});

	test("master bearer also satisfies /metrics", async () => {
		const res = await fetch(`${handle!.url}/metrics`, { headers: { authorization: `Bearer ${MASTER}` } });
		expect(res.status).toBe(200);
	});

	test("no token and an unknown token are both 401", async () => {
		expect((await fetch(`${handle!.url}/metrics`)).status).toBe(401);
		const bad = await fetch(`${handle!.url}/metrics`, { headers: { authorization: "Bearer nope" } });
		expect(bad.status).toBe(401);
	});

	test("scrape token is least-privilege: it cannot reach the vault", async () => {
		const res = await fetch(`${handle!.url}/v1/snapshot`, { headers: { authorization: `Bearer ${SCRAPE}` } });
		expect(res.status).toBe(401);
	});

	test("null usage renders an empty 200 exposition, not a 5xx", async () => {
		usageImpl = async () => null;
		const res = await fetch(`${handle!.url}/metrics`, { headers: { authorization: `Bearer ${SCRAPE}` } });
		expect(res.status).toBe(200);
		expect(await res.text()).toBe("");
	});

	test("a storage-level throw surfaces as 503", async () => {
		usageImpl = async () => {
			throw new Error("storage exploded");
		};
		const res = await fetch(`${handle!.url}/metrics`, { headers: { authorization: `Bearer ${SCRAPE}` } });
		expect(res.status).toBe(503);
		expect(await res.text()).toBe("");
	});
});
