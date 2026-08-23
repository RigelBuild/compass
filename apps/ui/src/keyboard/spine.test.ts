import { describe, expect, test } from "bun:test";
import type { CommandId } from "./commands";
import type { RovingGroupHandle } from "./roving";
import { createKeyboardSpine } from "./spine";
import type { FocusZone } from "./zones";

// The keyboard spine (RIG-2456): the shared registry + the published roving-group
// set, plus the tier-1/tier-2 accessors the root installKeymap reads. These units
// defend the group-publication model (RD-2): register/unregister round-trip,
// `activeGroup()` deriving the answer from `isFocused()` (never mount order), and
// `view.bridge` registered with its behavior at spine creation (A4).

const id = (s: string): CommandId => s as CommandId;

/** The spine's dependency closures, all no-op by default; a test overrides only
 *  the leg it asserts. Keeps every `createKeyboardSpine` call at the current
 *  (RIG-2483) shape without repeating six stubs. */
function stubDeps(
	overrides: Partial<{
		showBridge: () => void;
		toggleShortcuts: () => void;
		showBacklog: () => void;
		showDone: () => void;
		showSettings: () => void;
		togglePalette: () => void;
	}> = {},
) {
	return {
		showBridge: () => {},
		toggleShortcuts: () => {},
		showBacklog: () => {},
		showDone: () => {},
		showSettings: () => {},
		togglePalette: () => {},
		...overrides,
	};
}

/** A stub handle with a controllable `isFocused` so a test decides which group
 *  the scan should pick — the real `isFocused` scans `document.activeElement`,
 *  which the spine unit does not exercise (that is the App/Bridge suite). */
function stubHandle(
	groupId: string,
	zone: FocusZone,
	focused: () => boolean,
): RovingGroupHandle {
	return {
		group: { zone, id: groupId },
		handleCommand: () => false,
		isFocused: focused,
		focus: () => {},
	};
}

describe("createKeyboardSpine", () => {
	test("registers view.bridge as a global command that runs showBridge", () => {
		let ran = 0;
		const spine = createKeyboardSpine(stubDeps({ showBridge: () => ran++ }));

		const cmd = spine.registry.get(id("view.bridge"));
		expect(cmd).toBeDefined();
		expect(cmd?.title).toBe("Go to Bridge");
		expect(cmd?.scope).toBe("global");
		expect(spine.registry.all().map((c) => c.id)).toContain(id("view.bridge"));

		cmd?.run();
		expect(ran).toBe(1);
	});

	test("registerGroup/unregisterGroup round-trip: activeGroup reflects the set", () => {
		const spine = createKeyboardSpine(stubDeps());
		const g = stubHandle("board", "main", () => true);

		expect(spine.activeGroup()).toBeNull(); // empty set
		spine.registerGroup(g);
		expect(spine.activeGroup()).toBe(g);
		spine.unregisterGroup(g);
		expect(spine.activeGroup()).toBeNull(); // retracted
	});

	test("activeGroup picks the focused group among several, null when none", () => {
		const spine = createKeyboardSpine(stubDeps());
		let treeFocused = false;
		let boardFocused = false;
		const tree = stubHandle("tree", "left", () => treeFocused);
		const board = stubHandle("board", "main", () => boardFocused);
		spine.registerGroup(tree);
		spine.registerGroup(board);

		expect(spine.activeGroup()).toBeNull(); // neither focused

		boardFocused = true;
		expect(spine.activeGroup()).toBe(board);

		boardFocused = false;
		treeFocused = true;
		expect(spine.activeGroup()).toBe(tree);
	});

	test("activeZone mirrors the focused group's zone (null when none)", () => {
		const spine = createKeyboardSpine(stubDeps());
		let focused = false;
		const board = stubHandle("board", "main", () => focused);
		spine.registerGroup(board);

		expect(spine.activeZone()).toBeNull();
		focused = true;
		expect(spine.activeZone()).toBe("main");
	});

	test("registers view.shortcuts as a global command that runs toggleShortcuts", () => {
		let toggled = 0;
		const spine = createKeyboardSpine(
			stubDeps({ toggleShortcuts: () => toggled++ }),
		);

		const cmd = spine.registry.get(id("view.shortcuts"));
		expect(cmd).toBeDefined();
		expect(cmd?.title).toBe("Keyboard shortcuts");
		expect(cmd?.scope).toBe("global");
		expect(spine.registry.all().map((c) => c.id)).toContain(
			id("view.shortcuts"),
		);

		cmd?.run();
		expect(toggled).toBe(1);
	});

	test("seeds palette.open + view.settings/backlog/done, all global, none with a shortcut string (D4)", () => {
		const spine = createKeyboardSpine(stubDeps());
		const ids = spine.registry.all().map((c) => c.id);
		// The five app-lifetime seeds (view.bridge covered above) present in one
		// registry — action mode reads exactly these plus surface-registered ones.
		for (const seed of [
			"palette.open",
			"view.settings",
			"view.backlog",
			"view.done",
		]) {
			const cmd = spine.registry.get(id(seed));
			expect(cmd).toBeDefined();
			expect(cmd?.scope).toBe("global");
			// Chips derive from the keymap via shortcutFor — no seed hand-authors one.
			expect(cmd?.shortcut).toBeUndefined();
			expect(ids).toContain(id(seed));
		}
	});

	test("palette.open.run() fires togglePalette; view.settings/backlog/done run their show* legs", () => {
		let palette = 0;
		let settings = 0;
		let backlog = 0;
		let done = 0;
		const spine = createKeyboardSpine(
			stubDeps({
				togglePalette: () => palette++,
				showSettings: () => settings++,
				showBacklog: () => backlog++,
				showDone: () => done++,
			}),
		);
		spine.registry.get(id("palette.open"))?.run();
		spine.registry.get(id("view.settings"))?.run();
		spine.registry.get(id("view.backlog"))?.run();
		spine.registry.get(id("view.done"))?.run();
		expect(palette).toBe(1);
		expect(settings).toBe(1);
		expect(backlog).toBe(1);
		expect(done).toBe(1);
	});
});
