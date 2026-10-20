import { describe, expect, test } from "bun:test";
import { BoardFreshness, seedBoardFreshness } from "./board-freshness";
import {
	board,
	FakeTimer,
	FakeTransport,
	sequence,
	settle,
} from "./board-test-fakes";
import { CommsBroker } from "./comms";

describe("seedBoardFreshness", () => {
	test("reads the board with an empty automatic patch", async () => {
		const transport = new FakeTransport(
			board({ writtenTurn: 12n, writtenAtUnixMs: 30_000n }),
		);
		const freshness = new BoardFreshness();
		await seedBoardFreshness(new CommsBroker(transport), sequence(), freshness);
		const request = transport.requests[0];
		expect(request?.call.case).toBe("updateBoard");
		if (request?.call.case !== "updateBoard")
			throw new Error("expected updateBoard request");
		expect(request.call.value).toMatchObject({
			upsertTasks: [],
			removeTaskKeys: [],
			automatic: true,
		});
		// The stored turn belongs to another session's sequence; only time and tasks seed.
		expect(freshness.wroteThisTurn(12n)).toBe(false);
		expect(freshness.writeCount()).toBe(0);
		expect(freshness.ageMs(31_000)).toBe(1_000);
		expect(freshness.focusTaskKey()).toBe("focus");
		expect(freshness.hasTask("focus")).toBe(true);
	});

	test("a seed that lands after this session wrote changes nothing", () => {
		const freshness = new BoardFreshness();
		freshness.noteWrite(3n, 50_000, board({ focusTaskKey: "mine" }));
		freshness.seed(2n, 10_000, board({ focusTaskKey: "old" }));
		expect(freshness.ageMs(51_000)).toBe(1_000);
		expect(freshness.focusTaskKey()).toBe("mine");
		expect(freshness.hasTask("old")).toBe(false);
		expect(freshness.wroteThisTurn(3n)).toBe(true);
		expect(freshness.wroteThisTurn(2n)).toBe(false);
	});

	test("a seed that lands after a newer board read keeps that read's write time", () => {
		const freshness = new BoardFreshness();
		freshness.noteBoard(
			board({ focusTaskKey: "new", writtenAtUnixMs: 50_000n }),
		);
		freshness.seed(2n, 10_000, board({ focusTaskKey: "old" }));
		expect(freshness.ageMs(51_000)).toBe(1_000);
		expect(freshness.focusTaskKey()).toBe("new");
		expect(freshness.seeded()).toBe(true);
	});

	test("a seed with no reply fails at the deadline and settles freshness", async () => {
		const timer = new FakeTimer();
		const transport = new FakeTransport(board());
		transport.hold();
		const freshness = new BoardFreshness();
		const seeded = seedBoardFreshness(
			new CommsBroker(transport),
			sequence(),
			freshness,
			timer,
		).catch((error: unknown) => {
			freshness.seedFailed();
			return error;
		});
		timer.advance(9_999);
		await settle();
		expect(freshness.seeded()).toBe(false);
		timer.advance(1);
		expect(String(await seeded)).toContain("no reply within 10 s");
		expect(freshness.seeded()).toBe(true);
		expect(freshness.ageMs(0)).toBeUndefined();
	});

	test("a reply after the deadline still seeds", async () => {
		const timer = new FakeTimer();
		const transport = new FakeTransport(board({ focusTaskKey: "late" }));
		const release = transport.hold();
		const freshness = new BoardFreshness();
		const seeded = seedBoardFreshness(
			new CommsBroker(transport),
			sequence(),
			freshness,
			timer,
		).catch(() => freshness.seedFailed());
		timer.advance(10_000);
		await seeded;
		expect(freshness.boardKnown()).toBe(false);
		release();
		await settle();
		expect(freshness.focusTaskKey()).toBe("late");
		expect(freshness.boardKnown()).toBe(true);
	});
});
