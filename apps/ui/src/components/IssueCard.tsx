import { type Component, Show } from "solid-js";
import {
	ciBadge,
	isMultiForge,
	issueKey,
	primaryPr,
	reviewBadge,
} from "../board-render";
import { useStore } from "../context";
import type { Issue } from "../stub-data";
import { BadgeGlyph } from "./BadgeGlyph";

/** A single issue card — used in the Bridge swimlane cells. Single-click selects
 *  the issue (syncing the roster) without leaving the board; double-click jumps
 *  into the assigned agent's view (design D10). A card with no assignee has no
 *  jump target, so double-click falls back to select. When `onOpenPr` is passed
 *  (by the Bridge), the PR chip becomes an interactive cross-link to the PRs
 *  tab; without it the chip stays inert (a card rendered elsewhere). */
export const IssueCard: Component<{
	issue: Issue;
	onOpenPr?: () => void;
	/** Dormant hook (T5, D2): when a future store accessor marks this card as
	 *  advancing a column, it renders `data-advancing="1"` and the board paints
	 *  the chase-light. No live data sets it yet (the Issue model carries no
	 *  transition timestamp), so it is unwired — every caller omits it today. */
	advancing?: boolean;
	/** Board wiring (T4, DL-221): when the card is a stop in the Bridge's
	 *  roving-tabindex group, its PR chip must not be a second Tab stop — the
	 *  board owns keyboard nav and the cross-link moves to `board.openCardCrossLink`
	 *  (Space). True demotes the chip to `tabindex={-1}`; the pointer + chip
	 *  keydown handlers stay, so a non-board host (chip omitted) is unaffected. */
	inRovingGroup?: boolean;
	/** Board wiring (T4, design §179-182): when the card is a stop in the
	 *  Bridge's roving group, the board collects its `<button>` element (the stop
	 *  the design mandates carries the managed `tabindex`). A ref-forwarding
	 *  callback the board passes per stop; omitted by every non-board host. */
	cardRef?: (el: HTMLButtonElement) => void;
}> = (props) => {
	const store = useStore();
	const openAssignedAgent = () => {
		const agentId = props.issue.assignee;
		if (agentId) store.openAgent(agentId);
	};
	const pr = () => primaryPr(props.issue);
	const key = () => issueKey(props.issue, isMultiForge(store.issues()));
	// The card foot shows the issue's ASSIGNEE — the agent currently on it —
	// deliberately a different fact from the PR pane's artifact author
	// (RightSidebar's authorLabel over pr.agent): a reassigned issue's card
	// should name its current holder, not the original author. The two author
	// surfaces are intended to diverge, so the card does not go through the
	// authorLabel seam. (Confirm-with-Matt parked in the PR's Open Questions.)
	// The assignee is a trusted Compass account id, so agentView resolves it; a
	// miss is a real store bug, surfaced as the raw id rather than disguised as a
	// plausible handle.
	const assignee = () => {
		const id = props.issue.assignee;
		if (!id) return undefined;
		return store.agentView(id)?.account.handle ?? id;
	};
	return (
		<button
			type="button"
			class="cx-card"
			data-selected={
				props.issue.id === store.selectedIssueId() ? "" : undefined
			}
			data-advancing={props.advancing ? "1" : undefined}
			onClick={() => store.selectIssue(props.issue.id)}
			onDblClick={openAssignedAgent}
			ref={(el) => props.cardRef?.(el)}
		>
			<span class="card-top">
				<span class="card-issue">{key()}</span>
				<Show when={pr()}>
					{(p) => (
						/* biome-ignore lint/a11y/noStaticElementInteractions: the chip lives inside
						   the card <button>, so a nested button/link is disallowed — a role="link"
						   span with keyboard + stopPropagation is the content-model compromise
						   (DL-097 §2); inert when onOpenPr is absent. */
						<span
							class={["card-pr", { link: props.onOpenPr !== undefined }]}
							role={props.onOpenPr ? "link" : undefined}
							tabindex={
								props.onOpenPr ? (props.inRovingGroup ? -1 : 0) : undefined
							}
							onClick={(e) => {
								if (!props.onOpenPr) return;
								e.stopPropagation();
								props.onOpenPr();
							}}
							onKeyDown={(e) => {
								if (!props.onOpenPr) return;
								if (e.key !== "Enter" && e.key !== " ") return;
								e.preventDefault();
								e.stopPropagation();
								props.onOpenPr();
							}}
						>
							<Show when={ciBadge(p())}>
								{(status) => <BadgeGlyph axis="ci" status={status()} compact />}
							</Show>
							<Show when={reviewBadge(p())}>
								{(verdict) => (
									<BadgeGlyph axis="review" status={verdict()} compact />
								)}
							</Show>
							#{p().number}
						</span>
					)}
				</Show>
			</span>
			<span class="card-title">{props.issue.title}</span>
			<span class="card-foot">
				<span class="card-author">
					{assignee() ? `@${assignee()}` : "unassigned"}
				</span>
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
	);
};
