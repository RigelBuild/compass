// Show the live keymap and registry in a modal with focus management.

import {
	type Component,
	createMemo,
	createSignal,
	For,
	onSettled,
	Show,
} from "solid-js";
import { useStore } from "../context";
import "../design/components/input.css";
import { detectPlatform } from "../keyboard/dispatch";
import { DEFAULT_KEYMAP } from "../keyboard/keymap";
import { buildShortcutGroups } from "../keyboard/shortcuts-model";

export const ShortcutsOverlay: Component = () => {
	const store = useStore();
	const [query, setQuery] = createSignal("");

	const groups = createMemo(() =>
		buildShortcutGroups(
			DEFAULT_KEYMAP,
			store.keyboard.registry,
			detectPlatform(),
			query(),
		),
	);

	let dialogRef: HTMLDivElement | undefined;
	let searchRef: HTMLInputElement | undefined;
	// Save prior focus, focus search, and restore while the element stays connected.
	let restoreTo: HTMLElement | null = null;
	onSettled(() => {
		restoreTo =
			document.activeElement instanceof HTMLElement
				? document.activeElement
				: null;
		searchRef?.focus();
		return () => {
			if (restoreTo?.isConnected) restoreTo.focus();
		};
	});

	// Trap Tab navigation so focus stays inside the modal dialog.
	const focusables = (): HTMLElement[] =>
		dialogRef
			? Array.from(
					dialogRef.querySelectorAll<HTMLElement>(
						'a[href], button:not([disabled]), input:not([disabled]), [tabindex]:not([tabindex="-1"])',
					),
				)
			: [];

	const onKeyDown = (event: KeyboardEvent): void => {
		if (event.key === "Escape") {
			event.preventDefault();
			store.hideShortcuts();
			return;
		}
		if (event.key !== "Tab") return;
		const items = focusables();
		if (items.length === 0) {
			event.preventDefault();
			return;
		}
		const first = items[0];
		const last = items[items.length - 1];
		const active = document.activeElement;
		if (event.shiftKey && active === first) {
			event.preventDefault();
			last?.focus();
		} else if (!event.shiftKey && active === last) {
			event.preventDefault();
			first?.focus();
		}
	};

	return (
		// biome-ignore lint/a11y/useKeyWithClickEvents: scrim backdrop is a mouse-only close convenience; keyboard close is Escape on the panel.
		// biome-ignore lint/a11y/noStaticElementInteractions: scrim backdrop closes on click, mirroring the hand-rolled dialog convention.
		<div
			class="cx-dialog-backdrop cx-shortcuts-backdrop"
			onClick={() => store.hideShortcuts()}
		>
			<div
				ref={dialogRef}
				class="cx-dialog cx-shortcuts"
				role="dialog"
				aria-modal="true"
				aria-label="Keyboard shortcuts"
				tabindex={-1}
				onClick={(e) => e.stopPropagation()}
				onKeyDown={onKeyDown}
			>
				<input
					ref={searchRef}
					class="cx-search cx-shortcuts-search"
					type="text"
					placeholder="Search shortcuts"
					aria-label="Search shortcuts"
					value={query()}
					onInput={(e) => setQuery(e.currentTarget.value)}
				/>
				<div class="cx-shortcuts-scroll">
					<Show
						when={groups().length > 0}
						fallback={
							<div class="cx-shortcuts-empty">No matching shortcuts</div>
						}
					>
						<For each={groups()}>
							{(group) => (
								<div class="cx-shortcuts-group">
									<div class="cx-shortcuts-header">{group.scope}</div>
									<For each={group.rows}>
										{(row) => (
											<div class="cx-shortcuts-row">
												<span class="cx-shortcuts-title">{row.title}</span>
												<span class="cx-shortcuts-chord">{row.chord}</span>
											</div>
										)}
									</For>
								</div>
							)}
						</For>
					</Show>
				</div>
			</div>
		</div>
	);
};
