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

const FOCUS_GLOBAL = "search.focusGlobal" as CommandId;

export const TopBarSearch: Component = () => {
	const store = useStore();
	const providers = createStoreDestinationProviders(store);
	const [query, setQuery] = createSignal("");
	const [focused, setFocused] = createSignal(false);
	const [destinations, setDestinations] = createSignal<Map<
		DestinationKind,
		Destination[]
	> | null>(null);
	let generation = 0;
	let currentGeneration = 0;
	let pendingTimer: ReturnType<typeof window.setTimeout> | undefined;
	let inputRef: HTMLInputElement | undefined;
	let wrapperRef: HTMLDivElement | undefined;
	let restoreTo: HTMLElement | null = null;

	function queryNow(
		input: string,
		mine: number,
	): Promise<Map<DestinationKind, Destination[]> | null> {
		return queryDestinations(providers, input, mine, () => currentGeneration)
			.then((result) => {
				if (mine !== currentGeneration || result === null) return null;
				setDestinations(result);
				return result;
			})
			.catch(() => {
				if (mine === currentGeneration) setDestinations(null);
				return null;
			});
	}

	createEffect(
		() => query(),
		(input) => {
			generation += 1;
			const mine = generation;
			currentGeneration = mine;
			clearTimeout(pendingTimer);
			pendingTimer = undefined;
			if (!input.trim()) {
				setDestinations(null);
				return;
			}
			// Coalesce typing before provider searches.
			// biome-ignore lint/style/noRestrictedGlobals: intentional search debounce
			pendingTimer = setTimeout(() => {
				pendingTimer = undefined;
				void queryNow(input, mine);
			}, SEARCH_DEBOUNCE_MS);
			return () => clearTimeout(pendingTimer);
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
		const fallback = document.querySelector<HTMLElement>(
			'.bridge-grid [tabindex="0"]',
		);
		if (restoreTo?.isConnected) restoreTo.focus();
		else fallback?.focus();
	}

	function select(row: DestinationSurfaceRow): void {
		row.navigate();
		setQuery("");
		if (inputRef) inputRef.value = "";
		inputRef?.focus();
	}

	function onFocusOut(event: FocusEvent): void {
		const next = event.relatedTarget;
		if (!(next instanceof Node) || !wrapperRef?.contains(next))
			setFocused(false);
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
		id: FOCUS_GLOBAL,
		title: "Focus global search",
		keywords: ["search", "global"],
		scope: "global",
		run: () => inputRef?.focus(),
	});
	onCleanup(() => {
		store.keyboard.registry.unregister(FOCUS_GLOBAL);
		store.keyboard.unregisterGroup(focusGroup);
	});

	async function handleEnter(): Promise<void> {
		const input = query();
		if (!input.trim()) return;
		if (pendingTimer !== undefined) {
			clearTimeout(pendingTimer);
			pendingTimer = undefined;
			const result = await queryNow(input, currentGeneration);
			const first = result ? destinationSurfaceRows(result)[0] : undefined;
			if (first) select(first);
			return;
		}
		const first = rows()[0];
		if (first) select(first);
	}
	return (
		<div
			ref={wrapperRef}
			class="topbar-search"
			onFocusIn={(event) => {
				setFocused(true);
				if (event.target === inputRef) {
					const previous = event.relatedTarget;
					restoreTo = previous instanceof HTMLElement ? previous : null;
				}
			}}
			onFocusOut={onFocusOut}
		>
			<input
				ref={inputRef}
				class="topbar-search-input"
				type="text"
				aria-label="Global search"
				placeholder="Search…"
				value={query()}
				onInput={(event) => setQuery(event.currentTarget.value)}
				onKeyDown={(event) => {
					if (event.key === "Escape") {
						event.preventDefault();
						clearAndRestore();
					} else if (event.key === "Enter") {
						event.preventDefault();
						void handleEnter();
					}
				}}
			/>
			<Show when={focused() && query().trim().length > 0}>
				<div class="topbar-search-panel">
					<For each={rows()}>
						{(row) => (
							<>
								<Show when={row.groupStart}>
									<div class="topbar-search-group">{row.groupLabel}</div>
								</Show>
								<button
									class="topbar-search-row"
									type="button"
									onMouseDown={(mouseEvent) => mouseEvent.preventDefault()}
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
