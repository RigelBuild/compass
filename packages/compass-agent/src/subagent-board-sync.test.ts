import { describe, expect, test } from "bun:test";
import { TASK_SUBAGENT_LIFECYCLE_CHANNEL } from "@oh-my-pi/pi-coding-agent/task";
import { EventBus } from "@oh-my-pi/pi-coding-agent/utils/event-bus";
import { BoardFreshness, taskKeysOf } from "./board-freshness";
import {
	board,
	FakeTimer,
	FakeTransport,
	sequence,
	settle,
} from "./board-test-fakes";
import { CommsBroker } from "./comms";
import { attachSubagentBoardSync } from "./subagent-board-sync";

function attached(focus: string, opts: { seeded?: boolean } = {}) {
	const timer = new FakeTimer();
	const transport = new FakeTransport(board({ focusTaskKey: focus }));
	const bus = new EventBus();
	const freshness = new BoardFreshness();
	if (opts.seeded !== false)
		freshness.seed(1n, 1_000, focus, taskKeysOf(transport.board));
	const detach = attachSubagentBoardSync(
		bus,
		new CommsBroker(transport),
		sequence(),
		freshness,
		timer,
	);
	const emit = (id: string, status: string, description?: string) =>
		bus.emit(TASK_SUBAGENT_LIFECYCLE_CHANNEL, {
			id,
			agent: "reviewer",
			status,
			...(description === undefined ? {} : { description }),
		});
	const patches = () =>
		transport.requests.flatMap((r) =>
			r.call.case === "updateBoard" ? [r.call.value] : [],
		);
	const writes = () => patches().filter((p) => p.upsertTasks.length > 0);
	const window = async () => {
		timer.advance(5_000);
		await settle();
	};
	return { timer, transport, freshness, emit, patches, writes, window, detach };
}

describe("subagent board synchronization", () => {
	test("coalesces a grandchild start and completion into one automatic patch", async () => {
		const h = attached("parent-task");
		h.emit("grandchild-1", "started", "Check the branch");
		h.timer.advance(1_000);
		h.emit("grandchild-1", "completed");
		expect(h.patches()).toHaveLength(0);
		h.timer.advance(4_000);
		await settle();

		const [patch] = h.patches();
		expect(h.patches()).toHaveLength(1);
		expect(patch?.automatic).toBe(true);
		expect(patch?.upsertTasks).toHaveLength(1);
		expect(patch?.upsertTasks[0]?.key).toBe("parent-task");
		expect(patch?.upsertTasks[0]?.title).toBeUndefined();
		expect(patch?.upsertTasks[0]?.upsertSubagents).toEqual([
			expect.objectContaining({
				id: "grandchild-1",
				description: "Check the branch",
				status: "completed",
				startedAtUnixMs: 10_000n,
				endedAtUnixMs: 11_000n,
			}),
		]);
		void h.detach();
	});

	test("a second burst waits a full window after the first write settles", async () => {
		const h = attached("parent-task");
		h.emit("a", "started");
		await h.window();
		h.emit("b", "started");
		h.timer.advance(4_999);
		expect(h.patches()).toHaveLength(1);
		h.timer.advance(1);
		await settle();
		expect(h.patches()).toHaveLength(2);
		expect(h.patches()[1]?.upsertTasks[0]?.upsertSubagents).toEqual([
			expect.objectContaining({ id: "b" }),
		]);
		void h.detach();
	});

	test("writes never overlap: events during a slow write wait for it", async () => {
		const h = attached("parent-task");
		const release = h.transport.hold();
		h.emit("a", "started");
		h.timer.advance(5_000);
		h.emit("a", "completed");
		h.timer.advance(10_000);
		expect(h.patches()).toHaveLength(1);
		release();
		await settle();
		// The queued write still waits a full window from the settle.
		expect(h.patches()).toHaveLength(1);
		h.timer.advance(4_999);
		await settle();
		expect(h.patches()).toHaveLength(1);
		h.timer.advance(1);
		await settle();
		expect(h.patches()).toHaveLength(2);
		expect(h.patches()[1]?.upsertTasks[0]?.upsertSubagents[0]?.status).toBe(
			"completed",
		);
		void h.detach();
	});

	test("a failed write re-sends its subagents in the next window", async () => {
		const h = attached("parent-task");
		h.transport.failNext = true;
		h.emit("a", "completed");
		await h.window();
		await h.window();
		expect(h.patches()).toHaveLength(2);
		expect(h.patches()[1]?.upsertTasks[0]?.upsertSubagents[0]?.id).toBe("a");
		void h.detach();
	});

	test("a refused batch re-reads the board and retries once, then drops", async () => {
		const h = attached("parent-task");
		h.transport.refuseWrites = 1;
		h.emit("a", "started");
		await h.window();
		await h.window();
		expect(h.writes()).toHaveLength(2);
		h.transport.refuseWrites = 2;
		h.emit("a", "completed");
		await h.window();
		await h.window();
		await h.window();
		await h.window();
		expect(h.writes()).toHaveLength(4);
		expect(h.patches()).toHaveLength(6);
		void h.detach();
	});

	test("a removed task: the retry lands on the focus and keeps settled rows", async () => {
		const h = attached("parent-task");
		h.transport.addTask("other");
		h.freshness.noteBoard("parent-task", taskKeysOf(h.transport.board));
		h.emit("a", "started");
		await h.window();
		h.transport.focus = "other";
		h.freshness.noteWrite(2n, 2_000, "other", taskKeysOf(h.transport.board));
		h.emit("b", "started");
		await h.window();
		h.transport.removeTask("parent-task");
		h.emit("a", "completed");
		h.emit("b", "completed");
		await h.window();
		await h.window();
		const last = h.writes().at(-1);
		expect(
			last?.upsertTasks.map((t) => [t.key, t.upsertSubagents.map((s) => s.id)]),
		).toEqual([["other", ["a", "b"]]]);
		const other = h.transport.board.tasks.find((t) => t.key === "other");
		expect(other?.subagents.map((s) => [s.id, s.status])).toEqual([
			["a", "completed"],
			["b", "completed"],
		]);
		void h.detach();
	});

	test("a late settle for a sent subagent is ignored; a follow-up start is not", async () => {
		const h = attached("parent-task");
		h.emit("a", "started", "Scan");
		h.emit("a", "completed");
		await h.window();
		h.emit("a", "completed");
		h.timer.advance(10_000);
		await settle();
		expect(h.patches()).toHaveLength(1);
		h.emit("a", "started");
		await h.window();
		expect(h.patches()).toHaveLength(2);
		expect(h.patches()[1]?.upsertTasks[0]?.upsertSubagents[0]?.status).toBe(
			"started",
		);
		void h.detach();
	});

	test("a subagent keeps the task it started on when the focus moves", async () => {
		const h = attached("first");
		h.transport.addTask("second");
		h.freshness.noteBoard("first", taskKeysOf(h.transport.board));
		h.emit("a", "started");
		await h.window();
		h.freshness.noteWrite(2n, 2_000, "second", taskKeysOf(h.transport.board));
		h.emit("a", "completed");
		h.emit("b", "started");
		await h.window();
		const tasks = h.patches()[1]?.upsertTasks ?? [];
		expect(
			tasks.map((t) => [t.key, t.upsertSubagents.map((s) => s.id)]),
		).toEqual([
			["first", ["a"]],
			["second", ["b"]],
		]);
		void h.detach();
	});

	test("an existing task named subagents keeps its title", async () => {
		const h = attached("subagents");
		h.emit("a", "started");
		await h.window();
		expect(h.patches()[0]?.upsertTasks[0]?.title).toBeUndefined();
		void h.detach();
	});

	test("with no focus, an unfocused subagents task keeps its title", async () => {
		const h = attached("");
		h.transport.addTask("subagents", "Mine");
		h.freshness.noteBoard("", taskKeysOf(h.transport.board));
		h.emit("a", "started");
		await h.window();
		expect(h.patches()[0]?.upsertTasks[0]).toMatchObject({ key: "subagents" });
		expect(h.patches()[0]?.upsertTasks[0]?.title).toBeUndefined();
		void h.detach();
	});

	test("with no focus the patch creates a titled subagents task", async () => {
		const h = attached("");
		h.emit("a", "started");
		await h.window();
		expect(h.patches()[0]?.upsertTasks[0]).toMatchObject({
			key: "subagents",
			title: "Subagents",
		});
		void h.detach();
	});

	test("writes wait for the startup seed, then use its focus", async () => {
		const h = attached("", { seeded: false });
		h.emit("a", "started");
		h.timer.advance(15_000);
		await settle();
		expect(h.patches()).toHaveLength(0);
		h.transport.addTask("stored-focus");
		h.freshness.seed(1n, 1_000, "stored-focus", ["stored-focus"]);
		await h.window();
		expect(h.patches()[0]?.upsertTasks[0]?.key).toBe("stored-focus");
		void h.detach();
	});

	test("after a failed seed the sync reads the board before writing", async () => {
		const h = attached("", { seeded: false });
		h.transport.addTask("stored-focus");
		h.transport.focus = "stored-focus";
		h.freshness.seedFailed();
		h.emit("a", "started");
		await h.window();
		expect(h.patches()).toHaveLength(2);
		expect(h.patches()[0]?.upsertTasks).toHaveLength(0);
		expect(h.patches()[1]?.upsertTasks[0]?.key).toBe("stored-focus");
		void h.detach();
	});

	test("a failed read keeps the rows and recovers the focus later", async () => {
		const h = attached("", { seeded: false });
		h.transport.addTask("stored-focus");
		h.transport.focus = "stored-focus";
		h.freshness.seedFailed();
		h.transport.failNext = true;
		h.emit("a", "started");
		await h.window();
		expect(h.writes()).toHaveLength(0);
		await h.window();
		expect(h.writes()[0]?.upsertTasks[0]?.key).toBe("stored-focus");
		void h.detach();
	});

	test("a refused read falls back to a titled subagents task", async () => {
		const h = attached("", { seeded: false });
		h.transport.focus = "stored-focus";
		h.freshness.seedFailed();
		h.transport.refuseNext = true;
		h.emit("a", "started");
		await h.window();
		expect(h.writes()[0]?.upsertTasks[0]).toMatchObject({
			key: "subagents",
			title: "Subagents",
		});
		void h.detach();
	});

	test("detaching sends a pending write before it resolves", async () => {
		const h = attached("parent-task");
		h.emit("a", "started");
		await h.detach();
		expect(h.patches()).toHaveLength(1);
		h.emit("b", "started");
		await h.window();
		expect(h.patches()).toHaveLength(1);
	});

	test("detaching before the seed lands reads the board and sends", async () => {
		const h = attached("parent-task", { seeded: false });
		h.emit("a", "started");
		await h.detach();
		expect(h.writes()).toHaveLength(1);
		expect(h.writes()[0]?.upsertTasks[0]?.key).toBe("parent-task");
	});

	test("a detach that times out during the board read sends no write later", async () => {
		const h = attached("parent-task", { seeded: false });
		const release = h.transport.hold();
		h.emit("a", "started");
		const detached = h.detach();
		await settle();
		h.timer.advance(5_000);
		await detached;
		release();
		await settle();
		expect(h.patches()).toHaveLength(1);
		expect(h.writes()).toHaveLength(0);
	});

	test("detaching gives up on a hung write after 5 s and starts nothing later", async () => {
		const h = attached("parent-task");
		const release = h.transport.hold();
		h.emit("a", "started");
		h.timer.advance(5_000);
		h.emit("b", "started");
		let done = false;
		const detached = h.detach().then(() => {
			done = true;
		});
		await settle();
		expect(done).toBe(false);
		h.timer.advance(5_000);
		await detached;
		expect(done).toBe(true);
		release();
		await settle();
		expect(h.patches()).toHaveLength(1);
	});
});
