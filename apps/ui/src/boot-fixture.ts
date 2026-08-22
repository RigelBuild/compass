// The offline fixture boot (T2). Reached ONLY via the dynamic `import()` in
// index.tsx's inline `import.meta.env.MODE === "fixture"` branch — so in every
// non-fixture build that branch dead-code-eliminates and this module's chunk is
// never emitted (the hard wall, §A1). It mirrors `index.tsx main()` minus the
// network: no comms client, no compass client, no WhoAmI round-trip. By the
// store's existing gates that yields board = STUB_ISSUES, fleet = STUB_AGENTS,
// banner = STUB_DAEMON, comms = the fixture — the exact clientless construction
// every component test builds.
//
// The PROD tripwire below is defense-in-depth behind the AUTHORITATIVE gate,
// the build-scan test `fixture-wall.test.ts`, which asserts FIXTURE_SENTINEL is
// structurally absent from a production `dist/`.

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

	// The clientless store: NO comms, NO compass. The store seeds STUB_ISSUES /
	// STUB_AGENTS / STUB_DAEMON and holds STUB_COMMS_STATE as the comms surface;
	// callerId defaults to CALLER_ID, the identity the fixtures are authored
	// around. createRoot gives the store's memos a stable owner (never disposed),
	// as index.tsx's main() does.
	// Fixture-ONLY empty-board affordance (T5): the visual harness sets `?empty`
	// to capture the empty-board fallback. This module is dead-code-eliminated
	// from prod by the `import.meta.env.MODE === "fixture"` wall (§A1), so the
	// param never reaches a shipped build. When present, seed an empty issue
	// list so the board renders its `.bridge-empty` message.
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
