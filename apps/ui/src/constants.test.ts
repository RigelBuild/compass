import { describe, expect, test } from "bun:test";
import {
	avatarInitial,
	fleetItemForAgent,
	unreachableFleetItem,
} from "./constants";
import type { Agent } from "./stub-data";

// A minimal resolvable agent: the constructor reads only account.id and
// account.handle, but the type wants a whole Agent.
function agentWith(id: string, handle: string): Agent {
	return {
		account: { id, handle, displayName: handle, kind: "agent" },
		terminals: [],
	};
}

describe("avatarInitial", () => {
	test("uppercases a plain handle's first letter", () => {
		expect(avatarInitial("Mintaka")).toBe("M");
	});

	test("uppercases a lowercase handle", () => {
		expect(avatarInitial("rigel")).toBe("R");
	});

	test("empty string falls back to ?", () => {
		expect(avatarInitial("")).toBe("?");
	});

	test("whitespace-only falls back to ?", () => {
		expect(avatarInitial("   ")).toBe("?");
	});

	// An emoji carries no Latin letter, so it clamps to ? on its own merits.
	test("emoji-leading handle falls back to ?", () => {
		expect(avatarInitial("🚀ocket")).toBe("?");
	});

	// Guards the grapheme read: this astral char NFKD-folds to plain "A", so a
	// regression to .at(0) would split the surrogate pair and return ? instead.
	test("astral first character is read whole, not as half a surrogate", () => {
		expect(avatarInitial("𝐀lpha")).toBe("A");
	});

	test("accented Latin handle strips the diacritic", () => {
		expect(avatarInitial("Émile")).toBe("E");
	});

	test("non-Latin script (Cyrillic) falls back to ?", () => {
		expect(avatarInitial("Живко")).toBe("?");
	});

	// ß uppercases to "SS"; we keep the FIRST resulting char, not ?, so a real
	// letter still distinguishes the agent.
	test("uppercase-expanding character keeps its first char", () => {
		expect(avatarInitial("ßravo")).toBe("S");
	});

	// A digit is a printable ASCII char and survives the clamp — a handle like
	// "3pio" tabs as "3", which tells it apart better than ?.
	test("digit-leading handle keeps the digit", () => {
		expect(avatarInitial("3pio")).toBe("3");
	});

	// Punctuation is likewise printable ASCII and kept — the derivation only
	// falls back to ? for non-ASCII-representable scripts, not for ASCII symbols.
	test("punctuation-leading handle keeps the punctuation", () => {
		expect(avatarInitial("_hidden")).toBe("_");
	});
});

describe("fleetItemForAgent", () => {
	// A resolvable agent builds the avatar arm, keyed on the live account, with
	// no unreachable mark so its agentId badges a real StateDot.
	test("builds an unmarked avatar item from the live account", () => {
		const item = fleetItemForAgent(agentWith("acc-cook", "cook"));
		expect(item.kind).toBe("avatar");
		expect(item.id).toBe("agent:acc-cook");
		expect(item.agentId).toBe("acc-cook");
		expect(item.title).toBe("cook");
		expect(item.unreachable).toBeUndefined();
	});

	// The initial routes through avatarInitial: a non-ASCII handle clamps to "?"
	// rather than leaking a glyph the brand face can't render.
	test("derives the letter through avatarInitial (non-ASCII clamps to ?)", () => {
		expect(fleetItemForAgent(agentWith("acc-1", "Живко")).letter).toBe("?");
		expect(fleetItemForAgent(agentWith("acc-2", "mintaka")).letter).toBe("M");
	});
});

describe("unreachableFleetItem", () => {
	// A pin builds the avatar arm marked unreachable, titled by the cached
	// handle, with its agentId carrying the pinned id (which resolves no agent).
	test("builds a marked avatar item from the cached pin", () => {
		const item = unreachableFleetItem({
			id: "acc-ghost",
			handle: "ghosthandle",
		});
		expect(item.kind).toBe("avatar");
		expect(item.id).toBe("agent:acc-ghost");
		expect(item.agentId).toBe("acc-ghost");
		expect(item.title).toBe("ghosthandle");
		expect(item.unreachable).toBe(true);
	});

	// The initial routes through avatarInitial here too — same derivation as the
	// live constructor, so a cached non-ASCII handle clamps.
	test("derives the letter through avatarInitial", () => {
		expect(unreachableFleetItem({ id: "acc-3", handle: "Émile" }).letter).toBe(
			"E",
		);
	});
});
