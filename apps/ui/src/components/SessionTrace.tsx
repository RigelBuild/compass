import { type Component, For, Show } from "solid-js";
import { type DiffRow, diffRows } from "../line-diff";
import { safeHref } from "../safe-url";
import type { FileDiff, TraceItem } from "../session-events";

const DiffBlock: Component<{ diff: FileDiff }> = (props) => {
	const rows = (): DiffRow[] =>
		diffRows(props.diff.oldText, props.diff.newText);
	return (
		<div class="block-diff">
			<div class="diff-path">{props.diff.path}</div>
			<For each={rows()}>
				{(row) => (
					<div class="diff-line" data-kind={row.kind}>
						<span class="diff-gutter">{row.kind === "add" ? "+" : "-"}</span>
						<span class="diff-body">{row.text}</span>
					</div>
				)}
			</For>
		</div>
	);
};

/** One tool call row: a status dot (proto vocab data-status), the call title
 *  (falling back to the toolCallId for an orphan update), an optional output
 *  disclosure, and one diff block per carried diff. */
const ToolRow: Component<{ item: Extract<TraceItem, { kind: "tool" }> }> = (
	props,
) => (
	<div class="block-tool">
		<span
			class="tool-status"
			data-status={props.item.status}
			aria-hidden="true"
		/>
		<span class="tool-title">
			{props.item.call?.title ?? props.item.toolCallId}
		</span>
		<Show when={props.item.output}>
			{(output) => (
				<details class="tool-output">
					<summary class="tool-detail">output</summary>
					<pre class="tool-output-body">{output()}</pre>
				</details>
			)}
		</Show>
		<For each={props.item.diffs}>{(diff) => <DiffBlock diff={diff} />}</For>
	</div>
);

/** One notice row: the notice text, plus a link-out anchor when the event
 *  carries a link. */
const NoticeRow: Component<{ item: Extract<TraceItem, { kind: "notice" }> }> = (
	props,
) => {
	const notice = () => {
		const e = props.item.event;
		return e.kind === "notice" ? e : undefined;
	};
	const safeLink = () => safeHref(notice()?.link);
	return (
		<div class="block-notice">
			<span class="notice-text">{notice()?.text ?? ""}</span>
			<Show when={safeLink()}>
				{(href) => (
					<a class="notice-link" href={href()} target="_blank" rel="noreferrer">
						open ↗
					</a>
				)}
			</Show>
		</div>
	);
};

/** The typed session trace: walks the folded TraceItem[] and renders one row per
 *  item kind, reusing the existing block CSS (design compass-0.8 §440-478). The
 *  fold (foldSession) runs upstream; this component is pure presentation. */
export const SessionTrace: Component<{ items: TraceItem[] }> = (props) => (
	<For each={props.items}>
		{(item) => {
			switch (item.kind) {
				case "text":
					return <div class="block-text">{item.text}</div>;
				case "thinking":
					return <div class="block-thinking">{item.text}</div>;
				case "tool":
					return <ToolRow item={item} />;
				case "plan":
					return (
						<div class="block-plan">
							<For each={item.entries}>
								{(entry) => (
									<div class="plan-step" data-status={entry.status}>
										<span class="plan-mark" aria-hidden="true" />
										<span class="plan-content">{entry.content}</span>
									</div>
								)}
							</For>
						</div>
					);
				case "notice":
					return <NoticeRow item={item} />;
			}
		}}
	</For>
);
