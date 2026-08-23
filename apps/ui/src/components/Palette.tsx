// Command palette (RIG-2483) — the Cmd/Ctrl+K do-an-action surface. One
// prefix-free input feeds two modes at once: ACTION (fuzzy over the command
// registry, scoped-above-global per the captured open-time zone) and NAVIGATION
// (store-backed destination providers, grouped by kind). Hosted once at the App
// root behind `store.paletteOpen()`.
//
// Host anatomy (A2/D8): a hand-positioned `position: fixed` wrapper IS
// `.cx-palette`, holding a Kobalte Search primitive with `open` pinned true and
// — per Kobalte's published command-menu recipe — `Search.Portal`/
// `Search.Content` replaced by a plain div directly holding `Search.Listbox`, so
// nothing portals and floating-ui stays out of the test path. Our own
// `.cx-palette-backdrop` renders behind the wrapper. Dismiss is ours and thin:
// Escape + backdrop-click → `store.closePalette()` (the sole teardown is the
// `<Show>` unmount; `open` stays pinned so a blur can never close the list).
// Focus is hand-granted (`input.focus()` on mount) since Search does not
// autofocus the input; each row's `onMouseDown`-preventDefault keeps focus in
// the input on click.

import { Search } from "@kobalte/core/search";
import {
	type Component,
	createMemo,
	createResource,
	createSignal,
	onMount,
	Show,
} from "solid-js";
import { useStore } from "../context";
import type {
	Command,
	Destination,
	DestinationKind,
} from "../keyboard/commands";
import {
	createStoreDestinationProviders,
	queryDestinations,
} from "../keyboard/destinations";
import { detectPlatform } from "../keyboard/dispatch";
import { fuzzyScore } from "../keyboard/fuzzy";
import { shortcutFor } from "../keyboard/keymap";
import "../design/components/palette.css";
import { ShortcutChip } from "./ShortcutChip";

/** A rendered palette result — an action (command) or a navigation destination.
 *  Merged into one Kobalte option list so keyboard traversal spans both modes. */
interface PaletteOption {
	/** Kobalte option value: stable + unique across modes. */
	readonly key: string;
	readonly title: string;
	/** The dim right-of-title context: a command's scope or a destination's kind. */
	readonly context: string;
	/** The resolved shortcut chord, if the entry has a keymap row. */
	readonly shortcut?: string;
	/** The section label this option belongs to (Commands / a kind label). */
	readonly groupLabel: string;
	/** True for the FIRST option of its group — the row that renders the header.
	 *  Kobalte's own section nodes all collapse onto key "" (one <Key by="key">
	 *  loop), so we render group headers inline off the flat option list instead. */
	readonly groupStart: boolean;
	run(): void;
}

/** Human labels for the destination-group headers, in render order. */
const KIND_LABELS: Record<DestinationKind, string> = {
	view: "Views",
	agent: "Agents",
	channel: "Channels",
	topic: "Topics",
	issue: "Issues",
	pr: "Pull requests",
};
const KIND_ORDER: readonly DestinationKind[] = [
	"view",
	"agent",
	"channel",
	"topic",
	"issue",
	"pr",
];

// (Kobalte's own section nodes can't carry stable keys, so groups are expressed
//  as `groupLabel`/`groupStart` on a flat option list — see PaletteOption.)

export const Palette: Component = () => {
	const store = useStore();
	const platform = detectPlatform();
	const providers = createStoreDestinationProviders(store);

	const [query, setQuery] = createSignal("");
	// The latest-wins generation counter: bumped per keystroke, captured at issue
	// and re-checked at resolve inside queryDestinations (A4/T3).
	let generation = 0;
	const [currentGen, setCurrentGen] = createSignal(0);

	let inputRef: HTMLInputElement | undefined;
	onMount(() => inputRef?.focus());

	// ── Action mode: fuzzy over the registry, scoped-above-global (A3/D5) ──
	const actionOptions = createMemo<PaletteOption[]>(() => {
		const input = query();
		const zone = store.paletteZone();
		const ranked: {
			key: string;
			title: string;
			context: string;
			shortcut?: string;
			run: () => void;
			score: number;
			scoped: boolean;
		}[] = [];
		for (const cmd of store.keyboard.registry.all()) {
			const score = bestCommandScore(cmd, input);
			if (score === null) continue;
			ranked.push({
				key: `cmd:${cmd.id}`,
				title: cmd.title,
				context: cmd.scope,
				shortcut: shortcutFor(cmd.id, platform),
				run: () => {
					cmd.run();
					store.closePalette();
				},
				score,
				// A command whose scope matches the captured open-time zone ranks in
				// the upper band; scope:"global" (and non-matching scopes) below it.
				scoped: zone !== null && cmd.scope === zone,
			});
		}
		ranked.sort((a, b) => {
			if (a.scoped !== b.scoped) return a.scoped ? -1 : 1;
			return b.score - a.score;
		});
		return ranked.map((r, i) => ({
			key: r.key,
			title: r.title,
			context: r.context,
			shortcut: r.shortcut,
			groupLabel: "Commands",
			groupStart: i === 0,
			run: r.run,
		}));
	});

	// ── Navigation mode: store-backed providers, grouped, latest-wins ──
	const [destinations] = createResource(
		() => query(),
		async (input): Promise<Map<DestinationKind, Destination[]> | null> => {
			generation += 1;
			const mine = generation;
			setCurrentGen(mine);
			return queryDestinations(providers, input, mine, currentGen);
		},
	);

	const loading = () => destinations.loading;

	// The rendered rows, in display order: the Commands group first, then one
	// group per destination kind. A flat list (Kobalte's <Key by="key"> renders
	// it) with each group's first row flagged `groupStart` so the row component
	// draws that group's `.cx-palette-group` header above it.
	const allOptions = createMemo<PaletteOption[]>(() => {
		const out: PaletteOption[] = [...actionOptions()];
		const byKind = destinations.latest;
		if (byKind) {
			for (const kind of KIND_ORDER) {
				const dests = byKind.get(kind);
				if (!dests || dests.length === 0) continue;
				const label = KIND_LABELS[kind];
				dests.forEach((d, i) => {
					out.push({
						key: `dest:${kind}:${d.id}`,
						title: d.title,
						context: label,
						groupLabel: label,
						groupStart: i === 0,
						run: () => {
							d.navigate();
							store.closePalette();
						},
					});
				});
			}
		}
		return out;
	});
	const isEmpty = () => !loading() && allOptions().length === 0;

	return (
		<>
			<div
				class="cx-palette-backdrop"
				onClick={() => store.closePalette()}
				aria-hidden="true"
			/>
			<div class="cx-palette">
				<Search<PaletteOption>
					open
					options={allOptions()}
					optionValue="key"
					optionTextValue="title"
					optionLabel="title"
					placeholder="Search commands and destinations…"
					onInputChange={(value) => setQuery(value)}
					onChange={(value) => value?.run()}
					onOpenChange={(isOpen) => {
						if (!isOpen) store.closePalette();
					}}
					itemComponent={(props) => (
						<>
							<Show when={props.item.rawValue.groupStart}>
								<li class="cx-palette-group" role="presentation">
									{props.item.rawValue.groupLabel}
								</li>
							</Show>
							<Search.Item
								item={props.item}
								class="cx-palette-row"
								onMouseDown={(e: MouseEvent) => e.preventDefault()}
							>
								<span class="cx-palette-glyph" aria-hidden="true" />
								<Search.ItemLabel class="cx-palette-title">
									{props.item.rawValue.title}
								</Search.ItemLabel>
								<span class="cx-palette-context">
									{props.item.rawValue.context}
								</span>
								<Show when={props.item.rawValue.shortcut}>
									{(chord) => <ShortcutChip chord={chord()} />}
								</Show>
							</Search.Item>
						</>
					)}
				>
					<Search.Control>
						<Search.Input ref={inputRef} class="cx-palette-input" />
					</Search.Control>
					{/* Recipe: a plain div REPLACES Search.Portal/Search.Content so the
					    list mounts inline below the input (no floating-ui). */}
					<div class="cx-palette-list">
						<Show when={loading()}>
							<div class="cx-palette-loading">
								<span class="cx-loader" data-topology="bar">
									<span class="cx-loader-fill" />
								</span>
							</div>
						</Show>
						<Search.Listbox />
						<Show when={isEmpty()}>
							<div class="cx-palette-empty">No results</div>
						</Show>
					</div>
				</Search>
			</div>
		</>
	);
};

/** Best fuzzy score for a command across its title and keywords (A3): the
 *  keywords broaden matching, so a keyword hit counts even when the title
 *  misses. `null` when neither the title nor any keyword matches. */
function bestCommandScore(cmd: Command, input: string): number | null {
	let best = fuzzyScore(input, cmd.title);
	for (const keyword of cmd.keywords) {
		const s = fuzzyScore(input, keyword);
		if (s !== null && (best === null || s > best)) best = s;
	}
	return best;
}
