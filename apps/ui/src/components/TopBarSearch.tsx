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
import type { LiveClients } from "../live/client";

const FOCUS_GLOBAL = "search.focusGlobal" as CommandId;

export const TopBarSearch: Component<{
	clients?: Pick<LiveClients, "comms" | "compass">;
}> = (props) => {
	const store = useStore();
	const providers = createStoreDestinationProviders(store, props.clients);
	const [query, setQuery] = createSignal("");
	const [focused, setFocused] = createSignal(false);
	const [destinations, setDestinations] = createSignal<Map<
		DestinationKind,
		Destination[]
	> | null>(null);
	let generation = 0;
	let currentGeneration = 0;
	let pendingTimer: ReturnType<typeof window.setTimeout> | undefined;
	let inflight:
		| {
				gen: number;
				promise: Promise<Map<DestinationKind, Destination[]> | null>;
				applied: boolean;
		  }
		| undefined;
	let inputRef: HTMLInputElement | undefined;
	let wrapperRef: HTMLDivElement | undefined;
	let restoreTo: HTMLElement | null = null;

	function queryNow(
		input: string,
		mine: number,
	): Promise<Map<DestinationKind, Destination[]> | null> {
		const promise = queryDestinations(
			providers,
			input,
			mine,
			() => currentGeneration,
		)
			.then((result) => {
				if (mine !== currentGeneration || result === null) return null;
				setDestinations(result);
				return result;
			})
			.catch(() => {
				if (mine === currentGeneration) setDestinations(null);
				return null;
			});
		inflight = { gen: mine, promise, applied: false };
		void promise.then(() => {
			if (inflight?.promise === promise) inflight.applied = true;
		});
		return promise;
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
		run: () => queueMicrotask(() => inputRef?.focus()),
	});
	onCleanup(() => {
		store.keyboard.registry.unregister(FOCUS_GLOBAL);
		store.keyboard.unregisterGroup(focusGroup);
	});

	function selectFirst(
		result: Map<DestinationKind, Destination[]> | null,
	): void {
		const first = result ? destinationSurfaceRows(result)[0] : undefined;
		if (first) select(first);
	}

	async function handleEnter(): Promise<void> {
		if (!query().trim()) return;
		if (pendingTimer !== undefined) {
			clearTimeout(pendingTimer);
			pendingTimer = undefined;
			selectFirst(await queryNow(query(), currentGeneration));
			return;
		}
		const active = inflight;
		if (active?.gen === currentGeneration && !active.applied) {
			selectFirst(await active.promise);
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
				role="combobox"
				aria-label="Global search"
				aria-expanded={
					focused() && query().trim().length > 0 ? "true" : "false"
				}
				aria-controls="topbar-search-listbox"
				aria-activedescendant={
					rows().length > 0
						? `topbar-search-option-${rows()[0].key}`
						: undefined
				}
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
				<div
					id="topbar-search-listbox"
					class="topbar-search-panel"
					role="listbox"
				>
					<For each={rows()}>
						{(row, index) => (
							<>
								<Show when={row.groupStart}>
									<div class="topbar-search-group" role="presentation">
										{row.groupLabel}
									</div>
								</Show>
								<button
									id={`topbar-search-option-${row.key}`}
									class="topbar-search-row"
									type="button"
									role="option"
									tabindex={-1}
									aria-selected={index() === 0 ? "true" : "false"}
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
