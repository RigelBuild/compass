// The offline fixture boot (T2). Reached ONLY via the dynamic `import()` in index.tsx's
// `import.meta.env.MODE === "fixture"` branch, so in every non-fixture build that branch
// dead-code-eliminates and this chunk is never emitted (the hard wall, §A1). The PROD
// tripwire below is defense-in-depth behind the build-scan gate `fixture-wall.test.ts`.

import { createRoot } from "solid-js";
import { STUB_COMMS_STATE } from "./comms-stub";
import { mountShell, newAppQueryClient } from "./mount";
import { createAppStore } from "./store";

/** The unique build-scan sentinel — the literal `fixture-wall.test.ts` asserts
 *  is ABSENT from a production bundle. Referenced by the PROD tripwire so the
 *  literal is guaranteed present in this module. */
export const FIXTURE_SENTINEL = "COMPASS-FIXTURE-BOOT-SENTINEL-7f3a";

/** Boot the UI fully offline, seeded from the existing fixtures. Builds the
 *  clientless (offline) store and mounts the same shell the live boot mounts.
 *  Returns the shell disposer (from `mountShell`) so a test can tear the mount
 *  down; production's dynamic-import boot ignores it (the app owns the page). */
export function bootFixture(root: HTMLElement): () => void {
	// Runtime tripwire (§A1): a build that gained `--mode fixture` while defaulting
	// NODE_ENV to production trips here. It is insurance — the build-scan gate is
	// the wall — but references FIXTURE_SENTINEL so the sentinel literal is present.
	if (import.meta.env.PROD) {
		throw new Error(
			`${FIXTURE_SENTINEL} must never boot in a production build`,
		);
	}

	const queryClient = newAppQueryClient();

	// The clientless store: NO comms, NO compass. It seeds STUB_ISSUES / STUB_AGENTS /
	// STUB_DAEMON and holds STUB_COMMS_STATE; createRoot gives its memos a stable owner.
	// Fixture-ONLY empty-board affordance (T5): `?empty` seeds an empty issue list so the
	// board renders its `.bridge-empty` message (dead-code-eliminated from prod by the §A1 wall).
	const emptyBoard = new URLSearchParams(location.search).has("empty");
	const store = createRoot(() =>
		createAppStore({
			queryClient,
			initialComms: STUB_COMMS_STATE,
			workspaceKey: "fixture",
			...(emptyBoard ? { initialIssues: [] } : {}),
		}),
	);

	return mountShell(root, store, queryClient);
}
