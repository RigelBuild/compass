import { type Component, createSignal, Show } from "solid-js";
import { useStore } from "../context";
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
	const submit = async (event: SubmitEvent) => {
		event.preventDefault();
		if (busy()) return;
		setBusy(true);
		setError(undefined);
		setUrl(undefined);
		try {
			setUrl(
				await store.mirrorToForge(props.channelId, ref().trim(), props.source),
			);
		} catch (cause) {
			setError(cause instanceof Error ? cause.message : String(cause));
		} finally {
			setBusy(false);
		}
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
			<Show when={url()}>
				{(target) => (
					<span class="mirror-result">
						<a href={target()} target="_blank" rel="noreferrer">
							View comment
						</a>
					</span>
				)}
			</Show>
		</div>
	);
};
