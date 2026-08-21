import { describe, expect, test } from "bun:test";
import { render } from "@solidjs/testing-library";
import type { FileDiff, PlanEntry, TraceItem } from "../session-events";
import { SessionTrace } from "./SessionTrace";

// RED acceptance spec for T-U2 (design.md §440-478): the per-kind typed-trace
// renderer. `SessionTrace` takes the folded `TraceItem[]` (produced by
// `foldSession` upstream) and renders one row per kind, reusing the existing
// block CSS (app.css:996-1134). This suite mounts the component DIRECTLY with
// hand-built TraceItems — no store, no fold — so each test pins exactly one
// kind's render contract. `SessionTrace.tsx` does not exist yet, so the whole
// file is RED until the implementer builds it.
//
// The tool-status VALUE assertion (§473-476) is the load-bearing one: the model
// speaks `ToolCallStatus` (pending|in_progress|completed|failed) while the OLD
// CSS keyed running|ok|error. A presence-only check ("has a .tool-status") would
// pass against either vocabulary; asserting the emitted `data-status` VALUE is
// what makes red-green catch the gap.

// The old tool-status vocabulary the migration retires — the renderer must emit
// NONE of these.
const OLD_TOOL_STATUS = ["running", "ok", "error"];

// Mount SessionTrace over an explicit TraceItem list.
function mountTrace(items: TraceItem[]): HTMLElement {
	const { container } = render(() => <SessionTrace items={items} />);
	return container;
}

describe("SessionTrace (T-U2)", () => {
	// text item → `.block-text` carrying the item's text.
	test("text item renders .block-text with its text", () => {
		const container = mountTrace([
			{ kind: "text", messageId: "m1", text: "Wiring the workspace chat." },
		]);

		const block = container.querySelector(".block-text");
		expect(block).not.toBeNull();
		expect(block?.textContent).toContain("Wiring the workspace chat.");
	});

	// thinking item → `.block-thinking` (the italic-dim block) carrying its text.
	test("thinking item renders .block-thinking with its text", () => {
		const container = mountTrace([
			{ kind: "thinking", messageId: "t1", text: "Verify the chat renders." },
		]);

		const block = container.querySelector(".block-thinking");
		expect(block).not.toBeNull();
		expect(block?.textContent).toContain("Verify the chat renders.");
	});

	// tool item (with call) → `.block-tool` with `.tool-title` = the call title
	// AND a `.tool-status` whose data-status VALUE is the migrated proto vocab.
	// The value assertion (§473-476) is deliberate: assert it EQUALS "completed"
	// and is NOT any old-vocabulary value, so the vocabulary gap reddens.
	test("tool item with a call renders the title and a proto-vocab data-status", () => {
		const container = mountTrace([
			{
				kind: "tool",
				toolCallId: "tc-1",
				call: {
					id: "se-1",
					atUnixMs: 1_753_000_002_000,
					kind: "tool_call",
					toolCallId: "tc-1",
					title: "moon run compass-ui:test",
					status: "in_progress",
				},
				status: "completed",
			},
		]);

		const tool = container.querySelector(".block-tool");
		expect(tool).not.toBeNull();

		const title = tool?.querySelector(".tool-title");
		expect(title?.textContent).toContain("moon run compass-ui:test");

		const status = tool?.querySelector<HTMLElement>(".tool-status");
		expect(status).not.toBeNull();
		// The VALUE, not mere presence: proto vocab, latest-wins "completed".
		expect(status?.getAttribute("data-status")).toBe("completed");
		expect(OLD_TOOL_STATUS).not.toContain(status?.getAttribute("data-status"));
	});

	// tool item (orphan: call undefined) → `.tool-title` falls back to the
	// toolCallId (an orphan tool_call_update has no originating call title).
	test("orphan tool item falls back to the toolCallId for its title", () => {
		const container = mountTrace([
			{
				kind: "tool",
				toolCallId: "tc-orphan",
				call: undefined,
				status: "failed",
			},
		]);

		const title = container.querySelector(".block-tool .tool-title");
		expect(title).not.toBeNull();
		expect(title?.textContent).toContain("tc-orphan");
	});

	// tool item with diffs → one `.block-diff` per diff, each carrying
	// `.diff-line[data-kind="add"]` / `[data-kind="del"]` and the `.diff-path`.
	test("tool item with a diff renders add/del lines and the path", () => {
		const diff: FileDiff = {
			path: "apps/ui/src/App.tsx",
			oldText: "export function App() {\n\treturn <WorkspaceBoard />;\n}\n",
			newText: "export function App() {\n\treturn <ShellRouter />;\n}\n",
		};
		const container = mountTrace([
			{
				kind: "tool",
				toolCallId: "tc-1",
				call: {
					id: "se-1",
					atUnixMs: 1_753_000_002_000,
					kind: "tool_call",
					toolCallId: "tc-1",
					title: "apply edit",
					status: "completed",
				},
				status: "completed",
				diffs: [diff],
			},
		]);

		const diffBlock = container.querySelector(".block-diff");
		expect(diffBlock).not.toBeNull();

		const path = diffBlock?.querySelector(".diff-path");
		expect(path?.textContent).toContain("apps/ui/src/App.tsx");

		// Both an added and a removed line are rendered from the change.
		expect(
			diffBlock?.querySelector('.diff-line[data-kind="add"]'),
		).not.toBeNull();
		expect(
			diffBlock?.querySelector('.diff-line[data-kind="del"]'),
		).not.toBeNull();
	});

	// tool item with a NEW-FILE diff (oldText null) → the DiffBlock null-guard
	// (SessionTrace.tsx:9-10) yields an empty oldLines, so every rendered line is
	// an add and none is a del. This reddens if the guard regresses (`.split` on
	// null throwing, or del rows appearing for a new file).
	test("tool item with a new-file diff (null oldText) renders only add rows", () => {
		const newText = "const answer = 42;\nexport default answer;";
		const diff: FileDiff = {
			path: "apps/ui/src/answer.ts",
			oldText: null,
			newText,
		};
		const container = mountTrace([
			{
				kind: "tool",
				toolCallId: "tc-new",
				call: {
					id: "se-new",
					atUnixMs: 1_753_000_003_000,
					kind: "tool_call",
					toolCallId: "tc-new",
					title: "create file",
					status: "completed",
				},
				status: "completed",
				diffs: [diff],
			},
		]);

		const diffBlock = container.querySelector(".block-diff");
		expect(diffBlock).not.toBeNull();

		const path = diffBlock?.querySelector(".diff-path");
		expect(path?.textContent).toContain("apps/ui/src/answer.ts");

		// A new file is all adds: one add row per newText line, zero del rows.
		const addLines = diffBlock?.querySelectorAll('.diff-line[data-kind="add"]');
		const delLines = diffBlock?.querySelectorAll('.diff-line[data-kind="del"]');
		expect(delLines?.length).toBe(0);
		expect(addLines?.length).toBe(newText.split("\n").length);
	});

	// plan item → `.block-plan` with one `.plan-step` per entry, each carrying a
	// `data-status` in the PlanEntryStatus vocab (pending|in_progress|completed).
	test("plan item renders one .plan-step per entry with a PlanEntryStatus data-status", () => {
		const entries: PlanEntry[] = [
			{ content: "Half 1 — channel view", status: "completed" },
			{ content: "Half 2 — agent workspace", status: "in_progress" },
			{ content: "Verify + checkpoint with Matt", status: "pending" },
		];
		const container = mountTrace([{ kind: "plan", entries }]);

		const plan = container.querySelector(".block-plan");
		expect(plan).not.toBeNull();

		const steps = plan?.querySelectorAll<HTMLElement>(".plan-step");
		expect(steps?.length).toBe(3);

		const statuses = [...(steps ?? [])].map((s) =>
			s.getAttribute("data-status"),
		);
		expect(statuses).toEqual(["completed", "in_progress", "pending"]);

		// The step content is rendered (per-entry text, not merely a mark).
		const contents = [...(steps ?? [])].map(
			(s) => s.querySelector(".plan-content")?.textContent,
		);
		expect(contents.some((c) => c?.includes("Half 2 — agent workspace"))).toBe(
			true,
		);
	});

	// notice item → the notice text renders; a plain notice carries NO link-out.
	test("notice item renders its text and no anchor when it has no link", () => {
		const container = mountTrace([
			{
				kind: "notice",
				event: {
					id: "se-n1",
					atUnixMs: 1_753_000_000_000,
					kind: "notice",
					text: "Idle — awaiting franklin's grid.",
				},
			},
		]);

		expect(container.textContent).toContain("Idle — awaiting franklin's grid.");
		expect(container.querySelector("a")).toBeNull();
	});

	// notice item whose event carries a `link` → a link-out anchor with that href.
	test("notice item with a link renders a link-out anchor to that href", () => {
		const href = "https://github.com/RigelBuild/compass/pull/814";
		const container = mountTrace([
			{
				kind: "notice",
				event: {
					id: "se-n2",
					atUnixMs: 1_753_000_000_000,
					kind: "notice",
					text: "PR #814 opened",
					link: href,
				},
			},
		]);

		const anchor = container.querySelector<HTMLAnchorElement>("a[href]");
		expect(anchor).not.toBeNull();
		expect(anchor?.getAttribute("href")).toBe(href);
	});

	// A notice whose link carries a dangerous scheme (javascript:/data:) must
	// render NO navigable anchor. Once the live session-event stream feeds these
	// links (SEA-1342), the link is untrusted agent input; an unhardened
	// `href={link}` would make it clickable (stored-XSS-adjacent). The scheme
	// allow-list (safe-url.ts) gates the anchor. Pre-fix (`href={href()}` with no
	// gate) the anchor renders → red.
	for (const link of [
		"javascript:alert(document.domain)",
		"data:text/html,<script>alert(1)</script>",
		"jAvAsCrIpT:alert(1)",
	]) {
		test(`notice item with a ${link.split(":")[0]} link renders no navigable anchor`, () => {
			const container = mountTrace([
				{
					kind: "notice",
					event: {
						id: "se-n-danger",
						atUnixMs: 1_753_000_000_000,
						kind: "notice",
						text: "malicious notice",
						link,
					},
				},
			]);
			// text still renders; no anchor at all.
			expect(container.textContent).toContain("malicious notice");
			expect(container.querySelector("a")).toBeNull();
		});
	}

	// Companion: a safe https link is unaffected — the anchor renders with the
	// exact original href (safe-url returns the source string, not sanitize-url's
	// normalized form). Pins that the gate is scheme-based, not link-neutering.
	test("notice item with a safe https link keeps its exact href", () => {
		const link = "https://github.com/RigelBuild/compass/pull/814";
		const container = mountTrace([
			{
				kind: "notice",
				event: {
					id: "se-n-safe",
					atUnixMs: 1_753_000_000_000,
					kind: "notice",
					text: "PR opened",
					link,
				},
			},
		]);
		const anchor = container.querySelector<HTMLAnchorElement>("a[href]");
		expect(anchor).not.toBeNull();
		expect(anchor?.getAttribute("href")).toBe(link);
	});
});

// RED acceptance spec for T2 (design record §341-379): DiffBlock's line
// derivation moves from the current set-membership filter (SessionTrace.tsx:9-22)
// to the jsdiff-backed `diffRows`. These fixtures fail against the CURRENT
// membership code and go green once DiffBlock delegates to `diffRows`. Each
// mounts one tool item carrying a single diff, reusing the existing
// `mountTrace(items)` helper and tool-item TraceItem shape (see the kept-green
// tests at lines 106-183). The two existing DiffBlock tests are the regression
// gate and MUST stay green.
function mountDiff(diff: FileDiff): HTMLElement {
	return mountTrace([
		{
			kind: "tool",
			toolCallId: "tc-diff",
			call: {
				id: "se-diff",
				atUnixMs: 1_753_000_004_000,
				kind: "tool_call",
				toolCallId: "tc-diff",
				title: "apply edit",
				status: "completed",
			},
			status: "completed",
			diffs: [diff],
		},
	]);
}

describe("DiffBlock (T2, red against set-membership)", () => {
	// reorder: "a\nb" -> "b\na" is a real change, but set membership sees the same
	// two lines in both texts and renders ZERO rows. A real line diff renders at
	// least one .diff-line.
	test("reorder renders at least one diff line", () => {
		const container = mountDiff({
			path: "apps/ui/src/reorder.ts",
			oldText: "a\nb",
			newText: "b\na",
		});
		const lines = container.querySelectorAll(".diff-line");
		expect(lines.length).toBeGreaterThan(0);
	});

	// dup-line: "a" -> "a\na" adds one duplicate "a". Set membership sees "a" in
	// both and renders zero rows; a real diff renders exactly one add, zero dels.
	test("dup-line renders exactly one add and no del", () => {
		const container = mountDiff({
			path: "apps/ui/src/dup.ts",
			oldText: "a",
			newText: "a\na",
		});
		const adds = container.querySelectorAll('.diff-line[data-kind="add"]');
		const dels = container.querySelectorAll('.diff-line[data-kind="del"]');
		expect(adds.length).toBe(1);
		expect(dels.length).toBe(0);
	});

	// trailing-newline: "a\n" -> "a\nb" adds "b". The membership filter splits on
	// "\n" WITHOUT trimming, so oldText "a\n" -> ["a",""] and the vanished "" is
	// emitted as a phantom `del ""`. A real diff renders exactly one add ("b")
	// and NO blank-bodied row.
	test("trailing-newline renders one add and no blank-bodied row", () => {
		const container = mountDiff({
			path: "apps/ui/src/trailing.ts",
			oldText: "a\n",
			newText: "a\nb",
		});
		const adds = container.querySelectorAll('.diff-line[data-kind="add"]');
		expect(adds.length).toBe(1);
		expect(adds[0]?.querySelector(".diff-body")?.textContent).toBe("b");
		// No rendered diff line has an empty body (the phantom `del ""`).
		const lines = Array.from(container.querySelectorAll(".diff-line"));
		const blankBodied = lines.filter(
			(l) => l.querySelector(".diff-body")?.textContent === "",
		);
		expect(blankBodied.length).toBe(0);
	});

	// empty-text: null old, "" new is a no-op change. Membership splits "" ->
	// [""] and emits one phantom blank add row; a real diff renders zero lines.
	test("empty text renders zero diff lines", () => {
		const container = mountDiff({
			path: "apps/ui/src/empty.ts",
			oldText: null,
			newText: "",
		});
		const lines = container.querySelectorAll(".diff-line");
		expect(lines.length).toBe(0);
	});
});
