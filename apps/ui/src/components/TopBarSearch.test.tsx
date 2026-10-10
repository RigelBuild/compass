// Global search tests use an overridable debounce and event-loop settling.
import {
	afterEach,
	beforeEach,
	describe,
	expect,
	mock,
	spyOn,
	test,
} from "bun:test";
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
	// Restore spies even when an assertion throws before the inline restore.
	mock.restore();
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
	container.querySelector<HTMLInputElement>(".topbar-search .cx-search");

describe("TopBarSearch", () => {
	test("exposes combobox and listbox semantics as results open and close", async () => {
		const { container } = mountApp("/");
		const search = input(container) as HTMLInputElement;
		expect(search.getAttribute("role")).toBe("combobox");
		expect(search.getAttribute("aria-expanded")).toBe("false");
		search.focus();
		fireEvent.input(search, { target: { value: "s" } });
		await settleSearch();
		expect(search.getAttribute("aria-expanded")).toBe("true");
		const listbox = container.querySelector('[role="listbox"]');
		expect(listbox?.id ?? undefined).toBe(
			search.getAttribute("aria-controls") ?? undefined,
		);
		const options = container.querySelectorAll<HTMLElement>('[role="option"]');
		expect(options.length).toBeGreaterThanOrEqual(2);
		const selected = [...options].filter(
			(option) => option.getAttribute("aria-selected") === "true",
		);
		expect(selected).toHaveLength(1);
		expect(search.getAttribute("aria-activedescendant")).toBe(selected[0]?.id);
		for (const group of container.querySelectorAll(".topbar-search-group")) {
			expect(group.getAttribute("role")).toBe("presentation");
		}
		search.blur();
		await flush();
		expect(search.getAttribute("aria-expanded")).toBe("false");
		expect(search.getAttribute("aria-activedescendant")).toBeNull();
		expect(container.querySelector('[role="listbox"]')).toBeNull();
	});

	test("Escape closes the listbox and updates aria-expanded", async () => {
		const { container } = mountApp("/");
		const search = input(container) as HTMLInputElement;
		search.focus();
		fireEvent.input(search, { target: { value: "settings" } });
		await settleSearch();
		expect(search.getAttribute("aria-expanded")).toBe("true");
		fireEvent.keyDown(search, { key: "Escape" });
		await flush();
		expect(search.getAttribute("aria-expanded")).toBe("false");
		expect(container.querySelector('[role="listbox"]')).toBeNull();
	});
	test("ArrowDown activates and Enter selects the second result", async () => {
		let navigated = "";
		const querySpy = spyOn(
			destinationsModule,
			"createStoreDestinationProviders",
		);
		querySpy.mockImplementation(
			() =>
				[
					{
						id: "controlled",
						query: () =>
							Promise.resolve([
								{
									id: "first",
									title: "First result",
									kind: "agent",
									navigate: () => {
										navigated = "first";
									},
								},
								{
									id: "second",
									title: "Second result",
									kind: "agent",
									navigate: () => {
										navigated = "second";
									},
								},
							]),
					},
				] satisfies DestinationProvider[],
		);
		const { container } = mountApp("/");
		const search = input(container) as HTMLInputElement;
		search.focus();
		fireEvent.input(search, { target: { value: "results" } });
		await settleSearch();
		const options = container.querySelectorAll<HTMLElement>('[role="option"]');
		expect(options).toHaveLength(2);
		const second = options[1];
		fireEvent.keyDown(search, { key: "ArrowDown" });
		await flush();
		expect(search.getAttribute("aria-activedescendant")).toBe(second?.id);
		expect(second?.getAttribute("aria-selected")).toBe("true");
		expect(
			[...options].filter(
				(option) => option.getAttribute("aria-selected") === "true",
			),
		).toHaveLength(1);
		fireEvent.keyDown(search, { key: "Enter" });
		await flush();
		expect(navigated).toBe("second");
		querySpy.mockRestore();
	});
	test("ArrowUp wraps, a new result set resets the active row, and no hits says so", async () => {
		let navigated = "";
		const hit = (id: string): Destination => ({
			id,
			title: `${id} result`,
			kind: "agent",
			navigate: () => {
				navigated = id;
			},
		});
		const querySpy = spyOn(
			destinationsModule,
			"createStoreDestinationProviders",
		);
		querySpy.mockImplementation(
			() =>
				[
					{
						id: "controlled",
						query: (q: string) =>
							Promise.resolve(
								q === "none"
									? []
									: q === "abc"
										? [hit("a"), hit("b"), hit("c")]
										: [hit("x"), hit("y")],
							),
					},
				] satisfies DestinationProvider[],
		);
		const { container } = mountApp("/");
		const search = input(container) as HTMLInputElement;
		search.focus();
		fireEvent.input(search, { target: { value: "abc" } });
		await settleSearch();
		fireEvent.keyDown(search, { key: "ArrowUp" });
		await flush();
		const options = container.querySelectorAll<HTMLElement>('[role="option"]');
		expect(search.getAttribute("aria-activedescendant")).toBe(options[2]?.id);

		fireEvent.input(search, { target: { value: "xy" } });
		await settleSearch();
		fireEvent.keyDown(search, { key: "Enter" });
		await flush();
		expect(navigated).toBe("x");

		search.focus();
		fireEvent.input(search, { target: { value: "none" } });
		await settleSearch();
		expect(container.querySelector('[role="listbox"]')?.textContent).toContain(
			"No results",
		);
		querySpy.mockRestore();
	});
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
		const prior = container.querySelector<HTMLElement>('.topbar [role="tab"]');
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
