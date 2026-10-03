import { afterEach, describe, expect, jest, spyOn, test } from "bun:test";
import { cleanup, fireEvent } from "@solidjs/testing-library";
import { flush as flushSync } from "solid-js";
import type { Destination, DestinationProvider } from "../keyboard/commands";
import { SEARCH_DEBOUNCE_MS } from "../keyboard/destination-surface";
import * as destinationsModule from "../keyboard/destinations";
import { flush, mountApp } from "../test-router";

afterEach(() => {
	cleanup();
	jest.useRealTimers();
});

async function settleSearch(): Promise<void> {
	jest.advanceTimersByTime(0);
	flushSync();
	jest.advanceTimersByTime(SEARCH_DEBOUNCE_MS);
	await flush();
}

const input = (container: HTMLElement) =>
	container.querySelector<HTMLInputElement>(".topbar-search-input");

describe("TopBarSearch", () => {
	test("typing renders grouped destination rows and Enter navigates", async () => {
		jest.useFakeTimers();
		const { container, store } = mountApp("/");
		const search = input(container) as HTMLInputElement;
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
		jest.useFakeTimers();
		const { container } = mountApp("/");
		const prior = container.querySelector<HTMLElement>(".topbar .view-tab");
		const search = input(container) as HTMLInputElement;
		fireEvent.focus(search, { relatedTarget: prior });
		fireEvent.input(search, { target: { value: "backlog" } });
		await settleSearch();
		fireEvent.keyDown(search, { key: "Escape" });
		expect(search.value).toBe("");
		expect(document.activeElement).toBe(prior);
	});

	test("latest query wins when an earlier provider resolves slowly", async () => {
		jest.useFakeTimers();
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
		fireEvent.input(search, { target: { value: "old" } });
		await settleSearch();
		fireEvent.input(search, { target: { value: "new" } });
		await settleSearch();
		pending[0]?.([
			{ id: "old", title: "Old result", kind: "agent", navigate: () => {} },
		]);
		flushSync();
		expect(
			container.querySelector(".topbar-search-panel")?.textContent,
		).toContain("New result");
		expect(
			container.querySelector(".topbar-search-panel")?.textContent,
		).not.toContain("Old result");
		querySpy.mockRestore();
	});

	test("debounces a typing burst into one provider query", async () => {
		jest.useFakeTimers();
		let queries = 0;
		const querySpy = spyOn(
			destinationsModule,
			"createStoreDestinationProviders",
		);
		querySpy.mockImplementation(() => [
			{
				id: "count",
				query: async () => {
					queries += 1;
					return [];
				},
			} satisfies DestinationProvider,
		]);
		const { container } = mountApp("/");
		const search = input(container) as HTMLInputElement;
		fireEvent.input(search, { target: { value: "s" } });
		fireEvent.input(search, { target: { value: "se" } });
		fireEvent.input(search, { target: { value: "set" } });
		await settleSearch();
		expect(queries).toBe(1);
		querySpy.mockRestore();
	});
});
