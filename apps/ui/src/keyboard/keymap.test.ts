import { describe, expect, test } from "bun:test";
import type { CommandId } from "./commands";
import {
	chordSegments,
	DEFAULT_KEYMAP,
	formatChordForDisplay,
	type KeymapEntry,
	leaderPrefixes,
	shortcutFor,
	shortcutForAria,
} from "./keymap";

// shortcutFor (RIG-2483, A5/D4) — the single derivation for every shortcut chip:
// the first DEFAULT_KEYMAP row bound to an id, resolveChord-resolved. Pure
// module. These defend the hit/miss/platform contract the chips rely on.

const id = (s: string): CommandId => s as CommandId;

describe("shortcutFor", () => {
	test("resolves a bound command's chord, platform-specific (Mod→Cmd/Ctrl)", () => {
		expect(shortcutFor(id("palette.open"), "other")).toBe("Ctrl+K");
		expect(shortcutFor(id("palette.open"), "mac")).toBe("Cmd+K");
	});

	test("resolves view.settings' Mod+, (the D6-lit chord)", () => {
		expect(shortcutFor(id("view.settings"), "other")).toBe("Ctrl+,");
		expect(shortcutFor(id("view.settings"), "mac")).toBe("Cmd+,");
	});

	test("a non-Mod chord passes through unchanged on both platforms", () => {
		expect(shortcutFor(id("board.openAssignedAgent"), "other")).toBe(
			"Shift+Enter",
		);
		expect(shortcutFor(id("board.openAssignedAgent"), "mac")).toBe(
			"Shift+Enter",
		);
	});

	test("undefined for a command with no keymap row (miss)", () => {
		expect(shortcutFor(id("board.openCardCrossLink"), "other")).toBeUndefined();
		expect(shortcutFor(id("nonexistent.command"), "other")).toBeUndefined();
	});

	test("a sequence-only command renders its formatted sequence chord", () => {
		// view.backlog's only keymap row is the G L sequence (T2, RIG-2484).
		expect(shortcutFor(id("view.backlog"), "other")).toBe("G then L");
	});

	test("a dual-bound command shows its modifier chord (sequence row is later)", () => {
		// view.bridge is Mod+B (first) then G B; the modifier row wins.
		expect(shortcutFor(id("view.bridge"), "other")).toBe("Ctrl+B");
	});

	test("returns the FIRST matching row for an id bound more than once", () => {
		// Enter is bound to list.openOrSelect (unscoped) AND comms.send (when:main);
		// shortcutFor takes the first DEFAULT_KEYMAP row — list.openOrSelect's.
		expect(shortcutFor(id("list.openOrSelect"), "other")).toBe("Enter");
	});
});

describe("shortcutForAria", () => {
	test("emits WAI-ARIA modifier tokens (Mod→Control/Meta), never Ctrl/Cmd", () => {
		expect(shortcutForAria(id("view.bridge"), "other")).toBe("Control+B");
		expect(shortcutForAria(id("view.bridge"), "mac")).toBe("Meta+B");
	});

	test("undefined for a command with no keymap row (miss)", () => {
		expect(shortcutForAria(id("view.backlog"), "other")).toBeUndefined();
		expect(shortcutForAria(id("nonexistent.command"), "other")).toBeUndefined();
	});
});

// Sequence-grammar helpers (RIG-2484 T1) — pure, table-independent. Tested over
// a FIXTURE keymap because DEFAULT_KEYMAP carries no sequence rows until T2.

const seqFixture: readonly KeymapEntry[] = [
	{ chord: "Mod+B", commandId: id("view.bridge") },
	{ chord: "G B", commandId: id("view.bridge") },
	{ chord: "G L", commandId: id("view.backlog") },
];

describe("chordSegments", () => {
	test("splits a sequence on its single space", () => {
		expect(chordSegments("G B")).toEqual(["G", "B"]);
	});

	test("a plain chord yields a one-element array", () => {
		expect(chordSegments("Mod+B")).toEqual(["Mod+B"]);
		expect(chordSegments("Shift+Enter")).toEqual(["Shift+Enter"]);
	});
});

describe("leaderPrefixes", () => {
	test("collects the resolved first segment of every sequence row, and nothing else", () => {
		const prefixes = leaderPrefixes(seqFixture, "other");
		expect([...prefixes]).toEqual(["G"]);
	});

	test("empty for a table with no sequence rows", () => {
		const single: readonly KeymapEntry[] = [
			{ chord: "Mod+B", commandId: id("view.bridge") },
		];
		expect(leaderPrefixes(single, "other").size).toBe(0);
	});

	test("resolves the leader segment per-platform (a Mod leader → Cmd/Ctrl)", () => {
		const modLeader: readonly KeymapEntry[] = [
			{ chord: "Mod+X Y", commandId: id("view.bridge") },
		];
		expect([...leaderPrefixes(modLeader, "mac")]).toEqual(["Cmd+X"]);
		expect([...leaderPrefixes(modLeader, "other")]).toEqual(["Ctrl+X"]);
	});

	test("accumulates distinct leaders and dedups rows sharing one leader", () => {
		const twoLeaders: readonly KeymapEntry[] = [
			{ chord: "G B", commandId: id("view.bridge") },
			{ chord: "G L", commandId: id("view.backlog") },
			{ chord: "Space X", commandId: id("view.done") },
		];
		expect([...leaderPrefixes(twoLeaders, "other")].sort()).toEqual([
			"G",
			"Space",
		]);
	});
});

describe("formatChordForDisplay", () => {
	test("a single chord resolves platform-specifically (Mod→Cmd/Ctrl)", () => {
		expect(formatChordForDisplay("Mod+B", "mac")).toBe("Cmd+B");
		expect(formatChordForDisplay("Mod+B", "other")).toBe("Ctrl+B");
	});

	test("a sequence joins resolved segments with ' then '", () => {
		expect(formatChordForDisplay("G B", "mac")).toBe("G then B");
		expect(formatChordForDisplay("G L", "other")).toBe("G then L");
	});
});

// DEFAULT_KEYMAP authoring invariants for leader sequences (RIG-2484 §A2).
describe("DEFAULT_KEYMAP sequence authoring invariants", () => {
	const MODIFIER = /(?:^|\+)(?:Mod|Shift|Alt|Ctrl|Cmd|Meta)(?:\+|$)/;
	const sequenceRows = DEFAULT_KEYMAP.filter(
		(e) => chordSegments(e.chord).length > 1,
	);
	const singleChords = new Set(
		DEFAULT_KEYMAP.filter((e) => chordSegments(e.chord).length === 1).map(
			(e) => e.chord,
		),
	);

	test("every sequence is exactly two segments", () => {
		for (const entry of sequenceRows) {
			expect(chordSegments(entry.chord).length).toBe(2);
		}
	});

	test("every segment of a sequence is modifier-less", () => {
		for (const entry of sequenceRows) {
			for (const segment of chordSegments(entry.chord)) {
				expect(MODIFIER.test(segment)).toBe(false);
			}
		}
	});

	test("a sequence's first segment is not also a complete single chord", () => {
		for (const entry of sequenceRows) {
			expect(singleChords.has(chordSegments(entry.chord)[0])).toBe(false);
		}
	});
});
