import { describe, expect, test } from "bun:test";
import { avatarInitial } from "./constants";

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
