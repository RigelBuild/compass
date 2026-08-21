import { describe, expect, test } from "bun:test";
import { createRoot, createSignal } from "solid-js";
import type { CommandId } from "./commands";
import { createRovingGroup, type Stop } from "./roving";
import type { RovingGroup } from "./zones";

// The roving-group primitive owns the one-tab-stop DOM invariant over a reactive
// stop list. These defend: exactly one tabindex=0 across stops, a cursor move
// refocuses + re-tabs the new stop WHEN focus is already in the group, no
// focus-steal on mount or on a background stops-rebuild (WCAG 3.2.1), a stale
// cursor (removed stop) falls back without crashing while keeping one
// tabindex=0, and handleCommand delegates to onCommand verbatim.

const GROUP: RovingGroup = { zone: "main", id: "board" };
const id = (s: string): CommandId => s as CommandId;

// Drain microtasks so Solid's effect queue flushes before a DOM read.
const flush = async (): Promise<void> => {
	for (let i = 0; i < 20; i++) await Promise.resolve();
};

function makeStops(...ids: string[]): Stop[] {
	return ids.map((sid) => {
		const el = document.createElement("button");
		el.dataset.id = sid;
		el.scrollIntoView = () => {};
		document.body.appendChild(el);
		return { id: sid, el };
	});
}

const tabbable = (stops: Stop[]): string[] =>
	stops.filter((s) => s.el.tabIndex === 0).map((s) => s.id);

describe("createRovingGroup", () => {
	test("exactly one stop carries tabindex=0 (the cursor), rest -1", async () => {
		await createRoot(async (dispose) => {
			const stops = makeStops("a", "b", "c");
			const [cursor] = createSignal<string | null>("b");
			createRovingGroup({
				group: GROUP,
				stops: () => stops,
				cursor,
				setCursor: () => {},
				onCommand: () => true,
			});
			await flush();

			expect(tabbable(stops)).toEqual(["b"]);
			expect(stops.map((s) => s.el.tabIndex)).toEqual([-1, 0, -1]);
			dispose();
		});
	});

	test("an in-group cursor move re-tabs and refocuses the new stop", async () => {
		await createRoot(async (dispose) => {
			const stops = makeStops("a", "b", "c");
			const [cursor, setCursor] = createSignal<string | null>("a");
			createRovingGroup({
				group: GROUP,
				stops: () => stops,
				cursor,
				setCursor: () => {},
				onCommand: () => true,
			});
			await flush();
			expect(tabbable(stops)).toEqual(["a"]);

			// Enter the group (native Tab lands on the cursor stop), then move.
			stops[0]?.el.focus();
			setCursor("c");
			await flush();

			expect(tabbable(stops)).toEqual(["c"]);
			expect(document.activeElement).toBe(stops[2]?.el ?? null);
			dispose();
		});
	});

	test("does not steal focus on mount or a background cursor change", async () => {
		await createRoot(async (dispose) => {
			const outside = document.createElement("input");
			document.body.appendChild(outside);
			outside.focus();
			const stops = makeStops("a", "b", "c");
			const [cursor, setCursor] = createSignal<string | null>("a");
			createRovingGroup({
				group: GROUP,
				stops: () => stops,
				cursor,
				setCursor: () => {},
				onCommand: () => true,
			});
			await flush();

			// Mount tabbed the cursor stop but did NOT pull focus off `outside`.
			expect(tabbable(stops)).toEqual(["a"]);
			expect(document.activeElement).toBe(outside);

			// A cursor change while focus is outside the group stays hands-off.
			setCursor("c");
			await flush();
			expect(tabbable(stops)).toEqual(["c"]);
			expect(document.activeElement).toBe(outside);

			outside.remove();
			dispose();
		});
	});

	test("stale cursor falls back to the first stop, still one tabindex=0", async () => {
		await createRoot(async (dispose) => {
			const [stops, setStops] = createSignal(makeStops("a", "b", "c"));
			const [cursor] = createSignal<string | null>("b");
			createRovingGroup({
				group: GROUP,
				stops,
				cursor,
				setCursor: () => {},
				onCommand: () => true,
			});
			await flush();
			expect(tabbable(stops())).toEqual(["b"]);

			// Remove the cursor's stop — the cursor id "b" is now stale.
			const survivors = stops().filter((s) => s.id !== "b");
			setStops(survivors);
			await flush();

			// Exactly one stop tabbable, and it is the first survivor (fallback).
			expect(tabbable(survivors)).toEqual(["a"]);
			dispose();
		});
	});

	test("handleCommand delegates to onCommand and returns its boolean", async () => {
		await createRoot(async (dispose) => {
			const stops = makeStops("a");
			const [cursor] = createSignal<string | null>("a");
			const seen: CommandId[] = [];
			const handle = createRovingGroup({
				group: GROUP,
				stops: () => stops,
				cursor,
				setCursor: () => {},
				onCommand: (cmd) => {
					seen.push(cmd);
					return cmd === id("list.moveNext");
				},
			});
			await flush();

			expect(handle.handleCommand(id("list.moveNext"))).toBe(true);
			expect(handle.handleCommand(id("list.moveLeft"))).toBe(false);
			expect(seen).toEqual([id("list.moveNext"), id("list.moveLeft")]);
			expect(handle.group).toBe(GROUP);
			dispose();
		});
	});

	test("focus() focuses the cursor stop", async () => {
		await createRoot(async (dispose) => {
			const stops = makeStops("a", "b");
			const [cursor] = createSignal<string | null>("b");
			const handle = createRovingGroup({
				group: GROUP,
				stops: () => stops,
				cursor,
				setCursor: () => {},
				onCommand: () => true,
			});
			await flush();

			handle.focus();
			expect(document.activeElement).toBe(stops[1]?.el ?? null);
			dispose();
		});
	});
});
