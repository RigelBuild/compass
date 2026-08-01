import { describe, expect, test } from "bun:test";
import { mergeFromTracker } from "./components/SettingsView";
import type { WorkingIssueState } from "./stub-data";
import { LINEAR_STATUS_MAPPING } from "./tracker";

// `mergeFromTracker` (SettingsView) is the pure reverse-map merge Save runs
// before persisting a tracker config. The reverse map (`fromTracker`) is the
// canonical read-back and is NOT reconstructible from the forward map alone:
// it carries tracker-native aliases no forward row produces, and where several
// Compass states share a status the read-back is a first-in-lifecycle pick, not
// last-write-wins. The pre-fix code (`deriveFromTracker(toTracker)`) rebuilt the
// reverse map from scratch with last-write-wins and no committed map — dropping
// aliases and inverting shared statuses. These tests pin the fixed contract:
// committed entries are preserved, only genuinely-new statuses are added, and
// first-writer (earliest lifecycle state) wins. They run outside any reactive
// root — the function is pure.

describe("mergeFromTracker (reverse-map preservation)", () => {
	// Alias preservation. Linear's `Cancelled` and `Duplicate` read back to
	// `done` but are NOT values of the forward map, so a from-scratch rebuild
	// (the pre-fix `deriveFromTracker`) drops them entirely. Merging the Linear
	// forward map into its own committed reverse map MUST keep both aliases.
	// Mutant caught: rebuilding fromTracker from toTracker (aliases vanish →
	// these reads return undefined).
	test("preserves Cancelled/Duplicate aliases mapping to done", () => {
		const merged = mergeFromTracker(
			LINEAR_STATUS_MAPPING.toTracker,
			LINEAR_STATUS_MAPPING.fromTracker,
		);

		expect(merged.Cancelled).toBe("done");
		expect(merged.Duplicate).toBe("done");
		// Every committed alias survives — none silently dropped.
		expect(merged.Done).toBe("done");
	});

	// First-writer-wins via committed preservation. `Todo` is targeted by both
	// `todo` and (later in lifecycle) `queued`. The committed reverse map already
	// pins `Todo → todo`; the merge must NOT overwrite it. Mutant caught:
	// last-write-wins (pre-fix) sends `Todo → queued`; any code that overwrites a
	// committed key while adding forward targets reddens here.
	test("keeps a committed shared status (Todo stays todo, not queued)", () => {
		const merged = mergeFromTracker(
			LINEAR_STATUS_MAPPING.toTracker,
			LINEAR_STATUS_MAPPING.fromTracker,
		);

		expect(merged.Todo).toBe("todo");
		expect(merged.Todo).not.toBe("queued");
	});

	// Adding genuinely-new statuses. With an EMPTY committed map, every forward
	// target must be added, and where a status is shared the FIRST Compass state
	// in lifecycle order wins. STATE_ROWS order: backlog, todo, queued, blocked,
	// in_progress, in_review, done. So `Todo` (todo before queued) → todo, and
	// `In Progress` (blocked before in_progress) → blocked.
	// Mutants caught: (1) "never add, only copy committed" leaves these
	// undefined; (2) last-write-wins gives `Todo → queued` and
	// `In Progress → in_progress`.
	test("adds new statuses at the first lifecycle-order state targeting them", () => {
		const merged = mergeFromTracker(
			LINEAR_STATUS_MAPPING.toTracker,
			{} as Record<string, WorkingIssueState>,
		);

		// Single-target statuses are added to their sole state.
		expect(merged.Backlog).toBe("backlog");
		expect(merged["In Review"]).toBe("in_review");
		expect(merged.Done).toBe("done");
		// Shared statuses resolve to the earliest lifecycle state.
		expect(merged.Todo).toBe("todo");
		expect(merged["In Progress"]).toBe("blocked");
	});
});
