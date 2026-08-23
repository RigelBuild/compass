import {
	type Component,
	createMemo,
	createSignal,
	createStore,
	For,
	reconcile,
	snapshot,
} from "solid-js";
import { useStore } from "../context";
import type {
	TrackerConfig,
	TrackerKind,
	WorkingIssueState,
} from "../stub-data";

/** The tracker kinds selectable in the identity picker. Linear is the live one;
 *  Jira/GitHub are wired shapes but not yet backed by a real seam. */
const TRACKER_KINDS: readonly { value: TrackerKind; label: string }[] = [
	{ value: "linear", label: "Linear" },
	{ value: "jira", label: "Jira" },
	{ value: "github", label: "GitHub" },
];

/** Compass working states in lifecycle order, with human labels — one
 *  status-mapping row each. Keyed on `WorkingIssueState` (the seven working
 *  states) so a new working state can't ship without a mapping row here;
 *  `archived` carries no tracker status (DL-071) and is excluded. */
const STATE_ROWS: readonly { state: WorkingIssueState; label: string }[] = [
	{ state: "backlog", label: "Backlog" },
	{ state: "todo", label: "Todo" },
	{ state: "queued", label: "Queued" },
	{ state: "blocked", label: "Blocked" },
	{ state: "in_progress", label: "In progress" },
	{ state: "in_review", label: "In review" },
	{ state: "done", label: "Done" },
];

/** Merge the edited forward map back into the committed reverse map
 *  (`fromTracker`), preserving it as the source of truth for the reverse
 *  direction. The reverse map is NOT reconstructible from `toTracker` alone: it
 *  carries tracker-native aliases no forward row produces (e.g. Linear's
 *  `Cancelled`/`Duplicate` → `done`), and when several Compass states share a
 *  status the canonical read-back is a deliberate pick, not last-write-wins. So
 *  we keep every existing entry and only *add* a status the forward map now
 *  targets but the reverse map doesn't yet cover — first writer in lifecycle
 *  order wins (e.g. `Todo` reads back as `todo`, not the later `queued`). */
export function mergeFromTracker(
	toTracker: Record<WorkingIssueState, string>,
	committed: Record<string, WorkingIssueState>,
): Record<string, WorkingIssueState> {
	const fromTracker: Record<string, WorkingIssueState> = { ...committed };
	for (const { state } of STATE_ROWS) {
		const status = toTracker[state];
		if (status && !(status in fromTracker)) fromTracker[status] = state;
	}
	return fromTracker;
}

/** Group the true reverse map (`fromTracker`) by the Compass state each tracker
 *  status reads back to, in lifecycle order — so the read-only view shows the
 *  real many-to-one projection, aliases included (e.g. `Done, Cancelled,
 *  Duplicate → Done`), matching exactly what Save persists. */
function reverseGroups(
	fromTracker: Record<string, WorkingIssueState>,
): { label: string; statuses: string[] }[] {
	const byState = new Map<WorkingIssueState, string[]>();
	for (const [status, state] of Object.entries(fromTracker)) {
		const statuses = byState.get(state);
		if (statuses) statuses.push(status);
		else byState.set(state, [status]);
	}
	return STATE_ROWS.filter(({ state }) => byState.has(state)).map(
		({ state, label }) => ({ label, statuses: byState.get(state) ?? [] }),
	);
}

/** The Settings screen (T11 / D8): the tracker-config + status-mapping editor.
 *  Edits land in a local draft seeded from `store.trackerConfig()` — never the
 *  store on every keystroke — and are committed via `store.setTrackerConfig`
 *  only on Save. Reset reseeds the draft from the store. The header shows the
 *  live (committed) handle + kind so it's clear what's actually wired. */
export const SettingsView: Component = () => {
	const store = useStore();

	const seed = (): TrackerConfig =>
		structuredClone(snapshot(store.trackerConfig()));
	const [draft, setDraft] = createStore<TrackerConfig>(seed());
	// Bump on every draft mutation so the Save-enabled derivation re-runs; the
	// store's fine-grained reads don't otherwise notify a whole-object compare.
	const [dirtyTick, setDirtyTick] = createSignal(0);
	const touch = () => setDirtyTick((n) => n + 1);

	const reset = () => {
		setDraft(reconcile(seed()));
		touch();
	};

	const save = () => {
		const cfg: TrackerConfig = {
			kind: draft.kind,
			handle: draft.handle,
			mapping: {
				kind: draft.kind,
				toTracker: { ...draft.mapping.toTracker },
				fromTracker: mergeFromTracker(
					draft.mapping.toTracker,
					draft.mapping.fromTracker,
				),
			},
		};
		store.setTrackerConfig(cfg);
		touch();
	};

	const dirty = () => {
		dirtyTick();
		const live = store.trackerConfig();
		if (draft.kind !== live.kind || draft.handle !== live.handle) return true;
		return STATE_ROWS.some(
			({ state }) =>
				draft.mapping.toTracker[state] !== live.mapping.toTracker[state],
		);
	};

	// A blank mapping input would persist a WorkingIssueState → "" gap that breaks
	// status-sync once the seam is backed by a real tracker, so Save is blocked
	// (Reset stays live to recover). fromTracker's empty-string skip already
	// keeps the reverse map clean; this closes the forward hole.
	const hasEmptyMapping = () =>
		STATE_ROWS.some(({ state }) => !draft.mapping.toTracker[state]);

	// Preview the reverse map the way Save will persist it: the committed
	// fromTracker merged with any pending forward edits, grouped by target
	// state. Memoized so the read-back <For> keeps stable rows across unrelated
	// keystrokes (it recomputes only when the draft mapping actually changes).
	const reverse = createMemo(() =>
		reverseGroups(
			mergeFromTracker(draft.mapping.toTracker, draft.mapping.fromTracker),
		),
	);

	return (
		<section class="settings-view" aria-label="Settings">
			<div class="settings-head">
				<span class="heading">Tracker</span>
				<span class="sub">
					{store.trackerConfig().handle} · {store.trackerConfig().kind}
				</span>
			</div>

			<div class="settings-section">
				<div class="settings-section-head">
					<span class="heading">Identity</span>
					<span class="sub">Who Compass lists assigned issues for.</span>
				</div>
				<div class="settings-fields">
					<label class="settings-field">
						<span class="settings-label">Handle</span>
						<input
							type="text"
							class="settings-input"
							value={draft.handle}
							placeholder="you@org"
							onInput={(e) => {
								const value = e.currentTarget.value;
								setDraft((s) => {
									s.handle = value;
								});
								touch();
							}}
						/>
					</label>
					<label class="settings-field">
						<span class="settings-label">Kind</span>
						<select
							class="settings-select"
							value={draft.kind}
							onInput={(e) => {
								const value = e.currentTarget.value as TrackerKind;
								setDraft((s) => {
									s.kind = value;
								});
								touch();
							}}
						>
							<For each={TRACKER_KINDS}>
								{(k) => <option value={k.value}>{k.label}</option>}
							</For>
						</select>
					</label>
				</div>
			</div>

			<div class="settings-section">
				<div class="settings-section-head">
					<span class="heading">Status mapping</span>
					<span class="sub">Compass state → tracker status.</span>
				</div>
				<ul class="settings-map">
					<For each={STATE_ROWS}>
						{(row) => (
							<li class="settings-map-row">
								<span class="settings-map-state" data-state={row.state}>
									{row.label}
								</span>
								<span class="settings-map-arrow" aria-hidden="true">
									→
								</span>
								<input
									type="text"
									class="settings-input settings-map-input"
									value={draft.mapping.toTracker[row.state]}
									placeholder="tracker status"
									onInput={(e) => {
										const value = e.currentTarget.value;
										setDraft((s) => {
											s.mapping.toTracker[row.state] = value;
										});
										touch();
									}}
								/>
							</li>
						)}
					</For>
				</ul>
			</div>

			<div class="settings-section">
				<div class="settings-section-head">
					<span class="heading">Read-back</span>
					<span class="sub">
						Tracker status → Compass state (derived, many-to-one).
					</span>
				</div>
				<ul class="settings-reverse">
					<For each={reverse()}>
						{(group) => (
							<li class="settings-reverse-row">
								<span class="settings-reverse-status">
									{group.statuses.join(", ")}
								</span>
								<span class="settings-map-arrow" aria-hidden="true">
									→
								</span>
								<span class="settings-reverse-states">{group.label}</span>
							</li>
						)}
					</For>
				</ul>
			</div>

			<div class="settings-actions">
				<button
					type="button"
					class="settings-btn settings-btn-save"
					disabled={!dirty() || hasEmptyMapping()}
					onClick={save}
				>
					Save
				</button>
				<button
					type="button"
					class="settings-btn settings-btn-reset"
					disabled={!dirty()}
					onClick={reset}
				>
					Reset
				</button>
			</div>
		</section>
	);
};
