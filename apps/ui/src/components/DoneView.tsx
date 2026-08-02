import { type Component, For, Show } from "solid-js";
import {
	ciBadge,
	isMultiForge,
	issueKey,
	prBadge,
	primaryPr,
	reviewBadge,
} from "../board-render";
import { useStore } from "../context";
import type { Issue } from "../stub-data";

/** A single Done/Archived row. Mirrors the IssueCard shape but laid out as a
 *  wide list row: issue id, title, priority accent, PR summary, and the
 *  merge/branch line. Clicking selects the issue (syncs the roster) without
 *  leaving the board surface. When `archived` is false an Archive button moves
 *  the issue to the terminal `archived` state; when true the row is marker-only. */
const DoneRow: Component<{ issue: Issue; archived: boolean }> = (props) => {
	const store = useStore();
	const pr = () => primaryPr(props.issue);
	const key = () => issueKey(props.issue, isMultiForge(store.issues()));
	return (
		<li class="done-row" data-priority={props.issue.priority}>
			<button
				type="button"
				class="done-row-main"
				classList={{ selected: props.issue.id === store.selectedIssueId() }}
				onClick={() => store.selectIssue(props.issue.id)}
			>
				<span class="done-row-top">
					<span class="card-issue">{key()}</span>
					<span class="done-row-priority">{props.issue.priority}</span>
					<Show when={pr()}>
						{(p) => (
							<span class="card-pr" data-pr-state={prBadge(p())}>
								<Show when={p().checks}>
									<span class="ci-badge" data-status={ciBadge(p())} />
								</Show>
								<Show when={reviewBadge(p())}>
									{(verdict) => (
										<span class="review-badge" data-verdict={verdict()} />
									)}
								</Show>
								#{p().number} {prBadge(p())}
							</span>
						)}
					</Show>
				</span>
				<span class="done-row-title">{props.issue.title}</span>
				<span class="done-row-foot">
					<span class="done-row-branch">{props.issue.branch}</span>
					<Show
						when={pr()}
						fallback={<span class="done-row-merge">no PR</span>}
					>
						{(p) => (
							<span class="done-row-merge">
								{prBadge(p()) === "merged" ? "merged" : `PR ${prBadge(p())}`} ·{" "}
								{p().threads.filter((t) => t.resolved).length}/
								{p().threads.length} threads
							</span>
						)}
					</Show>
					<Show when={pr()?.changed} keyed>
						{(changed) => (
							<Show when={changed.files > 0}>
								<span class="card-diff">
									<span class="add">+{changed.additions}</span>
									<span class="del">−{changed.deletions}</span>
								</span>
							</Show>
						)}
					</Show>
				</span>
			</button>
			<Show when={!props.archived}>
				<button
					type="button"
					class="done-archive-btn"
					onClick={() => store.archiveIssue(props.issue.id)}
				>
					Archive
				</button>
			</Show>
		</li>
	);
};

/** The Done / archive view (T5 / D4). Two sections read reactively off the
 *  board, partitioned by `state` (DL-071): Done — `state === "done"`, each with
 *  an Archive action — and Archived — `state === "archived"`, marker only.
 *  Archive is a lifecycle transition, not a delete, so an issue stays listed
 *  here after archiving, moving from the first section to the second. */
export const DoneView: Component = () => {
	const store = useStore();

	const done = () => store.issues().filter((w) => w.state === "done");
	const archived = () => store.issues().filter((w) => w.state === "archived");

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
							{(ws) => <DoneRow issue={ws} archived={false} />}
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
					fallback={<p class="done-empty">No archived issues yet.</p>}
				>
					<ul class="done-list">
						<For each={archived()}>
							{(ws) => <DoneRow issue={ws} archived={true} />}
						</For>
					</ul>
				</Show>
			</div>
		</section>
	);
};
