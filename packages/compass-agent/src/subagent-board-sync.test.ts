import { describe, expect, test } from "bun:test";
import { TASK_SUBAGENT_LIFECYCLE_CHANNEL } from "@oh-my-pi/pi-coding-agent/task";
import { EventBus } from "@oh-my-pi/pi-coding-agent/utils/event-bus";
import { BoardFreshness } from "./board-freshness";
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
	if (opts.seeded !== false) freshness.seed(1n, 1_000, transport.board);
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
		h.freshness.noteBoard(h.transport.board);
		h.emit("a", "started");
		await h.window();
		h.transport.focus = "other";
		h.freshness.noteWrite(2n, 2_000, h.transport.board);
		h.emit("b", "started");
		await h.window();
		h.transport.removeTask("parent-task");
		h.emit("a", "completed");
		h.emit("b", "completed");
		await h.window();
		await h.window();
		await h.window();
		const other = h.transport.board.tasks.find((t) => t.key === "other");
		expect(other?.subagents.map((s) => [s.id, s.status]).sort()).toEqual([
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

	test("a focus change inside the coalescing window keeps the start's task", async () => {
		const h = attached("first");
		h.transport.addTask("second");
		h.freshness.noteBoard(h.transport.board);
		h.emit("a", "started");
		h.transport.focus = "second";
		h.freshness.noteBoard(h.transport.board);
		h.emit("b", "started");
		await h.window();
		const tasks = h.writes()[0]?.upsertTasks ?? [];
		expect(
			tasks.map((t) => [t.key, t.upsertSubagents.map((s) => s.id)]),
		).toEqual([
			["first", ["a"]],
			["second", ["b"]],
		]);
		void h.detach();
	});

	test("a reused id restarted mid-write takes the focus of its new start", async () => {
		const h = attached("first");
		h.transport.addTask("second");
		h.freshness.noteBoard(h.transport.board);
		h.emit("a", "started");
		await h.window();
		h.emit("a", "completed");
		const release = h.transport.hold();
		h.timer.advance(5_000);
		await settle();
		// The old run's completion is in flight when the follow-up run starts.
		h.transport.focus = "second";
		h.freshness.noteBoard(h.transport.board);
		h.emit("a", "started");
		release();
		await settle();
		await h.window();
		const last = h.writes().at(-1)?.upsertTasks ?? [];
		expect(
			last.map((t) => [t.key, t.upsertSubagents.map((s) => s.status)]),
		).toEqual([["second", ["started"]]]);
		void h.detach();
	});

	test("a reused id's pending settle still lands before the new run", async () => {
		const h = attached("first");
		h.transport.addTask("second");
		h.freshness.noteBoard(h.transport.board);
		h.emit("a", "started");
		await h.window();
		h.emit("a", "completed");
		h.transport.focus = "second";
		h.freshness.noteBoard(h.transport.board);
		h.emit("a", "started");
		await h.window();
		await h.window();
		const rows = (key: string) =>
			h.transport.board.tasks
				.find((t) => t.key === key)
				?.subagents.map((s) => `${s.id}:${s.status}`);
		expect(rows("first")).toEqual(["a:completed"]);
		expect(rows("second")).toEqual(["a:started"]);
		void h.detach();
	});

	test("every run queued behind a pending settle reaches its own task", async () => {
		const h = attached("first");
		h.transport.addTask("second");
		h.transport.addTask("third");
		h.freshness.noteBoard(h.transport.board);
		const focus = (key: string) => {
			h.transport.focus = key;
			h.freshness.noteBoard(h.transport.board);
		};
		h.emit("a", "started");
		await h.window();
		h.emit("a", "completed");
		focus("second");
		h.emit("a", "started");
		h.emit("a", "completed");
		focus("third");
		h.emit("a", "started");
		for (let i = 0; i < 4; i++) await h.window();
		const rows = (key: string) =>
			h.transport.board.tasks
				.find((t) => t.key === key)
				?.subagents.map((s) => `${s.id}:${s.status}`);
		expect(rows("first")).toEqual(["a:completed"]);
		expect(rows("second")).toEqual(["a:completed"]);
		expect(rows("third")).toEqual(["a:started"]);
		void h.detach();
	});

	test("a restart before a queued run settles stays on that run's task", async () => {
		const h = attached("first");
		h.transport.addTask("second");
		h.transport.addTask("third");
		h.freshness.noteBoard(h.transport.board);
		const focus = (key: string) => {
			h.transport.focus = key;
			h.freshness.noteBoard(h.transport.board);
		};
		h.emit("a", "started");
		await h.window();
		h.emit("a", "completed");
		focus("second");
		h.emit("a", "started");
		focus("third");
		h.emit("a", "started");
		h.emit("a", "completed");
		for (let i = 0; i < 4; i++) await h.window();
		const open = h.transport.board.tasks.flatMap((t) =>
			t.subagents.filter((s) => s.endedAtUnixMs === 0n).map(() => t.key),
		);
		expect(open).toEqual([]);
		const second = h.transport.board.tasks.find((t) => t.key === "second");
		expect(second?.subagents.map((s) => s.status)).toEqual(["completed"]);
		void h.detach();
	});

	test("an id restarting past the queue cap drops its oldest queued run", async () => {
		const h = attached("t0");
		for (let i = 1; i <= 10; i++) h.transport.addTask(`t${i}`);
		h.freshness.noteBoard(h.transport.board);
		h.emit("a", "started");
		await h.window();
		h.emit("a", "completed");
		const logged: unknown[][] = [];
		const original = console.error;
		console.error = (...args: unknown[]) => logged.push(args);
		try {
			for (let i = 1; i <= 9; i++) {
				h.transport.focus = `t${i}`;
				h.freshness.noteBoard(h.transport.board);
				h.emit("a", "started");
				h.emit("a", "completed");
			}
		} finally {
			console.error = original;
		}
		expect(logged.map((a) => String(a[0]))).toContain(
			"[compass-agent] subagent board run dropped:",
		);
		for (let i = 0; i < 12; i++) await h.window();
		const landed = h.transport.board.tasks
			.filter((t) => t.subagents.length > 0)
			.map((t) => t.key);
		expect(landed).toEqual([
			"t0",
			"t2",
			"t3",
			"t4",
			"t5",
			"t6",
			"t7",
			"t8",
			"t9",
		]);
		void h.detach();
	});

	test("a settle during its start's write follows the start's task", async () => {
		const h = attached("first", { seeded: false });
		h.transport.addTask("second");
		h.emit("a", "started");
		h.freshness.seed(1n, 1_000, h.transport.board);
		const release = h.transport.hold();
		h.timer.advance(5_000);
		await settle();
		h.emit("a", "completed");
		h.transport.focus = "second";
		release();
		await settle();
		h.freshness.noteBoard(h.transport.board);
		await h.window();
		const first = h.transport.board.tasks.find((t) => t.key === "first");
		expect(first?.subagents.map((s) => s.status)).toEqual(["completed"]);
		void h.detach();
	});

	test("a fault on an older row does not spend the newer row's retries", async () => {
		const h = attached("parent-task");
		h.emit("a", "started");
		const release = h.transport.hold();
		h.transport.faultWrites = 4;
		h.timer.advance(5_000);
		await settle();
		h.emit("a", "completed");
		release();
		await settle();
		for (let i = 0; i < 5; i++) await h.window();
		expect(h.patches().filter((p) => p.upsertTasks.length === 0)).toHaveLength(
			0,
		);
		const task = h.transport.board.tasks.find((t) => t.key === "parent-task");
		expect(task?.subagents.map((s) => s.status)).toEqual(["completed"]);
		void h.detach();
	});

	test("a row the store trims from a full task is logged", async () => {
		const h = attached("parent-task");
		// The first send runs the startup sweep; rows stored after it are a live writer's.
		h.emit("first", "completed");
		await h.window();
		h.transport.addOpenRows(
			"parent-task",
			Array.from({ length: 20 }, (_, i) => `o${i}`),
		);
		h.freshness.noteBoard(h.transport.board);
		const logged: unknown[][] = [];
		const original = console.error;
		console.error = (...args: unknown[]) => logged.push(args);
		try {
			h.emit("c", "completed");
			await h.window();
		} finally {
			console.error = original;
		}
		expect(logged.map((a) => String(a[0]))).toContain(
			"[compass-agent] subagent board rows trimmed:",
		);
		expect(String(logged.at(-1)?.[1])).toContain("dropped c");
		void h.detach();
	});

	test("a subagent keeps the task it started on when the focus moves", async () => {
		const h = attached("first");
		h.transport.addTask("second");
		h.freshness.noteBoard(h.transport.board);
		h.emit("a", "started");
		await h.window();
		h.transport.focus = "second";
		h.freshness.noteWrite(2n, 2_000, h.transport.board);
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
		h.freshness.noteBoard(h.transport.board);
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
		h.freshness.seed(1n, 1_000, board({ focusTaskKey: "stored-focus" }));
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

	test("a refused read drops the pending rows rather than guess a task", async () => {
		const h = attached("", { seeded: false });
		h.transport.addTask("subagents", "Mine");
		h.freshness.seedFailed();
		h.transport.refuseNext = true;
		h.emit("a", "started");
		await h.window();
		await h.window();
		expect(h.writes()).toHaveLength(0);
		h.emit("b", "started");
		await h.window();
		expect(h.writes()[0]?.upsertTasks[0]).toMatchObject({ key: "subagents" });
		expect(h.writes()[0]?.upsertTasks[0]?.title).toBeUndefined();
		void h.detach();
	});

	test("a bad row refused twice drops alone; its batch mates still land", async () => {
		const h = attached("parent-task");
		h.emit("a", "completed");
		h.emit("b", "completed");
		h.transport.refuseWrites = 2;
		await h.window();
		await h.window();
		await h.window();
		await h.window();
		const task = h.transport.board.tasks.find((t) => t.key === "parent-task");
		expect(task?.subagents.map((s) => s.id)).toEqual(["b"]);
		void h.detach();
	});

	test("starts past the server's open-row cap wait until rows settle", async () => {
		const h = attached("parent-task");
		for (let i = 0; i < 25; i++) h.emit(`s${i}`, "started");
		await h.window();
		await h.window();
		expect(
			h.writes().map((w) => w.upsertTasks[0]?.upsertSubagents.length),
		).toEqual([20]);
		for (let i = 0; i < 5; i++) h.emit(`s${i}`, "completed");
		await h.window();
		await h.window();
		const task = h.transport.board.tasks.find((t) => t.key === "parent-task");
		expect(task?.subagents.filter((s) => s.endedAtUnixMs === 0n)).toHaveLength(
			20,
		);
		expect(task?.subagents.map((s) => s.id)).toContain("s24");
		void h.detach();
	});

	test("open rows another writer stored count toward the cap", async () => {
		const h = attached("parent-task");
		h.emit("first", "completed");
		await h.window();
		h.transport.addOpenRows(
			"parent-task",
			Array.from({ length: 20 }, (_, i) => `o${i}`),
		);
		h.freshness.noteBoard(h.transport.board);
		h.emit("s", "started");
		await h.window();
		expect(h.writes()).toHaveLength(1);
		void h.detach();
	});

	test("a refused start with no room does not hold back other rows", async () => {
		const h = attached("parent-task");
		h.transport.refuseWrites = 1;
		h.emit("x", "started");
		await h.window();
		// Another writer filled the task; "c" is one of its open rows.
		h.transport.addOpenRows("parent-task", [
			"c",
			...Array.from({ length: 19 }, (_, i) => `o${i}`),
		]);
		h.emit("c", "completed");
		await h.window();
		await h.window();
		// "x" waits for room; "c" goes out and frees it.
		expect(
			h
				.writes()
				.map((w) =>
					w.upsertTasks[0]?.upsertSubagents.map((s) => `${s.id}:${s.status}`),
				),
		).toEqual([["x:started"], ["c:completed"], ["x:started"]]);
		void h.detach();
	});

	test("a row that keeps faulting is dropped after bounded retries", async () => {
		const h = attached("parent-task");
		h.transport.faultWrites = 1_000;
		h.emit("a", "completed");
		for (let i = 0; i < 12; i++) await h.window();
		const attempts = h.writes().length;
		for (let i = 0; i < 12; i++) await h.window();
		expect(h.writes()).toHaveLength(attempts);
		h.transport.faultWrites = 0;
		h.emit("b", "completed");
		await h.window();
		const task = h.transport.board.tasks.find((t) => t.key === "parent-task");
		expect(task?.subagents.map((s) => s.id)).toEqual(["b"]);
		void h.detach();
	});

	test("thrown transport failures share the bounded retries", async () => {
		const h = attached("parent-task");
		let thrown = 0;
		const comms = h.transport.comms.bind(h.transport);
		h.transport.comms = async (req) => {
			if (
				req.call.case === "updateBoard" &&
				req.call.value.upsertTasks.length > 0
			) {
				thrown += 1;
				throw new Error("transport down");
			}
			return comms(req);
		};
		h.emit("a", "completed");
		for (let i = 0; i < 12; i++) await h.window();
		const attempts = thrown;
		for (let i = 0; i < 12; i++) await h.window();
		expect(thrown).toBe(attempts);
		void h.detach();
	});

	test("an open row left by an earlier process is closed as aborted", async () => {
		const h = attached("parent-task", { seeded: false });
		h.transport.addOpenRows("parent-task", ["old"]);
		h.freshness.seed(1n, 1_000, h.transport.board);
		// An idle sync makes no calls; the sweep rides the first real send.
		await h.window();
		expect(h.patches()).toHaveLength(0);
		h.emit("live", "started");
		await h.window();
		const task = h.transport.board.tasks.find((t) => t.key === "parent-task");
		expect(task?.subagents.map((s) => [s.id, s.status])).toEqual([
			["old", "aborted"],
			["live", "started"],
		]);
		void h.detach();
	});

	test("detaching closes this session's open rows as aborted", async () => {
		const h = attached("parent-task");
		h.emit("a", "started");
		await h.window();
		h.emit("b", "started");
		await h.detach();
		const task = h.transport.board.tasks.find((t) => t.key === "parent-task");
		expect(task?.subagents.map((s) => [s.id, s.status])).toEqual([
			["a", "aborted"],
			["b", "aborted"],
		]);
	});

	test("a canceled call is transient: the rows retry without a re-read", async () => {
		const h = attached("parent-task");
		h.transport.faultCode = "canceled";
		h.transport.faultWrites = 1;
		h.emit("a", "started");
		await h.window();
		await h.window();
		expect(h.patches().filter((p) => p.upsertTasks.length === 0)).toHaveLength(
			0,
		);
		const task = h.transport.board.tasks.find((t) => t.key === "parent-task");
		expect(task?.subagents.map((s) => s.id)).toEqual(["a"]);
		void h.detach();
	});

	test("an in-band server fault keeps rows pending; a read fault too", async () => {
		const h = attached("parent-task");
		h.transport.faultNext = true;
		h.emit("a", "started");
		await h.window();
		h.freshness.forgetBoard();
		h.transport.faultNext = true;
		await h.window();
		await h.window();
		const task = h.transport.board.tasks.find((t) => t.key === "parent-task");
		expect(task?.subagents.map((s) => s.id)).toEqual(["a"]);
		void h.detach();
	});

	test("a new event after a refusal gets its own retry", async () => {
		const h = attached("parent-task");
		h.transport.refuseWrites = 1;
		h.emit("a", "started");
		await h.window();
		// The re-read faults, so "a" stays pending with its refusal mark.
		h.transport.faultNext = true;
		await h.window();
		h.emit("a", "completed");
		h.transport.refuseWrites = 1;
		await h.window();
		await h.window();
		await h.window();
		const task = h.transport.board.tasks.find((t) => t.key === "parent-task");
		expect(task?.subagents.map((s) => [s.id, s.status])).toEqual([
			["a", "completed"],
		]);
		void h.detach();
	});

	test("rows are clipped to the server's field caps; an over-long id is skipped", async () => {
		const h = attached("parent-task");
		h.emit("a", "started", `${"d".repeat(250)}\0`);
		h.emit("i".repeat(65), "started");
		await h.window();
		const rows = h.writes()[0]?.upsertTasks[0]?.upsertSubagents ?? [];
		expect(rows.map((r) => [r.id, [...r.description].length])).toEqual([
			["a", 200],
		]);
		void h.detach();
	});

	test("a seed landing after a sync read keeps the newer board", () => {
		const freshness = new BoardFreshness();
		freshness.noteBoard(
			board({ focusTaskKey: "new", writtenAtUnixMs: 4_000n }),
		);
		freshness.forgetBoard();
		freshness.seed(1n, 5_000, board({ focusTaskKey: "old" }));
		expect(freshness.focusTaskKey()).toBe("new");
		expect(freshness.boardKnown()).toBe(false);
		expect(freshness.ageMs(6_000)).toBe(2_000);
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

	test("detaching sends every capped batch and a refused row's retry", async () => {
		const h = attached("parent-task");
		for (let i = 0; i < 25; i++) h.emit(`s${i}`, "completed");
		h.transport.refuseWrites = 1;
		await h.detach();
		const task = h.transport.board.tasks.find((t) => t.key === "parent-task");
		expect(task?.subagents).toHaveLength(20);
		expect(h.writes().length).toBeGreaterThanOrEqual(3);
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
