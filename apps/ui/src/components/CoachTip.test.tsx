import { afterEach, describe, expect, test } from "bun:test";
import { cleanup, render } from "@solidjs/testing-library";
import type { CommandId } from "../keyboard/commands";
import { formatChordForDisplay, shortcutFor } from "../keyboard/keymap";
import { CoachTip, CoachTipContent, CoachTipTrigger } from "./CoachTip";

// CoachTip's rendered contract (RIG-2530): a Kobalte Tooltip whose content is a
// control's label + its keymap-resolved chord. Defends: chord derivation via
// shortcutFor (never hand-authored), the ARIA tooltip wiring the primitive
// owns (role="tooltip" + aria-describedby), focus reveal, the label-only path
// when no keymap row exists, and the sequence-aware branch that keeps a leader
// sequence out of ShortcutChip's "+"-split.

function setPlatform(platform: "mac" | "other"): void {
	Object.defineProperty(navigator, "platform", {
		value: platform === "mac" ? "MacIntel" : "Linux x86_64",
		configurable: true,
	});
}

const cmd = (id: string) => id as CommandId;

// Kobalte mounts the portalled content through createPresence on a macrotask,
// so a focus that opens the tooltip is observable only after one setTimeout(0).
async function settle(): Promise<void> {
	const { promise, resolve } = Promise.withResolvers<void>();
	setTimeout(resolve, 0);
	await promise;
}

const tooltipOf = (root: HTMLElement) =>
	root.querySelector<HTMLElement>('[role="tooltip"]');

afterEach(() => {
	cleanup();
	setPlatform("other");
});

describe("CoachTip (RIG-2530)", () => {
	test("label + chord: view.bridge on other shows the label and a Ctrl+B chip derived from the keymap", async () => {
		setPlatform("other");
		const { getByRole, baseElement } = render(() => (
			<CoachTip>
				<CoachTipTrigger as="button" type="button">
					Bridge
				</CoachTipTrigger>
				<CoachTipContent label="Bridge" command={cmd("view.bridge")} />
			</CoachTip>
		));

		getByRole("button").focus();
		await settle();

		const tooltip = tooltipOf(baseElement);
		expect(tooltip).not.toBeNull();
		expect(tooltip?.textContent).toContain("Bridge");

		const chip = tooltip?.querySelector(".cx-palette-shortcut");
		expect(chip).not.toBeNull();
		const kbds = Array.from(chip?.querySelectorAll("kbd") ?? []).map(
			(k) => k.textContent,
		);
		// Grounded in DEFAULT_KEYMAP via shortcutFor — never a hand-authored string.
		expect(shortcutFor(cmd("view.bridge"), "other")).toBe("Ctrl+B");
		expect(kbds).toEqual(["Ctrl", "B"]);
	});

	test("aria wiring: focusing the trigger opens the tooltip and links aria-describedby to the content id", async () => {
		setPlatform("other");
		const { getByRole, baseElement } = render(() => (
			<CoachTip>
				<CoachTipTrigger as="button" type="button">
					Bridge
				</CoachTipTrigger>
				<CoachTipContent label="Bridge" command={cmd("view.bridge")} />
			</CoachTip>
		));

		const trigger = getByRole("button");
		trigger.focus();
		await settle();

		const tooltip = tooltipOf(baseElement);
		expect(tooltip).not.toBeNull();
		expect(tooltip?.id).toBeTruthy();
		expect(trigger.getAttribute("aria-describedby")).toBe(tooltip?.id ?? "");
	});

	test("focus reveal: the tooltip opens on trigger focus with no pointer event", async () => {
		const { getByRole, baseElement } = render(() => (
			<CoachTip>
				<CoachTipTrigger as="button" type="button">
					Bridge
				</CoachTipTrigger>
				<CoachTipContent label="Bridge" command={cmd("view.bridge")} />
			</CoachTip>
		));

		expect(tooltipOf(baseElement)).toBeNull();
		getByRole("button").focus();
		await settle();
		expect(tooltipOf(baseElement)).not.toBeNull();
	});

	test("label-only: a command with no keymap row renders the label and no chip", async () => {
		setPlatform("other");
		// Guard the premise: board.openCardCrossLink has no keymap row (it is
		// board-nav dispatched, never a global chord — keymap.test.ts pins this).
		expect(
			shortcutFor(cmd("board.openCardCrossLink"), "other"),
		).toBeUndefined();

		const { getByRole, baseElement } = render(() => (
			<CoachTip>
				<CoachTipTrigger as="button" type="button">
					Open cross-link
				</CoachTipTrigger>
				<CoachTipContent
					label="Open cross-link"
					command={cmd("board.openCardCrossLink")}
				/>
			</CoachTip>
		));

		getByRole("button").focus();
		await settle();

		const tooltip = tooltipOf(baseElement);
		expect(tooltip).not.toBeNull();
		expect(tooltip?.textContent).toContain("Open cross-link");
		expect(tooltip?.querySelector(".cx-palette-shortcut")).toBeNull();
		expect(tooltip?.querySelector("kbd")).toBeNull();
	});

	test("sequence handling: a 'then'-sequence chord renders plain text (no kbd split), a '+'-chord renders the kbd chip", async () => {
		// The sequence fixture is #544's formatChordForDisplay output (DL-251),
		// so a format change surfaces here rather than silently mis-rendering.
		const sequence = formatChordForDisplay("G B", "other");
		expect(sequence).toBe("G then B");

		const seq = render(() => (
			<CoachTip>
				<CoachTipTrigger as="button" type="button">
					Go
				</CoachTipTrigger>
				<CoachTipContent label="Go" chord={sequence} />
			</CoachTip>
		));
		seq.getByRole("button").focus();
		await settle();
		const seqTip = tooltipOf(seq.baseElement);
		expect(seqTip?.textContent).toContain(sequence);
		expect(seqTip?.querySelector("kbd")).toBeNull();
		cleanup();

		const plain = render(() => (
			<CoachTip>
				<CoachTipTrigger as="button" type="button">
					Bridge
				</CoachTipTrigger>
				<CoachTipContent label="Bridge" chord="Ctrl+B" />
			</CoachTip>
		));
		plain.getByRole("button").focus();
		await settle();
		const plainTip = tooltipOf(plain.baseElement);
		const kbds = Array.from(plainTip?.querySelectorAll("kbd") ?? []).map(
			(k) => k.textContent,
		);
		expect(kbds).toEqual(["Ctrl", "B"]);
	});
});
