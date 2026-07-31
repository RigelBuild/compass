// The tracker seam (design D2/T11/T12) — Compass state is canonical; a tracker's
// native status is a projection over it, through a user-editable mapping.
//
// Compass state is server-authoritative (DL-070): the canonical `Issue.state`
// is computed and streamed by the server projection, and the tracker is a
// projection OF it (DL-032), mirrored server-side on real working-state
// transitions. This module is the UI-side client seam over that model; the
// fixture implements it in-memory until the daemon's board projection and
// write-path RPC land, when `listAssignedIssues`/`updateIssueStatus` become
// `@compass/client` calls — a one-module swap. The projection domain is the
// seven WORKING states; `archived` carries no tracker status (DL-071).

import type {
	Issue,
	TrackerConfig,
	TrackerKind,
	TrackerStatusMapping,
	WorkingIssueState,
} from "./stub-data";
import { STUB_ASSIGNED_ISSUES } from "./stub-data";

/**
 * The thin async contract the store calls to read/write the linked tracker. The
 * fixture implements it in-memory now (`createFixtureTrackerSeam`); the real
 * `@compass/client` implements it against the daemon later — a one-module swap.
 */
export interface TrackerSeam {
	/** The user's tracker-assigned issues, for the Backlog view (D3). */
	listAssignedIssues(handle: string): Promise<Issue[]>;
	/** Mirror a Compass state change onto the tracker (D2), mapping through
	 *  `TrackerStatusMapping.toTracker` before the write. */
	updateIssueStatus(id: string, compassState: WorkingIssueState): Promise<void>;
}

/**
 * The default Linear projection (D2). `toTracker` is total over Compass states;
 * `fromTracker` is many-to-one — Linear lacks a `Queued`, so both `todo` and
 * `queued` project to `Todo`, and `Cancelled`/`Duplicate` both read back as
 * `done` (the design's named many-to-one case).
 */
export const LINEAR_STATUS_MAPPING: TrackerStatusMapping = {
	kind: "linear",
	toTracker: {
		backlog: "Backlog",
		todo: "Todo",
		queued: "Todo",
		blocked: "In Progress",
		in_progress: "In Progress",
		in_review: "In Review",
		done: "Done",
	},
	fromTracker: {
		Backlog: "backlog",
		Todo: "todo",
		"In Progress": "in_progress",
		"In Review": "in_review",
		Done: "done",
		Cancelled: "done",
		Duplicate: "done",
	},
};

/** The default tracker wiring — Linear, a placeholder handle, the Linear map. */
export const DEFAULT_TRACKER_CONFIG: TrackerConfig = {
	kind: "linear",
	handle: "matt@sealed",
	mapping: LINEAR_STATUS_MAPPING,
};

/** Project a Compass working state onto the tracker's native status name (D2).
 *  Total over the seven working states, so every state has a target. */
export function toTrackerStatus(
	state: WorkingIssueState,
	mapping: TrackerStatusMapping,
): string {
	return mapping.toTracker[state];
}

/** Read a tracker's native status back to a Compass state (D2). Many-to-one;
 *  an unmapped status falls back to `backlog` (a safe pre-active default rather
 *  than dropping the issue). */
export function fromTrackerStatus(
	status: string,
	mapping: TrackerStatusMapping,
): WorkingIssueState {
	return mapping.fromTracker[status] ?? "backlog";
}

/**
 * The in-memory fixture seam. `listAssignedIssues` returns the user's fixture
 * issues; `updateIssueStatus` maps the state through the config and resolves
 * (the fixture *is* the tracker, so there's nothing to POST). The store swaps
 * this for a `@compass/client`-backed seam when the daemon grows the contract.
 */
export function createFixtureTrackerSeam(
	config: TrackerConfig = DEFAULT_TRACKER_CONFIG,
): TrackerSeam {
	return {
		listAssignedIssues(handle: string): Promise<Issue[]> {
			// The fixture ignores the handle beyond the contract; the real seam
			// queries the tracker for `handle`'s assigned issues. An empty handle
			// (tracker not configured) yields nothing.
			return Promise.resolve(handle ? STUB_ASSIGNED_ISSUES : []);
		},
		updateIssueStatus(
			_id: string,
			compassState: WorkingIssueState,
		): Promise<void> {
			// Map before the (no-op) write so the mapping is exercised, matching
			// the real write path's shape.
			void toTrackerStatus(compassState, config.mapping);
			return Promise.resolve();
		},
	};
}

export type { TrackerKind };
