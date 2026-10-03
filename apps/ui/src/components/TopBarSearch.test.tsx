// Global search tests use an overridable debounce and event-loop settling.
import { afterEach, beforeEach, describe, expect, spyOn, test } from "bun:test";
import { cleanup, fireEvent } from "@solidjs/testing-library";
import { flush as flushSync } from "solid-js";
import type { Destination, DestinationProvider } from "../keyboard/commands";
import {
	resetSearchDebounceForTest,
	setSearchDebounceMsForTest,
} from "../keyboard/destination-surface";
import * as destinationsModule from "../keyboard/destinations";
import { flush, mountApp } from "../test-router";

beforeEach(() => setSearchDebounceMsForTest(0));
afterEach(() => {
	cleanup();
	resetSearchDebounceForTest();
});

async function settleSearch(): Promise<void> {
	const { promise, resolve } = Promise.withResolvers<void>();
	// biome-ignore lint/style/noRestrictedGlobals: deterministic macrotask yield for debounce observation; not a timed wait
	setTimeout(resolve, 0);
	await promise;
	await flush();
}

const input = (container: HTMLElement) =>
	container.querySelector<HTMLInputElement>(".topbar-search-input");

describe("TopBarSearch", () => {
	test("typing renders grouped destination rows and Enter navigates", async () => {
		const { container, store } = mountApp("/");
		const search = input(container) as HTMLInputElement;
		search.focus();
		fireEvent.input(search, { target: { value: "settings" } });
		await settleSearch();
		expect(container.querySelector(".topbar-search-group")?.textContent).toBe(
			"Views",
		);
		const row =
			container.querySelector<HTMLButtonElement>(".topbar-search-row");
		expect(row?.textContent).toContain("Settings");
		fireEvent.keyDown(search, { key: "Enter" });
		await flush();
		expect(store.view()).toBe("settings");
		expect(search.value).toBe("");
	});

	test("Escape clears and returns focus to the prior element", async () => {
		const { container } = mountApp("/");
		const prior = container.querySelector<HTMLElement>(".topbar .view-tab");
		const search = input(container) as HTMLInputElement;
		prior?.focus();
		search.focus();
		fireEvent.input(search, { target: { value: "backlog" } });
		await settleSearch();
		fireEvent.keyDown(search, { key: "Escape" });
		expect(search.value).toBe("");
		expect(document.activeElement).toBe(prior);
	});
	test("Enter flushes a pending query and selects its matching result", async () => {
		setSearchDebounceMsForTest(500);
		const { container, store } = mountApp("/backlog");
		const search = input(container) as HTMLInputElement;
		fireEvent.input(search, { target: { value: "settings" } });
		flushSync();
		fireEvent.keyDown(search, { key: "Enter" });
		await flush();
		expect(store.view()).toBe("settings");
	});
	test("Enter waits for the current in-flight query before selecting", async () => {
		setSearchDebounceMsForTest(0);
		const pending = new Map<string, (rows: Destination[]) => void>();
		let navigated = "";
		const querySpy = spyOn(
			destinationsModule,
			"createStoreDestinationProviders",
		);
		querySpy.mockImplementation(() => [
			{
				id: "controlled",
				query: (value) => {
					const { promise, resolve } = Promise.withResolvers<Destination[]>();
					pending.set(value, resolve);
					return promise;
				},
			} satisfies DestinationProvider,
		]);
		const { container } = mountApp("/");
		const search = input(container) as HTMLInputElement;
		search.focus();

		fireEvent.input(search, { target: { value: "alpha" } });
		await settleSearch();
		pending.get("alpha")?.([
			{
				id: "alpha",
				title: "Alpha result",
				kind: "agent",
				navigate: () => {
					navigated = "alpha";
				},
			},
		]);
		await flush();

		fireEvent.input(search, { target: { value: "beta" } });
		await settleSearch();
		expect(pending.has("beta")).toBe(true);
		fireEvent.keyDown(search, { key: "Enter" });
		pending.get("beta")?.([
			{
				id: "beta",
				title: "Beta result",
				kind: "agent",
				navigate: () => {
					navigated = "beta";
				},
			},
		]);
		await flush();
		await Promise.resolve();
		expect(navigated).toBe("beta");
		querySpy.mockRestore();
	});

	test("empty Enter does not navigate and blur hides results", async () => {
		const { container, store } = mountApp("/backlog");
		const search = input(container) as HTMLInputElement;
		fireEvent.keyDown(search, { key: "Enter" });
		await flush();
		expect(store.view()).toBe("backlog");
		fireEvent.input(search, { target: { value: "settings" } });
		await settleSearch();
		search.blur();
		await flush();
		expect(container.querySelector(".topbar-search-panel")).toBeNull();
	});

	test("latest query wins when an earlier provider resolves slowly", async () => {
		const pending: Array<(rows: Destination[]) => void> = [];
		const querySpy = spyOn(
			destinationsModule,
			"createStoreDestinationProviders",
		);
		querySpy.mockImplementation(() => [
			{
				id: "slow",
				query: (value) => {
					const { promise, resolve } = Promise.withResolvers<Destination[]>();
					if (value === "old") pending.push(resolve);
					else
						resolve([
							{
								id: "new",
								title: "New result",
								kind: "agent",
								navigate: () => {},
							},
						]);
					return promise;
				},
			} satisfies DestinationProvider,
		]);
		const { container } = mountApp("/");
		const search = input(container) as HTMLInputElement;
		search.focus();
		fireEvent.input(search, { target: { value: "old" } });
		await settleSearch();
		fireEvent.input(search, { target: { value: "new" } });
		await settleSearch();
		expect(pending).toHaveLength(1);
		pending[0]([
			{ id: "old", title: "Old result", kind: "agent", navigate: () => {} },
		]);
		await flush();

		expect(
			container.querySelector(".topbar-search-panel")?.textContent,
		).toContain("New result");
		expect(
			container.querySelector(".topbar-search-panel")?.textContent,
		).not.toContain("Old result");
		querySpy.mockRestore();
	});

	test("debounces a typing burst into one provider query", async () => {
		const seen: string[] = [];
		const querySpy = spyOn(
			destinationsModule,
			"createStoreDestinationProviders",
		);
		querySpy.mockImplementation(() => [
			{
				id: "seen",
				query: async (value) => {
					seen.push(value);
					return [];
				},
			} satisfies DestinationProvider,
		]);
		setSearchDebounceMsForTest(0);
		const { container } = mountApp("/");
		const search = input(container) as HTMLInputElement;
		for (const value of ["s", "se", "set"]) {
			fireEvent.input(search, { target: { value } });
			flushSync();
		}
		await settleSearch();
		expect(seen.filter((value) => value !== "")).toEqual(["set"]);
		querySpy.mockRestore();
	});
});
