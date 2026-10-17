import {
	type Component,
	createEffect,
	createSignal,
	onCleanup,
	Show,
} from "solid-js";
import { useStore } from "../context";
import { safeHref } from "../safe-url";
import type { MirrorToForgeSource } from "../store";

/** The "Mirror to issue" control on an ask or topic: an inline ref field (no
 *  modal) that posts through `MirrorToForge` and shows the comment URL or the
 *  server's refusal beside the source. */
export const MirrorControl: Component<{
	channelId: string;
	source: MirrorToForgeSource;
	initialRef?: string;
}> = (props) => {
	const store = useStore();
	const [open, setOpen] = createSignal(false);
	const [ref, setRef] = createSignal("");
	const [busy, setBusy] = createSignal(false);
	const [error, setError] = createSignal<string>();
	const [url, setUrl] = createSignal<string>();
	const sourceKey = () => {
		const source = props.source;
		return source.case === "ask"
			? `${props.channelId}:ask:${source.askId}`
			: `${props.channelId}:topic:${source.topicId}`;
	};
	let generation = 0;
	let alive = true;
	createEffect(sourceKey, (key, previous) => {
		if (previous !== undefined && key !== previous) {
			generation += 1;
			setOpen(false);
			setRef("");
			setBusy(false);
			setError(undefined);
			setUrl(undefined);
		}
	});
	onCleanup(() => {
		alive = false;
		generation += 1;
	});
	const isCurrent = (mine: number, key: string) =>
		alive && mine === generation && key === sourceKey();
	const completeMirror = async (
		mine: number,
		key: string,
		channelId: string,
		source: MirrorToForgeSource,
		issueRef: string,
	) => {
		try {
			const commentUrl = await store.mirrorToForge(channelId, issueRef, source);
			if (isCurrent(mine, key)) setUrl(commentUrl);
		} catch (cause) {
			if (isCurrent(mine, key)) {
				setError(cause instanceof Error ? cause.message : String(cause));
			}
		} finally {
			if (isCurrent(mine, key)) setBusy(false);
		}
	};
	const submit = async (event: SubmitEvent) => {
		event.preventDefault();
		if (busy()) return;
		const mine = ++generation;
		const key = sourceKey();
		const channelId = props.channelId;
		const source = props.source;
		const issueRef = ref().trim();
		setBusy(true);
		setError(undefined);
		setUrl(undefined);
		await completeMirror(mine, key, channelId, source, issueRef);
	};
	return (
		<div class="mirror-control">
			<button
				type="button"
				class="cx-btn"
				aria-label="Mirror to issue"
				onClick={() => {
					if (!open()) setRef(props.initialRef ?? "");
					setOpen((previous) => !previous);
				}}
			>
				Mirror to issue
			</button>
			<Show when={open()}>
				<form class="mirror-form" onSubmit={submit}>
					<label>
						Issue or PR ref
						<input
							type="text"
							class="cx-input"
							aria-label="Issue or PR ref"
							value={ref()}
							onInput={(event) => setRef(event.currentTarget.value)}
						/>
					</label>
					<button type="submit" class="cx-btn mirror-submit" disabled={busy()}>
						Mirror
					</button>
				</form>
			</Show>
			<Show when={error()}>
				{(message) => (
					<span class="mirror-error" role="alert">
						{message()}
					</span>
				)}
			</Show>
			{/* Mounted before the result so a screen reader announces the change. */}
			<span class="mirror-result" role="status">
				<Show when={safeHref(url())}>
					{(target) => (
						<a href={target()} target="_blank" rel="noreferrer noopener">
							View comment
						</a>
					)}
				</Show>
				{url() && !safeHref(url()) ? "Mirrored" : ""}
			</span>
		</div>
	);
};
