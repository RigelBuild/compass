import { Popover } from "@kobalte/core/popover";
import type { JSX } from "@solidjs/web";
import {
	type Component,
	createEffect,
	createSignal,
	For,
	onSettled,
	Show,
} from "solid-js";
import { useStore } from "../context";
import "../design/components/tour.css";
import { TOUR_STEPS, type TourStep } from "../tour/state";

/** How long a callout waits for its `data-tour` anchor before the step skips. */
export const ANCHOR_WAIT_MS = 300;
const SPOTLIGHT_PADDING = 8;
const CHASE_CELLS = Array.from({ length: 24 }, (_, index) => index);

// Cells walk the frame clockwise: 8 top, 4 right, 8 bottom, 4 left.
function chaseCellStyle(index: number): JSX.CSSProperties {
	const size = { "--i": index } as JSX.CSSProperties;
	if (index < 8)
		return {
			...size,
			left: `${(index / 8) * 100}%`,
			top: "0%",
			width: "12.5%",
			height: "2px",
		};
	if (index < 12)
		return {
			...size,
			left: "100%",
			top: `${(index - 8) * 25}%`,
			width: "2px",
			height: "25%",
		};
	if (index < 20)
		return {
			...size,
			left: `${((19 - index) / 8) * 100}%`,
			top: "100%",
			width: "12.5%",
			height: "2px",
		};
	return {
		...size,
		left: "0%",
		top: `${(23 - index) * 25}%`,
		width: "2px",
		height: "25%",
	};
}

const TourDialog: Component<{ step: TourStep; index: number }> = (props) => {
	const store = useStore();
	let dialogRef: HTMLDivElement | undefined;
	let restoreTo: HTMLElement | null = null;

	onSettled(() => {
		restoreTo =
			document.activeElement instanceof HTMLElement
				? document.activeElement
				: null;
		const first = dialogRef?.querySelector<HTMLElement>("button");
		first?.focus();
		return () => {
			if (restoreTo?.isConnected) restoreTo.focus();
		};
	});

	const onKeyDown = (event: KeyboardEvent): void => {
		if (event.key === "Escape") {
			event.preventDefault();
			store.tour.close();
			return;
		}
		if (event.key !== "Tab") return;
		const items = dialogRef
			? Array.from(
					dialogRef.querySelectorAll<HTMLElement>(
						"button:not([disabled]), a[href], [tabindex]:not([tabindex='-1'])",
					),
				)
			: [];
		const first = items[0];
		const last = items[items.length - 1];
		if (!first || !last) {
			event.preventDefault();
		} else if (event.shiftKey && document.activeElement === first) {
			event.preventDefault();
			last.focus();
		} else if (!event.shiftKey && document.activeElement === last) {
			event.preventDefault();
			first.focus();
		}
	};

	return (
		<div class="cx-dialog-backdrop cx-tour-backdrop">
			<div
				ref={dialogRef}
				class="cx-dialog cx-tour-dialog"
				role="dialog"
				aria-modal="true"
				aria-label="Compass tour"
				tabindex={-1}
				onKeyDown={onKeyDown}
			>
				<Show when={props.step.id === "welcome"}>
					<div class="cx-tour-chase" aria-hidden="true">
						<For each={CHASE_CELLS}>
							{(index) => <span style={chaseCellStyle(index)} />}
						</For>
					</div>
				</Show>
				<h2 class="cx-tour-title">{props.step.title}</h2>
				<p class="cx-tour-body">{props.step.body}</p>
				<div class="cx-tour-footer">
					<span class="cx-tour-step-counter">
						{props.index + 1} of {TOUR_STEPS.length}
					</span>
					<div class="cx-tour-actions">
						<Show when={props.index > 0}>
							<button type="button" onClick={() => store.tour.back()}>
								Back
							</button>
						</Show>
						<button type="button" onClick={() => store.tour.dismiss()}>
							Skip tour
						</button>
						<button
							type="button"
							class="cx-btn"
							onClick={() => store.tour.next()}
						>
							{props.step.id === "finale"
								? "Done"
								: props.step.id === "welcome"
									? "Start"
									: "Next"}
						</button>
					</div>
				</div>
			</div>
		</div>
	);
};

export const TourOverlay: Component = () => {
	const store = useStore();
	const [anchor, setAnchor] = createSignal<HTMLElement>();
	const [spotlightStyle, setSpotlightStyle] = createSignal({
		"--cx-tour-cutout-x": "0px",
		"--cx-tour-cutout-y": "0px",
		"--cx-tour-cutout-width": "0px",
		"--cx-tour-cutout-height": "0px",
	});
	const step = () => TOUR_STEPS[store.tour.stepIndex()];

	createEffect(
		() => ({ open: store.tour.open(), current: step() }),
		({ open, current }) => {
			setAnchor(undefined);
			if (!open || current?.kind !== "callout" || !current.anchor) return;

			let settled = false;
			let timeout = 0;
			const observer = new MutationObserver(() => resolveAnchor());
			const finish = (resolved?: HTMLElement) => {
				if (settled) return;
				settled = true;
				observer.disconnect();
				window.clearTimeout(timeout);
				if (resolved) {
					// Anchors can sit below the fold (the demo agent ends the tree).
					resolved.scrollIntoView({ block: "nearest" });
					setAnchor(resolved);
				} else store.tour.next();
			};
			const resolveAnchor = () => {
				// Background views stay mounted but hidden; anchor in the shown one.
				const resolved = [
					...document.querySelectorAll<HTMLElement>(
						`[data-tour="${CSS.escape(current.anchor ?? "")}"]`,
					),
				].find((el) => el.closest("[hidden], [inert]") === null);
				if (resolved) finish(resolved);
			};
			observer.observe(document.documentElement, {
				childList: true,
				subtree: true,
				attributeFilter: ["hidden", "inert"],
			});
			resolveAnchor();
			timeout = window.setTimeout(() => finish(), ANCHOR_WAIT_MS);
			return () => {
				settled = true;
				observer.disconnect();
				window.clearTimeout(timeout);
			};
		},
	);

	createEffect(
		() => anchor(),
		(target) => {
			if (!target) return;
			let frame = 0;
			const update = () => {
				if (frame !== 0) return;
				frame = window.requestAnimationFrame(() => {
					frame = 0;
					if (!target.isConnected) return;
					const rect = target.getBoundingClientRect();
					setSpotlightStyle({
						"--cx-tour-cutout-x": `${rect.left - SPOTLIGHT_PADDING}px`,
						"--cx-tour-cutout-y": `${rect.top - SPOTLIGHT_PADDING}px`,
						"--cx-tour-cutout-width": `${rect.width + SPOTLIGHT_PADDING * 2}px`,
						"--cx-tour-cutout-height": `${rect.height + SPOTLIGHT_PADDING * 2}px`,
					});
				});
			};
			update();
			window.addEventListener("resize", update);
			window.addEventListener("scroll", update, true);
			return () => {
				window.removeEventListener("resize", update);
				window.removeEventListener("scroll", update, true);
				if (frame !== 0) window.cancelAnimationFrame(frame);
			};
		},
	);

	return (
		<Show when={store.tour.open() && step()} keyed>
			{(current) => (
				<>
					<Show when={current.kind === "callout"}>
						<div
							class="cx-tour-spotlight"
							aria-hidden="true"
							style={{ ...spotlightStyle(), "pointer-events": "none" }}
						/>
						{/* Unmount under the shortcuts overlay so the callout leaves Kobalte's
						    layer stack and Escape reaches only the overlay. */}
						<Show when={anchor() && !store.shortcutsOpen()}>
							<Popover
								open
								anchorRef={anchor}
								modal={false}
								placement="bottom-start"
								gutter={8}
								overlap
								slide
								onOpenChange={(isOpen) => {
									if (!isOpen) store.tour.close();
								}}
							>
								<Popover.Portal mount={document.body}>
									<Popover.Content
										class="cx-tour-callout"
										aria-label={current.title}
										onPointerDownOutside={(event) => event.preventDefault()}
										onInteractOutside={(event) => event.preventDefault()}
										onKeyDown={(event) => {
											if (event.key === "ArrowRight") {
												event.preventDefault();
												store.tour.next();
											} else if (event.key === "ArrowLeft") {
												event.preventDefault();
												store.tour.back();
											}
										}}
									>
										<h2 class="cx-tour-title">{current.title}</h2>
										<p class="cx-tour-body">{current.body}</p>
										<div class="cx-tour-footer">
											<span class="cx-tour-step-counter">
												{store.tour.stepIndex() + 1} of {TOUR_STEPS.length}
											</span>
											<div class="cx-tour-actions">
												<button
													type="button"
													onClick={() => store.tour.back()}
													disabled={store.tour.stepIndex() === 0}
												>
													Back
												</button>
												<button type="button" onClick={() => store.tour.next()}>
													Next
												</button>
												<button
													type="button"
													onClick={() => store.tour.dismiss()}
												>
													Skip tour
												</button>
											</div>
										</div>
									</Popover.Content>
								</Popover.Portal>
							</Popover>
						</Show>
					</Show>
					<Show when={current.kind === "dialog"}>
						<TourDialog step={current} index={store.tour.stepIndex()} />
					</Show>
				</>
			)}
		</Show>
	);
};
