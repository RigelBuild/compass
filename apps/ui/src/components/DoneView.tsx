import { type Component, For, Show } from "solid-js";
import { useStore } from "../context";
import type { Workstream } from "../stub-data";

/** A single Done/Archived row. Mirrors the WorkstreamCard shape but laid out as
 *  a wide list row: issue id, title, priority accent, PR summary, and the
 *  merge/branch line. Clicking selects the workstream (syncs the roster) without
 *  leaving the board surface. When `archived` is false an Archive button stamps
 *  `archivedAt`; when true the row shows the archive marker instead. */
const DoneRow: Component<{ ws: Workstream; archived: boolean }> = (props) => {
	const store = useStore();
	return (
		<li class="done-row" data-priority={props.ws.priority}>
			<button
				type="button"
				class="done-row-main"
				classList={{ selected: props.ws.id === store.selectedWorkstreamId() }}
				onClick={() => store.selectWorkstream(props.ws.id)}
			>
				<span class="done-row-top">
					<span class="card-issue">{props.ws.issue}</span>
					<span class="done-row-priority">{props.ws.priority}</span>
					<Show when={props.ws.pr}>
						{(pr) => (
							<span class="card-pr" data-pr-state={pr().state}>
								<span class="check-pips">
									<For each={pr().checks}>
										{(c) => <span class="check-pip" data-status={c.status} />}
									</For>
								</span>
								#{pr().number} {pr().state}
							</span>
						)}
					</Show>
				</span>
				<span class="done-row-title">{props.ws.title}</span>
				<span class="done-row-foot">
					<span class="done-row-branch">{props.ws.branch}</span>
					<Show
						when={props.ws.pr}
						fallback={<span class="done-row-merge">no PR</span>}
					>
						{(pr) => (
							<span class="done-row-merge">
								{pr().state === "merged" ? "merged" : `PR ${pr().state}`} ·{" "}
								{pr().threads.resolved}/{pr().threads.total} threads
							</span>
						)}
					</Show>
					<Show when={props.ws.changed.files > 0}>
						<span class="card-diff">
							<span class="add">+{props.ws.changed.additions}</span>
							<span class="del">−{props.ws.changed.deletions}</span>
						</span>
					</Show>
				</span>
			</button>
			<Show
				when={props.archived}
				fallback={
					<button
						type="button"
						class="done-archive-btn"
						onClick={() => store.archiveWorkstream(props.ws.id)}
					>
						Archive
					</button>
				}
			>
				<span class="done-archived-mark" title={props.ws.archivedAt}>
					archived {props.ws.archivedAt?.slice(0, 10)}
				</span>
			</Show>
		</li>
	);
};

/** The Done / archive view (T5 / D4). Two sections read reactively off the
 *  board: Done (active) — `done` and not yet archived, each with an Archive
 *  action — and Archived — the `archivedAt`-stamped ones, marker only. Archive
 *  is a marker, not a delete, so a workstream stays listed here after archiving,
 *  moving from the first section to the second. */
export const DoneView: Component = () => {
	const store = useStore();

	const done = () =>
		store.workstreams().filter((w) => w.state === "done" && !w.archivedAt);
	const archived = () => store.workstreams().filter((w) => w.archivedAt);

	return (
		<section class="done-view" aria-label="Done">
			<div class="done-section">
				<div class="done-section-head">
					<span class="heading">Done</span>
					<span class="sub">{done().length}</span>
				</div>
				<Show
					when={done().length > 0}
					fallback={<p class="done-empty">Nothing to archive.</p>}
				>
					<ul class="done-list">
						<For each={done()}>
							{(ws) => <DoneRow ws={ws} archived={false} />}
						</For>
					</ul>
				</Show>
			</div>

			<div class="done-section">
				<div class="done-section-head">
					<span class="heading">Archived</span>
					<span class="sub">{archived().length}</span>
				</div>
				<Show
					when={archived().length > 0}
					fallback={<p class="done-empty">No archived workstreams yet.</p>}
				>
					<ul class="done-list">
						<For each={archived()}>
							{(ws) => <DoneRow ws={ws} archived={true} />}
						</For>
					</ul>
				</Show>
			</div>
		</section>
	);
};
