import { type Component, createSignal, For, Show } from "solid-js";
import { useStore } from "../context";
import type { Workstream } from "../stub-data";

/** A single Linear-style issue row: id · title · priority · state · tracker.
 *  Clicking the row selects the workstream (staying on the view); Backlog rows
 *  also get a Promote → Todo action. Not the board `WorkstreamCard` — that's a
 *  swimlane card and can't nest an action button. */
const BacklogRow: Component<{ ws: Workstream }> = (props) => {
	const store = useStore();
	return (
		<li class="backlog-row">
			<button
				type="button"
				class="backlog-row-main"
				data-priority={props.ws.priority}
				classList={{ selected: props.ws.id === store.selectedWorkstreamId() }}
				onClick={() => store.selectWorkstream(props.ws.id)}
			>
				<span class="backlog-id">{props.ws.issue}</span>
				<span class="backlog-title">{props.ws.title}</span>
				<span class="backlog-priority" data-priority={props.ws.priority}>
					{props.ws.priority}
				</span>
				<span class="backlog-state" data-state={props.ws.state}>
					{props.ws.state}
				</span>
				<Show when={props.ws.tracker}>
					{(tracker) => (
						<span class="backlog-tracker" data-kind={tracker().kind}>
							{tracker().id}
						</span>
					)}
				</Show>
			</button>
			<Show when={props.ws.state === "backlog"}>
				<button
					type="button"
					class="backlog-promote"
					onClick={() => store.promoteToTodo(props.ws.id)}
				>
					Promote → Todo
				</button>
			</Show>
		</li>
	);
};

/** A collapsible section with a count badge and an empty-state line. Collapse is
 *  component-local UI state (`createSignal`) — no store state (design D3). */
const BacklogSection: Component<{
	title: string;
	rows: Workstream[];
	empty: string;
}> = (props) => {
	const [open, setOpen] = createSignal(true);
	// Stable id tying the toggle to the region it controls, so screen readers
	// announce the collapse relationship (aria-expanded alone doesn't).
	const contentId = `backlog-section-${props.title
		.toLowerCase()
		.replace(/\s+/g, "-")}`;
	return (
		<section class="backlog-section">
			<button
				type="button"
				class="backlog-section-head"
				aria-expanded={open()}
				aria-controls={contentId}
				onClick={() => setOpen(!open())}
			>
				<span class="backlog-chevron" classList={{ open: open() }}>
					▸
				</span>
				<span class="backlog-section-title">{props.title}</span>
				<span class="backlog-count">{props.rows.length}</span>
			</button>
			<Show when={open()}>
				<div id={contentId}>
					<Show
						when={props.rows.length > 0}
						fallback={<p class="backlog-empty sub">{props.empty}</p>}
					>
						<ul class="backlog-list">
							<For each={props.rows}>{(ws) => <BacklogRow ws={ws} />}</For>
						</ul>
					</Show>
				</div>
			</Show>
		</section>
	);
};

/** The Backlog view (design D3/T4): a Linear-style vertical issue list with
 *  three collapsible sections — Todo (the global promoted-but-unassigned pool),
 *  Backlog (the un-promoted tier), and Assigned to me (the user's personal
 *  tracker queue). All lists read reactively through the store. */
export const BacklogView: Component = () => {
	const store = useStore();

	const todo = () => store.workstreams().filter((w) => w.state === "todo");
	const backlog = () =>
		store.workstreams().filter((w) => w.state === "backlog");

	return (
		<section class="backlog-view" aria-label="Backlog">
			<h2 class="heading">Backlog</h2>
			<BacklogSection
				title="Todo"
				rows={todo()}
				empty="No promoted issues waiting for an agent."
			/>
			<BacklogSection
				title="Backlog"
				rows={backlog()}
				empty="Backlog is empty — nothing waiting to be promoted."
			/>
			<BacklogSection
				title="Assigned to me"
				rows={store.assignedIssues()}
				empty="No tracker issues assigned to you."
			/>
		</section>
	);
};
