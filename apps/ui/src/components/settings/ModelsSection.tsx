import {
	type Component,
	createMemo,
	createUniqueId,
	For,
	Match,
	Show,
	Switch,
} from "solid-js";
import { useStore } from "../../context";

export const ModelsSection: Component = () => {
	const store = useStore();
	const uid = createUniqueId();
	const ready = createMemo(() => {
		const state = store.modelRegistry();
		return state.status === "ready" ? state : undefined;
	});
	const error = createMemo(() => {
		const state = store.modelRegistry();
		return state.status === "error" ? state : undefined;
	});

	return (
		<section class="settings-content" aria-labelledby={`${uid}-models-title`}>
			<header class="settings-content-head">
				<h2 class="settings-heading" id={`${uid}-models-title`}>
					Models
				</h2>
				<p class="settings-description">View the configured model registry.</p>
			</header>
			<Show when={store.modelRegistry().status !== "offline"}>
				<section class="settings-group" aria-label="Model registry">
					<div class="settings-group-head">
						<h3 class="settings-group-title">Model registry</h3>
						<Show when={ready()}>
							{(state) => (
								<span class="settings-group-help">
									v{String(state().version)} · Read-only — edited via the
									operator registry RPC.
								</span>
							)}
						</Show>
					</div>
					<Switch>
						<Match when={store.modelRegistry().status === "pending"}>
							<span class="settings-group-help">Loading…</span>
						</Match>
						<Match when={error()}>
							{(state) => <span role="alert">{state().message}</span>}
						</Match>
						<Match when={ready()}>
							{(state) => (
								<Show
									when={state().entries.length > 0}
									fallback={
										<p class="settings-empty">No model registry configured.</p>
									}
								>
									<ul class="settings-registry">
										<For each={state().entries}>
											{(row) => (
												<li class="settings-registry-row">
													<span class="settings-registry-name">
														{row.stableName}
													</span>
													<span class="settings-registry-display">
														{row.displayName}
													</span>
													<span class="settings-registry-candidates">
														<For each={row.candidates}>
															{(candidate, index) => (
																<span class="settings-registry-candidate">
																	<Show when={index() > 0}>
																		<span
																			class="settings-map-arrow"
																			aria-hidden="true"
																		>
																			→
																		</span>
																	</Show>
																	{candidate.provider}/{candidate.modelId}
																</span>
															)}
														</For>
													</span>
												</li>
											)}
										</For>
									</ul>
								</Show>
							)}
						</Match>
					</Switch>
				</section>
			</Show>
		</section>
	);
};
