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
		const spine = createKeyboardSpine({ showBridge: () => ran++ });

		const cmd = spine.registry.get(id("view.bridge"));
		expect(cmd).toBeDefined();
		expect(cmd?.title).toBe("Go to Bridge");
		expect(cmd?.scope).toBe("global");
		expect(spine.registry.all().map((c) => c.id)).toContain(id("view.bridge"));

		cmd?.run();
		expect(ran).toBe(1);
	});

	test("registerGroup/unregisterGroup round-trip: activeGroup reflects the set", () => {
		const spine = createKeyboardSpine({ showBridge: () => {} });
		const g = stubHandle("board", "main", () => true);

		expect(spine.activeGroup()).toBeNull(); // empty set
		spine.registerGroup(g);
		expect(spine.activeGroup()).toBe(g);
		spine.unregisterGroup(g);
		expect(spine.activeGroup()).toBeNull(); // retracted
	});

	test("activeGroup picks the focused group among several, null when none", () => {
		const spine = createKeyboardSpine({ showBridge: () => {} });
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
		const spine = createKeyboardSpine({ showBridge: () => {} });
		let focused = false;
		const board = stubHandle("board", "main", () => focused);
		spine.registerGroup(board);

		expect(spine.activeZone()).toBeNull();
		focused = true;
		expect(spine.activeZone()).toBe("main");
	});
});
