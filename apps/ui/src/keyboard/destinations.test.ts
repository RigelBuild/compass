import { describe, expect, test } from "bun:test";
import { createRoot } from "solid-js";
import { STUB_COMMS_STATE } from "../comms-stub";
import { type AppStore, createAppStore } from "../store";
import { testQueryClient } from "../test-support";
import type { Destination, DestinationProvider } from "./commands";
import {
	createStoreDestinationProviders,
	queryDestinations,
} from "./destinations";

// Store-backed destination providers (RIG-2483, A4/T3). The providers map the
// store's live accessors to Destinations of all six kinds; queryDestinations
// groups them with per-provider isolation and a latest-wins generation guard.
// These mount a fixture-backed store inside createRoot (createMemo needs an
// owner) and drive navigation through the store's in-memory route path.

async function withStoreAsync(
	body: (store: AppStore) => Promise<void>,
): Promise<void> {
	let dispose!: () => void;
	const store = createRoot((d) => {
		dispose = d;
		return createAppStore({
			initialComms: STUB_COMMS_STATE,
			queryClient: testQueryClient(),
		});
	});
	try {
		await body(store);
	} finally {
		dispose();
	}
}

/** Drain the microtask queue so an async accessor (assignedIssues) settles. */
async function flush(): Promise<void> {
	for (let i = 0; i < 20; i++) await Promise.resolve();
}

const CURRENT_GEN = () => 1;

describe("createStoreDestinationProviders", () => {
	test("maps all six destination kinds from the fixture store", async () => {
		await withStoreAsync(async (store) => {
			await flush(); // let assignedIssues load through the tracker seam
			const providers = createStoreDestinationProviders(store);
			const byKind = await queryDestinations(providers, "", 1, CURRENT_GEN);
			expect(byKind).not.toBeNull();
			const kinds = byKind as Map<string, Destination[]>;
			for (const kind of ["agent", "channel", "topic", "view", "issue", "pr"]) {
				expect((kinds.get(kind) ?? []).length).toBeGreaterThan(0);
			}
		});
	});

	test("the views provider yields exactly Bridge/Backlog/Done/Settings", async () => {
		await withStoreAsync(async (store) => {
			const providers = createStoreDestinationProviders(store);
			const views = await providers.find((p) => p.id === "views")?.query("");
			expect((views ?? []).map((d) => d.title).sort()).toEqual([
				"Backlog",
				"Bridge",
				"Done",
				"Settings",
			]);
		});
	});

	test("the prs provider derives Pr rows and navigates to the owning issue + PR tab", async () => {
		await withStoreAsync(async (store) => {
			// The prs accessor derives PrRows from fixture issues via prRows(issues()).
			expect(store.prs().length).toBeGreaterThan(0);
			const providers = createStoreDestinationProviders(store);
			const prs =
				(await providers.find((p) => p.id === "prs")?.query("")) ?? [];
			expect(prs.length).toBe(store.prs().length);
			// The pr id is `${repo}#${number}` and navigate selects the owning issue.
			const row = store.prs()[0];
			const dest = prs.find((d) => d.id === `${row.pr.repo}#${row.pr.number}`);
			expect(dest).toBeDefined();
			dest?.navigate();
			await flush();
			expect(store.selectedIssueId()).toBe(row.issue.id);
			expect(store.activeRightTab()).toBe("pr");
		});
	});

	test("the agents provider navigates via the store's in-memory route path", async () => {
		await withStoreAsync(async (store) => {
			const providers = createStoreDestinationProviders(store);
			const agents =
				(await providers.find((p) => p.id === "agents")?.query("")) ?? [];
			expect(agents.length).toBeGreaterThan(0);
			agents[0].navigate();
			await flush();
			expect(store.view()).toBe("agent");
		});
	});

	test("a query filters each provider's rows by fuzzy match", async () => {
		await withStoreAsync(async (store) => {
			const providers = createStoreDestinationProviders(store);
			const views =
				(await providers.find((p) => p.id === "views")?.query("sett")) ?? [];
			expect(views.map((d) => d.title)).toEqual(["Settings"]);
		});
	});
});

describe("queryDestinations", () => {
	test("per-provider isolation: a rejected provider drops only its group", async () => {
		const good: DestinationProvider = {
			id: "good",
			query: () =>
				Promise.resolve([
					{ id: "a", title: "Alpha", kind: "agent", navigate: () => {} },
				]),
		};
		const bad: DestinationProvider = {
			id: "bad",
			query: () => Promise.reject(new Error("provider blew up")),
		};
		const byKind = await queryDestinations([good, bad], "", 1, CURRENT_GEN);
		expect(byKind).not.toBeNull();
		const kinds = byKind as Map<string, Destination[]>;
		expect((kinds.get("agent") ?? []).map((d) => d.id)).toEqual(["a"]);
		// The rejected provider contributed no group; the surface still resolved.
		expect(kinds.size).toBe(1);
	});

	test("latest-wins: a stale-generation resolution applies nothing (returns null)", async () => {
		let release!: (v: Destination[]) => void;
		const pending: DestinationProvider = {
			id: "slow",
			query: () =>
				new Promise<Destination[]>((resolve) => {
					release = resolve;
				}),
		};
		// generation captured at issue is 1, but the counter has since moved to 2.
		const result = queryDestinations([pending], "", 1, () => 2);
		release([{ id: "z", title: "Zed", kind: "agent", navigate: () => {} }]);
		expect(await result).toBeNull();
	});

	test("a current-generation resolution applies its results", async () => {
		let release!: (v: Destination[]) => void;
		const pending: DestinationProvider = {
			id: "slow",
			query: () =>
				new Promise<Destination[]>((resolve) => {
					release = resolve;
				}),
		};
		const result = queryDestinations([pending], "", 5, () => 5);
		release([{ id: "z", title: "Zed", kind: "agent", navigate: () => {} }]);
		const byKind = await result;
		expect(byKind).not.toBeNull();
		expect((byKind as Map<string, Destination[]>).get("agent")?.[0].id).toBe(
			"z",
		);
	});
});
