import {
	type Component,
	createEffect,
	createSignal,
	For,
	onCleanup,
	Show,
} from "solid-js";
import { useStore } from "../context";
import type {
	CommandId,
	Destination,
	DestinationKind,
} from "../keyboard/commands";
import {
	type DestinationSurfaceRow,
	destinationSurfaceRows,
	SEARCH_DEBOUNCE_MS,
} from "../keyboard/destination-surface";
import {
	createStoreDestinationProviders,
	queryDestinations,
} from "../keyboard/destinations";
import { createRovingGroup } from "../keyboard/roving";

export const TopBarSearch: Component = () => {
	const store = useStore();
	const providers = createStoreDestinationProviders(store);
	const [query, setQuery] = createSignal("");
	const [destinations, setDestinations] = createSignal<Map<
		DestinationKind,
		Destination[]
	> | null>(null);
	let generation = 0;
	let currentGeneration = 0;
	let inputRef: HTMLInputElement | undefined;
	let restoreTo: HTMLElement | null = null;

	createEffect(
		() => query(),
		(input) => {
			generation += 1;
			const mine = generation;
			currentGeneration = mine;
			// This production debounce coalesces keystrokes before querying providers.
			// biome-ignore lint/style/noRestrictedGlobals: intentional 150ms search debounce
			const timer = setTimeout(() => {
				void queryDestinations(providers, input, mine, () => currentGeneration)
					.then((result) => {
						if (mine === currentGeneration && result !== null)
							setDestinations(result);
					})
					.catch(() => {
						if (mine === currentGeneration) setDestinations(null);
					});
			}, SEARCH_DEBOUNCE_MS);
			return () => clearTimeout(timer);
		},
	);

	const rows = () => {
		const current = destinations();
		return current ? destinationSurfaceRows(current) : [];
	};

	function clearAndRestore(): void {
		setQuery("");
		if (inputRef) inputRef.value = "";
		inputRef?.blur();
		if (restoreTo?.isConnected) restoreTo.focus();
	}

	function select(row: DestinationSurfaceRow): void {
		row.navigate();
		setQuery("");
		if (inputRef) inputRef.value = "";
		inputRef?.focus();
	}

	const focusGroup = createRovingGroup({
		group: { zone: "topbar", id: "global-search" },
		stops: () => (inputRef ? [{ id: "search", el: inputRef }] : []),
		cursor: () => "search",
		setCursor: () => {},
		onCommand: () => false,
	});
	store.keyboard.registerGroup(focusGroup);
	store.keyboard.registry.register({
		id: "search.focusGlobal" as CommandId,
		title: "Focus global search",
		keywords: ["search", "global"],
		scope: "global",
		run: () => inputRef?.focus(),
	});
	onCleanup(() => {
		store.keyboard.registry.unregister("search.focusGlobal" as CommandId);
		store.keyboard.unregisterGroup(focusGroup);
	});

	return (
		<div class="topbar-search">
			<input
				ref={inputRef}
				class="topbar-search-input"
				type="text"
				aria-label="Global search"
				placeholder="Search…"
				value={query()}
				onFocus={(event) => {
					const previous = event.relatedTarget;
					restoreTo = previous instanceof HTMLElement ? previous : null;
				}}
				onInput={(event) => setQuery(event.currentTarget.value)}
				onKeyDown={(event) => {
					if (event.key === "Escape") {
						event.preventDefault();
						clearAndRestore();
					} else if (event.key === "Enter") {
						event.preventDefault();
						const row = rows()[0];
						if (row) select(row);
					}
				}}
			/>
			<Show when={query().trim().length > 0}>
				<div class="topbar-search-panel" role="listbox">
					<For each={rows()}>
						{(row) => (
							<>
								<Show when={row.groupStart}>
									<div class="topbar-search-group" role="presentation">
										{row.groupLabel}
									</div>
								</Show>
								<button
									class="topbar-search-row"
									role="option"
									type="button"
									onMouseDown={(event) => event.preventDefault()}
									onClick={() => select(row)}
								>
									{row.title}
								</button>
							</>
						)}
					</For>
				</div>
			</Show>
		</div>
	);
};
