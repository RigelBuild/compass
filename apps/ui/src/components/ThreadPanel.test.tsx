import { describe, expect, test } from "bun:test";
import { fireEvent, render } from "@solidjs/testing-library";
import { blockText, type Thread, threadsOf } from "../comms";
import {
	type Account,
	type Channel,
	type Message,
	STUB_CHANNELS,
	STUB_MESSAGES,
} from "../comms-stub";
import { StoreContext } from "../context";
import { type AppStore, createAppStore } from "../store";
import { ChannelView, ThreadView } from "./ChannelView";
import { ThreadPanel } from "./ThreadPanel";

// RED acceptance spec for T-T2 (design.md §344-374, OQ-1 ruling §721-722): the
// ThreadPanel — a split-beside-the-stream aside that ChannelView hosts, opened
// by a per-thread reply affordance on the thread ROOT row and driven entirely
// through the already-green T-T1 store API (openThreadRootId / openThread /
// closeThread / postReply). These tests mount the STANDALONE ChannelView (no
// channel prop → store.selectedChannel()) over a real store and assert the
// panel's observable contract. The component + affordance do NOT exist yet, so
// the affordance/panel legs go RED now (the expected red); test 1 is the
// starts-null guard that must stay green through implementation.
//
// Contract selectors (test and impl agree — record §346-370):
//   - `.thread-reply` button on the thread root row → store.openThread(rootId)
//   - `.thread-panel` aside, present only when openThreadRootId() resolves to a
//     thread in the current channel; carries `.thread-close` → closeThread(),
//     the root + replies under `.thread-replies`, and a thread-scoped composer
//     reusing `.conv-composer` (`input.field` + `.send`).
//   - the main stream is `.conv-stream` (sibling of the panel, not inside it) —
//     both read the same store.messages(), so a posted reply shows in both.

// Exact rendered text of a message: concatenation of its text blocks (mention
// chips still contribute their `@handle` run to textContent, so a full-text
// includes() match holds). Non-text (ask) blocks contribute "".
const textOf = (m: Message): string => m.blocks.map(blockText).join("").trim();

// Derived from the fixture (never hardcoded, so a reshuffle can't stale the
// test): the first STANDALONE channel (kind "channel") the caller is a member
// of (membership !== "none", so its composer is enabled per OQ-1) whose
// threadsOf yields a thread with ≥1 reply. That thread's root + first reply are
// the coordinates the panel must render and the affordance must open.
// Ground truth today: ch-svc-compass ("svc.compass", subscribed) → msg-c1 root
// + msg-c2/msg-c3 replies (comms-stub.ts:319-355).
const THREAD = (() => {
	for (const c of STUB_CHANNELS) {
		if (c.kind !== "channel" || c.membership === "none") continue;
		for (const t of threadsOf(STUB_MESSAGES, c.id)) {
			if (t.replies.length >= 1) {
				return {
					channelId: c.id,
					rootId: t.root.id,
					rootText: textOf(t.root),
					replyText: textOf(t.replies[0]),
				};
			}
		}
	}
	throw new Error(
		"fixture has no joined standalone channel with a threaded reply — T-T2 panel tests need one",
	);
})();

// Mount the standalone ChannelView (no channel prop → store.selectedChannel())
// over a real store through the app's StoreContext, mirroring
// ChannelView.test.tsx:99-109. The store is built inside render's reactive root
// so its memos are owned + disposed on the library's per-test cleanup; the
// reference is captured so tests drive openChannel/openThread and re-read state.
function mountChannelView(): { store: AppStore; container: HTMLElement } {
	let store!: AppStore;
	const { container } = render(() => {
		store = createAppStore();
		return (
			<StoreContext.Provider value={store}>
				<ChannelView />
			</StoreContext.Provider>
		);
	});
	return { store, container };
}

// The `.thread` element rendering the derived root — scoped so we click the
// reply affordance for THIS root, not some other thread's.
const threadElFor = (
	container: HTMLElement,
	rootText: string,
): Element | null =>
	[...container.querySelectorAll(".thread")].find((el) =>
		el.textContent?.includes(rootText),
	) ?? null;

describe("ThreadPanel (T-T2)", () => {
	// The open-thread state starts null (T-T1), so a freshly-mounted channel view
	// shows NO panel. Guard leg: stays green through implementation — a panel that
	// renders unconditionally (ignoring openThreadRootId) reddens this.
	test("no thread panel by default", () => {
		const { store, container } = mountChannelView();
		store.openChannel(THREAD.channelId);

		expect(store.openThreadRootId()).toBeNull();
		expect(container.querySelector(".thread-panel")).toBeNull();
	});

	// The summary affordance opens the panel: a thread root that HAS replies
	// carries a `.thread-summary` button (not `.thread-reply` — that is now the
	// zero-reply-only affordance) whose click calls store.openThread(rootId),
	// which both sets openThreadRootId AND makes the `.thread-panel` appear. RED
	// now: the summary affordance doesn't render, so the button query is null.
	test("summary affordance opens the panel", () => {
		const { store, container } = mountChannelView();
		store.openChannel(THREAD.channelId);

		const threadEl = threadElFor(container, THREAD.rootText);
		expect(threadEl).not.toBeNull();
		const summaryBtn =
			threadEl?.querySelector<HTMLButtonElement>(".thread-summary") ?? null;
		expect(summaryBtn).not.toBeNull();

		fireEvent.click(summaryBtn as HTMLButtonElement);

		expect(store.openThreadRootId()).toBe(THREAD.rootId);
		expect(container.querySelector(".thread-panel")).not.toBeNull();
	});

	// The open panel renders the root message and its replies. Assert the actual
	// fixture text of the root AND its first reply appear INSIDE the panel element
	// (scoped to `.thread-panel`, not merely somewhere on screen). RED now: no
	// panel renders.
	test("panel shows root and replies", () => {
		const { store, container } = mountChannelView();
		store.openChannel(THREAD.channelId);
		store.openThread(THREAD.rootId);

		const panel = container.querySelector(".thread-panel");
		expect(panel).not.toBeNull();
		expect(panel?.textContent).toContain(THREAD.rootText);
		expect(panel?.textContent).toContain(THREAD.replyText);
	});

	// The thread-scoped composer posts through store.postReply. Type a distinctive
	// line into `.thread-panel .conv-composer input.field`, click its `.send`, and
	// assert the text (a) appears inside the panel as a new reply, and (b) does
	// NOT leak into the main stream — under the Slack model no reply body renders
	// under `.conv-stream`; instead the root's `.thread-summary-count` reflects
	// the incremented count (2 → 3 replies).
	test("panel composer posts a reply appearing in-panel only, not in the stream", () => {
		const { store, container } = mountChannelView();
		store.openChannel(THREAD.channelId);
		store.openThread(THREAD.rootId);

		const input = container.querySelector<HTMLInputElement>(
			".thread-panel .conv-composer input.field",
		);
		const send = container.querySelector<HTMLButtonElement>(
			".thread-panel .conv-composer .send",
		);
		expect(input).not.toBeNull();
		expect(send).not.toBeNull();

		fireEvent.input(input as HTMLInputElement, {
			target: { value: "reply-from-test" },
		});
		fireEvent.click(send as HTMLButtonElement);

		// (a) inside the panel, as a new reply
		expect(container.querySelector(".thread-panel")?.textContent).toContain(
			"reply-from-test",
		);
		// (b) NOT in the main stream: no reply-body rows and the text is absent
		expect(
			container.querySelectorAll(".conv-stream .thread-replies").length,
		).toBe(0);
		expect(container.querySelector(".conv-stream")?.textContent).not.toContain(
			"reply-from-test",
		);
		// …and the root's summary count reflects the incremented reply count.
		const threadEl = threadElFor(container, THREAD.rootText);
		expect(
			threadEl?.querySelector(".thread-summary-count")?.textContent,
		).toContain("3 replies");
	});

	// The close control tears the panel down: click `.thread-close` →
	// store.closeThread() resets openThreadRootId to null and the panel leaves the
	// DOM. RED now: no panel/close control.
	test("close hides the panel", () => {
		const { store, container } = mountChannelView();
		store.openChannel(THREAD.channelId);
		store.openThread(THREAD.rootId);

		const close = container.querySelector<HTMLButtonElement>(".thread-close");
		expect(close).not.toBeNull();

		fireEvent.click(close as HTMLButtonElement);

		expect(store.openThreadRootId()).toBeNull();
		expect(container.querySelector(".thread-panel")).toBeNull();
	});

	// OQ-1 §711-719: a thread reply is text posting, never read-only in a joined
	// channel. In a subscribed standalone channel the panel composer mirrors the
	// main composer's enablement — the input is disabled only on
	// membership==="none", so here it is NOT disabled. RED now: no panel composer.
	test("thread composer is enabled in a joined channel", () => {
		const { store, container } = mountChannelView();
		store.openChannel(THREAD.channelId);
		store.openThread(THREAD.rootId);

		const input = container.querySelector<HTMLInputElement>(
			".thread-panel .conv-composer input.field",
		);
		expect(input).not.toBeNull();
		expect((input as HTMLInputElement).disabled).toBe(false);
	});

	// OQ-1 §711-719, the DISABLED side of the same contract: in a channel the
	// caller has NOT joined (membership==="none") the thread composer mirrors the
	// main composer being read-only — the input is disabled AND the send button
	// is disabled. The only none-membership channel in the fixture (ch-random)
	// carries no messages, so no root is constructible there; per the assignment
	// fallback we mount the exported ThreadPanel directly over the real thread's
	// channel spread to membership:"none" (the panel resolves the thread from
	// store.messages() by channel id, unaffected by membership; only the
	// composer's enablement reads it). Mirrors the enabled test's assertion style.
	test("thread composer is disabled in a non-member channel", () => {
		let store!: AppStore;
		const { container } = render(() => {
			store = createAppStore();
			const real = store
				.channels()
				.find((c) => c.id === THREAD.channelId) as Channel;
			const noneChannel: Channel = { ...real, membership: "none" };
			const byId = new Map<string, Account>(
				store.accounts().map((a) => [a.id, a]),
			);
			const byHandle = new Map<string, Account>(
				store.accounts().map((a) => [a.handle.toLowerCase(), a]),
			);
			store.openThread(THREAD.rootId);
			return (
				<StoreContext.Provider value={store}>
					<ThreadPanel channel={noneChannel} byId={byId} byHandle={byHandle} />
				</StoreContext.Provider>
			);
		});

		// the panel renders (membership does not gate the thread resolution)…
		expect(container.querySelector(".thread-panel")).not.toBeNull();
		// …but its composer is read-only: both the input and the send button
		// carry disabled.
		const input = container.querySelector<HTMLInputElement>(
			".thread-panel .conv-composer input.field",
		);
		const send = container.querySelector<HTMLButtonElement>(
			".thread-panel .conv-composer .send",
		);
		expect(input).not.toBeNull();
		expect(send).not.toBeNull();
		expect((input as HTMLInputElement).disabled).toBe(true);
		expect((send as HTMLButtonElement).disabled).toBe(true);
	});

	// End-to-end DOM invariant: switching the standalone channel surface to a
	// DIFFERENT channel tears the panel out of the DOM. This is the combined path
	// — openChannel clears openThreadRootId AND the panel's threadsOf rescope to
	// the new channel no longer resolves the old root — proven at the rendered
	// level, not just at the two store/close halves. A regressed openChannel that
	// left the id set, or a panel that ignored the channel rescope, keeps the
	// aside in the DOM and reddens this.
	test("switching channel removes the panel from the DOM", () => {
		const { store, container } = mountChannelView();
		store.openChannel(THREAD.channelId);
		store.openThread(THREAD.rootId);
		// the panel is present while its thread's channel is the open one.
		expect(container.querySelector(".thread-panel")).not.toBeNull();

		// a second standalone channel (kind "channel"), never a 1:1 DM (which
		// would route to the agent workspace instead of the channel surface).
		const other = STUB_CHANNELS.find(
			(c) => c.kind === "channel" && c.id !== THREAD.channelId,
		);
		if (!other) throw new Error("fixture has no second standalone channel");
		store.openChannel(other.id);

		expect(container.querySelector(".thread-panel")).toBeNull();
	});
});

// The Slack-model summary affordance on the thread ROOT row in the main stream.
// These mount the standalone ChannelView and assert the summary markup plus the
// absence of inline reply bodies from the stream: a threaded root shows a
// compact `.thread-summary`, and reply bodies never render under
// `.conv-stream`.
describe("ThreadView (Slack summary)", () => {
	// The first joined standalone channel's first zero-reply thread — derived,
	// never hardcoded, so a fixture reshuffle can't stale the zero-reply legs.
	// Ground truth today: ch-announcements / msg-a1, a zero-reply text root. The
	// affordance render is block-type-independent, so a text root exercises the
	// zero-reply "reply" affordance contract exactly as an ask root would.
	const ZERO = (() => {
		for (const c of STUB_CHANNELS) {
			if (c.kind !== "channel" || c.membership === "none") continue;
			for (const t of threadsOf(STUB_MESSAGES, c.id)) {
				if (t.replies.length === 0) {
					return {
						channelId: c.id,
						rootId: t.root.id,
						rootText: textOf(t.root),
					};
				}
			}
		}
		throw new Error("fixture has no joined standalone zero-reply thread");
	})();

	// 1. A root with N>0 replies renders `.thread-summary` (with an "N replies"
	// count) and NO reply body leaks into the stream: no `.thread-replies` rows
	// under `.conv-stream`, and the reply text is absent from the stream while
	// present in the open panel. The whole point of the Slack model.
	test("summary replaces inline replies: count shows, reply body leaves the stream", () => {
		const { store, container } = mountChannelView();
		store.openChannel(THREAD.channelId);

		const threadEl = threadElFor(container, THREAD.rootText);
		expect(threadEl).not.toBeNull();
		const summary =
			threadEl?.querySelector<HTMLButtonElement>(".thread-summary") ?? null;
		expect(summary).not.toBeNull();
		expect(
			summary?.querySelector(".thread-summary-count")?.textContent,
		).toContain("2 replies");

		// no reply-body rows in the stream, and the reply text is gone from it…
		expect(
			container.querySelectorAll(".conv-stream .thread-replies").length,
		).toBe(0);
		expect(container.querySelector(".conv-stream")?.textContent).not.toContain(
			THREAD.replyText,
		);
		// …but the reply is still reachable in the opened panel.
		store.openThread(THREAD.rootId);
		expect(container.querySelector(".thread-panel")?.textContent).toContain(
			THREAD.replyText,
		);
	});

	// 2. Clicking `.thread-summary` opens the panel (openThreadRootId set + the
	// aside mounts). (Distinct from the migrated leg above: this scopes the click
	// to the summary node itself, proving the click handler is on the summary.)
	test("clicking the summary opens the thread panel", () => {
		const { store, container } = mountChannelView();
		store.openChannel(THREAD.channelId);

		const threadEl = threadElFor(container, THREAD.rootText);
		const summary =
			threadEl?.querySelector<HTMLButtonElement>(".thread-summary") ?? null;
		expect(summary).not.toBeNull();

		fireEvent.click(summary as HTMLButtonElement);

		expect(store.openThreadRootId()).toBe(THREAD.rootId);
		expect(container.querySelector(".thread-panel")).not.toBeNull();
	});

	// 3. A zero-reply root keeps the `.thread-reply` ("reply") affordance so a
	// thread stays startable, and clicking it opens the panel on that root.
	test("a zero-reply root still shows the reply affordance and it opens the panel", () => {
		const { store, container } = mountChannelView();
		store.openChannel(ZERO.channelId);

		const threadEl = threadElFor(container, ZERO.rootText);
		expect(threadEl).not.toBeNull();
		const replyBtn =
			threadEl?.querySelector<HTMLButtonElement>(".thread-reply") ?? null;
		expect(replyBtn).not.toBeNull();

		fireEvent.click(replyBtn as HTMLButtonElement);

		expect(store.openThreadRootId()).toBe(ZERO.rootId);
		expect(container.querySelector(".thread-panel")).not.toBeNull();
	});

	// 4. Exactly one affordance per root: a root WITH replies renders the summary
	// and NOT the `.thread-reply` button.
	test("a root with replies does not render the reply affordance", () => {
		const { store, container } = mountChannelView();
		store.openChannel(THREAD.channelId);

		const threadEl = threadElFor(container, THREAD.rootText);
		expect(threadEl).not.toBeNull();
		expect(threadEl?.querySelector(".thread-summary")).not.toBeNull();
		expect(threadEl?.querySelector(".thread-reply")).toBeNull();
	});

	// 5. The people pile: one `.thread-summary-avatar` badge per DISTINCT reply
	// author in first-reply order — badge text is the handle's first char
	// uppercased, full `@handle` on the title attr. Fixture msg-c1: livingstone
	// replied first (min 24) then cook (min 27) → ["L"/"@livingstone",
	// "C"/"@cook"]. And `.thread-summary-time` reads `last ` + the hhmm of the
	// latest reply (min 27 → 17:27 UTC).
	test("summary people pile: one initialled badge per distinct author, in first-reply order, plus the last-reply time", () => {
		const { store, container } = mountChannelView();
		store.openChannel(THREAD.channelId);

		const threadEl = threadElFor(container, THREAD.rootText);
		const summary =
			threadEl?.querySelector<HTMLElement>(".thread-summary") ?? null;
		expect(summary).not.toBeNull();

		const badges = [
			...(summary?.querySelectorAll<HTMLElement>(".thread-summary-avatar") ??
				[]),
		];
		expect(badges.map((b) => b.textContent)).toEqual(["L", "C"]);
		expect(badges.map((b) => b.getAttribute("title"))).toEqual([
			"@livingstone",
			"@cook",
		]);

		expect(
			summary?.querySelector(".thread-summary-time")?.textContent,
		).toContain("last 17:27");
	});

	// 6 (overflow cap). The people pile caps at 5 badges + one
	// `.thread-summary-overflow` "+N" node when a thread has >5 DISTINCT reply
	// authors. Mount `ThreadView` directly with a hand-built ≥6-distinct-author
	// Thread: the store's postReply authors every reply as the caller, so a
	// ≥6-author thread can't be built through the store — the model-layer
	// numeric contract is pinned in comms.test.ts; this is its view render.
	test("summary people pile caps at 5 badges with a +N overflow node", () => {
		// 6 distinct authors, distinct first initials A–F so badge order + the
		// dropped 6th are verifiable; displayName is the handle capitalized.
		const accts: Account[] = [
			{ id: "acc-a1", handle: "alice", displayName: "Alice", kind: "user" },
			{ id: "acc-a2", handle: "bob", displayName: "Bob", kind: "user" },
			{ id: "acc-a3", handle: "carol", displayName: "Carol", kind: "user" },
			{ id: "acc-a4", handle: "dave", displayName: "Dave", kind: "user" },
			{ id: "acc-a5", handle: "erin", displayName: "Erin", kind: "user" },
			{ id: "acc-a6", handle: "frank", displayName: "Frank", kind: "user" },
		];
		const byId = new Map(accts.map((a) => [a.id, a]));
		const byHandle = new Map(accts.map((a) => [a.handle.toLowerCase(), a]));

		// root (acc-a1) + 6 replies by acc-a1..acc-a6, atUnixMs strictly
		// increasing so first-reply order is deterministic.
		const mk = (i: number, authorAccountId: string): Message => ({
			id: `msg-t${i}`,
			channelId: "ch-test",
			authorAccountId,
			atUnixMs: Date.UTC(2026, 6, 24, 12, 0, i),
			parentMessageId: "msg-t0",
			blocks: [{ kind: "text", text: `reply ${i}` }],
		});
		const thread: Thread = {
			root: {
				id: "msg-t0",
				channelId: "ch-test",
				authorAccountId: "acc-a1",
				atUnixMs: Date.UTC(2026, 6, 24, 11, 59, 0),
				blocks: [{ kind: "text", text: "root" }],
			},
			replies: accts.map((a, i) => mk(i + 1, a.id)),
		};

		// ThreadView calls useStore() (for openThread); the provider only needs to
		// exist, so build a real store inside render's reactive root exactly as
		// mountChannelView does.
		const { container } = render(() => {
			const store = createAppStore();
			return (
				<StoreContext.Provider value={store}>
					<ThreadView thread={thread} byId={byId} byHandle={byHandle} />
				</StoreContext.Provider>
			);
		});

		const badges = [
			...container.querySelectorAll<HTMLElement>(".thread-summary-avatar"),
		];
		expect(badges.length).toBe(5);
		expect(badges.map((b) => b.textContent)).toEqual(["A", "B", "C", "D", "E"]);
		expect(badges.map((b) => b.textContent)).not.toContain("F");

		const overflow = container.querySelectorAll(".thread-summary-overflow");
		expect(overflow.length).toBe(1);
		expect(overflow[0]?.textContent).toContain("+1");
	});
});
