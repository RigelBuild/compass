// Keyboard-shortcuts overlay (RIG-2482) — the `?`-opened, searchable reference
// sheet. The content is a render-time JOIN of DEFAULT_KEYMAP × the live command
// registry (buildShortcutGroups), so it can never drift from the real bindings;
// this component only renders that model + owns the modal chrome.
//
// Hand-rolled modal on the `.cx-dialog` convention (StartAgentDialog precedent,
// ratified D5 — no @kobalte/core), with two correctness additions the precedent
// lacks: focus-RESTORE (capture activeElement on mount, restore on cleanup) and
// a minimal Tab/Shift+Tab focus TRAP, so a modal claiming aria-modal cannot let
// focus walk out and leave its local Escape handler unreachable.

import {
	type Component,
	createMemo,
	createSignal,
	For,
	onCleanup,
	onMount,
	Show,
} from "solid-js";
import { useStore } from "../context";
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
	// The element focused before the overlay opened; focus returns here on close
	// (every close path — `?` toggle, Escape, backdrop, close-on-navigation).
	let restoreTo: HTMLElement | null = null;

	onMount(() => {
		restoreTo =
			document.activeElement instanceof HTMLElement
				? document.activeElement
				: null;
		searchRef?.focus();
	});
	// Restore only if the captured element is still in the DOM: a route change
	// during the overlay's lifetime can detach it, and .focus() on a detached
	// node silently drops focus to <body>. When disconnected, leave focus alone.
	onCleanup(() => {
		if (restoreTo?.isConnected) restoreTo.focus();
	});

	// Minimal focus trap: keep Tab/Shift+Tab inside the dialog so focus cannot
	// leave behind aria-modal="true" and the local Escape stays reachable.
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
