// TranscriptTeeBackend (SEA-1570 T2): the container-local FS backend that tees
// every committed session write upstream as a durable TranscriptEntry frame.
//
// Each test defends a load-bearing contract over a RECORDING FrameSink (captures
// the frames + lets a test force a definitive reject) and a temp session dir:
//   - append writes locally AND emits a delta frame (verbatim line + monotonic
//     bigint entry_seq);
//   - writeFull writes locally AND emits a checkpoint frame;
//   - local reads round-trip what was written;
//   - ops on one path emit in commit order even under interleaved slow sends
//     (the awaited-op ordering);
//   - a compaction-shaped writeFull after appends emits a checkpoint AFTER the
//     deltas it supersedes;
//   - a definitively-erred emit FAILS THE SESSION CLOSED — the op re-throws a
//     fatal error, it is surfaced once on stderr, and every subsequent op
//     re-throws the latched error (R4, fail-closed per rule://no-retries; no
//     backend buffer/retry tier).
//
// No sockets, no timers-as-sleeps: ordering is gated on a deferred the test
// controls, not a wall-clock wait.

import { afterEach, describe, expect, spyOn, test } from "bun:test";
import {
	existsSync,
	mkdtempSync,
	readFileSync,
	rmSync,
	writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { SESSION_TITLE_SLOT_BYTES } from "@oh-my-pi/pi-coding-agent/session/session-entries";
import {
	parseTitleSlotFromContent,
	serializeTitleSlot,
} from "@oh-my-pi/pi-coding-agent/session/session-title-slot";
import type { OutboundFrame } from "./frame";
import { createTeeSessionStorage, TranscriptTeeBackend } from "./session-tee";

const tmpdirs: string[] = [];

function scratch(): string {
	const dir = mkdtempSync(join(tmpdir(), "compass-tee-"));
	tmpdirs.push(dir);
	return dir;
}

afterEach(() => {
	while (tmpdirs.length > 0) {
		const dir = tmpdirs.pop();
		if (dir) rmSync(dir, { recursive: true, force: true });
	}
});

// A recording FrameSink. `emitDurable` captures each frame and resolves; a test
// can swap in test controls: `failAll` (every send rejects — a definitive
// give-up, i.e. the sink's own transport retry already exhausted), `failTimes`
// (reject the first N sends then succeed — the transient blip the backend's own
// retry tier absorbs), or `gate` (hold a send open for the ordering test).
// `emit`/`drain` are unused by the backend but satisfy the type.
interface RecordingSink {
	readonly frames: OutboundFrame[];
	emit(frame: OutboundFrame): void;
	emitDurable(frame: OutboundFrame): Promise<void>;
	drain(): Promise<void>;
	// Test controls:
	failAll?: Error;
	failTimes?: number;
	gate?: (frame: OutboundFrame) => Promise<void>;
	// Called synchronously when emitDurable is ENTERED (before the gate/hold) —
	// a deterministic signal that a send was reached, independent of local fs
	// timing. Used by the ordering test.
	onEmitEntered?: (frame: OutboundFrame) => void;
	// How many emitDurable calls were made (successes + rejects).
	attempts: number;
}

function recordingSink(): RecordingSink {
	const sink: RecordingSink = {
		frames: [],
		attempts: 0,
		emit(frame) {
			sink.frames.push(frame);
		},
		async emitDurable(frame) {
			sink.attempts += 1;
			sink.onEmitEntered?.(frame);
			if (sink.failAll) throw sink.failAll;
			if (sink.failTimes && sink.failTimes > 0) {
				sink.failTimes -= 1;
				throw new Error("transient unary give-up");
			}
			if (sink.gate) await sink.gate(frame);
			sink.frames.push(frame);
		},
		drain: () => Promise.resolve(),
	};
	return sink;
}

// The domain shape of a recorded transcript frame — entry_json + checkpoint +
// entry_seq. A helper because every assertion reads exactly these three.
function transcript(frame: OutboundFrame): {
	entryJson: string;
	checkpoint: boolean;
	entrySeq: bigint;
} {
	if (frame.kind !== "transcriptEntry") {
		throw new Error(`expected transcriptEntry frame, got ${frame.kind}`);
	}
	return {
		entryJson: frame.value.entryJson,
		checkpoint: frame.value.checkpoint,
		entrySeq: frame.value.entrySeq,
	};
}

describe("TranscriptTeeBackend.append", () => {
	// The delta path: the line lands on the local file AND a delta frame goes
	// upstream, verbatim. Non-vacuity: a backend that only wrote locally (or only
	// emitted) fails one half; a backend that mangled the line fails the verbatim
	// assert.
	test("writes the line locally AND emits a verbatim delta frame", async () => {
		const dir = scratch();
		const file = join(dir, "s.jsonl");
		const sink = recordingSink();
		const backend = new TranscriptTeeBackend(sink, dir);

		await backend.append(file, '{"a":1}\n', 1);

		expect(readFileSync(file, "utf-8")).toBe('{"a":1}\n');
		expect(sink.frames).toHaveLength(1);
		expect(transcript(sink.frames[0])).toEqual({
			entryJson: '{"a":1}\n',
			checkpoint: false,
			entrySeq: 1n,
		});
	});

	// entry_seq is monotonic from 1n per container lifetime, as a bigint (proto
	// uint64). Non-vacuity: a number counter, or one starting at 0, reds here.
	test("stamps a monotonic bigint entry_seq from 1n", async () => {
		const dir = scratch();
		const file = join(dir, "s.jsonl");
		const sink = recordingSink();
		const backend = new TranscriptTeeBackend(sink, dir);

		await backend.append(file, "a\n", 1);
		await backend.append(file, "b\n", 2);
		await backend.append(file, "c\n", 3);

		expect(sink.frames.map((f) => transcript(f).entrySeq)).toEqual([
			1n,
			2n,
			3n,
		]);
	});

	test("appends accumulate on the local file", async () => {
		const dir = scratch();
		const file = join(dir, "s.jsonl");
		const sink = recordingSink();
		const backend = new TranscriptTeeBackend(sink, dir);

		await backend.append(file, "a\n", 1);
		await backend.append(file, "b\n", 2);

		expect(readFileSync(file, "utf-8")).toBe("a\nb\n");
	});
});

describe("TranscriptTeeBackend.writeFull", () => {
	// The checkpoint path: the full body is atomically written locally AND a
	// checkpoint frame (checkpoint: true) carries it upstream. Non-vacuity: a
	// backend that emitted checkpoint:false, or skipped the local write, reds.
	test("writes the body locally AND emits a checkpoint frame", async () => {
		const dir = scratch();
		const file = join(dir, "s.jsonl");
		const sink = recordingSink();
		const backend = new TranscriptTeeBackend(sink, dir);

		await backend.writeFull(file, "full-body\n", 1);

		expect(readFileSync(file, "utf-8")).toBe("full-body\n");
		expect(sink.frames).toHaveLength(1);
		expect(transcript(sink.frames[0])).toEqual({
			entryJson: "full-body\n",
			checkpoint: true,
			entrySeq: 1n,
		});
	});

	test("writeFull overwrites prior local content", async () => {
		const dir = scratch();
		const file = join(dir, "s.jsonl");
		const sink = recordingSink();
		const backend = new TranscriptTeeBackend(sink, dir);

		await backend.append(file, "old\n", 1);
		await backend.writeFull(file, "new\n", 2);

		expect(readFileSync(file, "utf-8")).toBe("new\n");
	});
});

describe("TranscriptTeeBackend reads", () => {
	// Reads are REAL local FS (load-bearing for resume). Non-vacuity: a backend
	// that returned index-only or stubbed content reds the round-trip.
	test("readFull round-trips written content, null for a missing path", async () => {
		const dir = scratch();
		const file = join(dir, "s.jsonl");
		const sink = recordingSink();
		const backend = new TranscriptTeeBackend(sink, dir);

		await backend.append(file, "line\n", 1);

		expect(await backend.readFull(file)).toBe("line\n");
		expect(await backend.readFull(join(dir, "absent.jsonl"))).toBeNull();
	});

	test("readSlices returns head + tail windows, ENOENT for a missing path", async () => {
		const dir = scratch();
		const file = join(dir, "s.jsonl");
		const sink = recordingSink();
		const backend = new TranscriptTeeBackend(sink, dir);
		writeFileSync(file, "HEADERxxxxxxxxTAIL");

		const [head, tail] = await backend.readSlices(file, 6, 4);
		expect(head).toBe("HEADER");
		expect(tail).toBe("TAIL");

		await expect(
			backend.readSlices(join(dir, "absent.jsonl"), 4, 4),
		).rejects.toThrow(/ENOENT/);
	});

	// loadIndex scans the real dir for *.jsonl — the resume file must appear so
	// the SDK wrapper does not ENOENT it. Non-vacuity: a backend that returned an
	// empty index would leave resume files invisible.
	test("loadIndex lists the *.jsonl files in the session dir", async () => {
		const dir = scratch();
		const sink = recordingSink();
		const backend = new TranscriptTeeBackend(sink, dir);
		writeFileSync(join(dir, "a.jsonl"), "aa");
		writeFileSync(join(dir, "b.jsonl"), "bbbb");
		writeFileSync(join(dir, "not-a-session.txt"), "x");

		const index = [...(await backend.loadIndex())].sort((l, r) =>
			l.path.localeCompare(r.path),
		);
		expect(index.map((e) => e.path)).toEqual([
			join(dir, "a.jsonl"),
			join(dir, "b.jsonl"),
		]);
		expect(index.map((e) => e.size)).toEqual([2, 4]);
	});

	test("loadIndex is empty for a fresh dir", async () => {
		const dir = scratch();
		const sink = recordingSink();
		const backend = new TranscriptTeeBackend(sink, dir);
		expect([...(await backend.loadIndex())]).toHaveLength(0);
	});
});

describe("TranscriptTeeBackend ordering", () => {
	// The composite ordering guarantee (design.md:764-767): the SDK wrapper's
	// per-path tail chain serializes ops per path, and the backend AWAITS each
	// emit INSIDE the op — so per-session emit order == send order == commit
	// order. This drives two same-path appends through the REAL
	// IndexedSessionStorage (the production write vector, openWriter().append),
	// exactly as the SDK does — the backend is not (and need not be) independently
	// concurrency-safe; the wrapper is the single locus of same-path ordering.
	//
	// Non-vacuity: the first send is HELD open. A backend that fired emits
	// fire-and-forget (not awaiting inside the op) would let the wrapper's op
	// complete and the second op's frame land while the first is still held →
	// order inverts. Because the emit is awaited inside the op, the wrapper's
	// chain cannot advance to the second append until the first send commits.
	test("emits in commit order across serialized same-path ops (via the SDK wrapper)", async () => {
		const dir = scratch();
		const file = join(dir, "s.jsonl");
		const sink = recordingSink();
		const { storage } = await createTeeSessionStorage(sink, dir);

		// Hold the FIRST send (entry_seq 1n) open until released; later sends are
		// instant. `firstEmitEntered` fires when emitDurable for seq 1n is reached
		// — AFTER the op's local fs write in BOTH impls, so it factors out fs
		// timing and makes the assertion deterministic.
		const held = Promise.withResolvers<void>();
		const firstEmitEntered = Promise.withResolvers<void>();
		sink.onEmitEntered = (frame) => {
			if (transcript(frame).entrySeq === 1n) firstEmitEntered.resolve();
		};
		sink.gate = (frame) =>
			transcript(frame).entrySeq === 1n ? held.promise : Promise.resolve();

		// The production append vector: one writer, two appends. The wrapper's
		// per-path tail chain serializes them; each op awaits its backend.append,
		// which awaits the emit.
		const writer = storage.openWriter(file, { flags: "a" });
		let firstDone = false;
		let secondDone = false;
		const first = writer.append("first\n").then(() => {
			firstDone = true;
		});
		const second = writer.append("second\n").then(() => {
			secondDone = true;
		});

		// Wait until the first send is REACHED — a deterministic signal that fires
		// AFTER the op's local fs.appendFile in both impls, so fs timing is
		// factored out. Then drain the whole microtask queue with one setImmediate
		// (a macrotask runs strictly after every queued microtask). Under the
		// correct impl backend.append is still blocked awaiting the held emit, so
		// neither op has resolved and no frame has landed. A fire-and-forget
		// backend would have returned from the op after the local write (never
		// awaiting the emit), so `first` resolves within that drain → firstDone
		// true → red.
		await firstEmitEntered.promise;
		const drained = Promise.withResolvers<void>();
		setImmediate(drained.resolve);
		await drained.promise;
		expect(firstDone).toBe(false);
		expect(secondDone).toBe(false);
		expect(sink.frames).toHaveLength(0);

		held.resolve();
		await Promise.all([first, second]);
		await writer.close();

		expect(firstDone && secondDone).toBe(true);
		// Frames landed in commit order, with monotonic entry_seq.
		expect(sink.frames.map((f) => transcript(f).entryJson)).toEqual([
			"first\n",
			"second\n",
		]);
		expect(sink.frames.map((f) => transcript(f).entrySeq)).toEqual([1n, 2n]);
		// And the local file bytes are in the same order (no interleave).
		expect(readFileSync(file, "utf-8")).toBe("first\nsecond\n");
	});

	// A compaction-shaped writeFull after appends: the checkpoint emits AFTER the
	// deltas it supersedes. Non-vacuity: a checkpoint that raced ahead of the
	// buffered deltas would violate supersession ordering (T4).
	test("a compaction writeFull after appends emits the checkpoint last", async () => {
		const dir = scratch();
		const file = join(dir, "s.jsonl");
		const sink = recordingSink();
		const backend = new TranscriptTeeBackend(sink, dir);

		await backend.append(file, "d1\n", 1);
		await backend.append(file, "d2\n", 2);
		await backend.writeFull(file, "compacted\n", 3);

		expect(
			sink.frames.map((f) => {
				const t = transcript(f);
				return { checkpoint: t.checkpoint, entrySeq: t.entrySeq };
			}),
		).toEqual([
			{ checkpoint: false, entrySeq: 1n },
			{ checkpoint: false, entrySeq: 2n },
			{ checkpoint: true, entrySeq: 3n },
		]);
	});
});

describe("TranscriptTeeBackend erred emit — buffer + retry + fatal at cap (R4)", () => {
	// A transient reject (the sink's transport retry blipped) is absorbed by the
	// backend's OWN retry tier: the send is retried and the op still succeeds.
	// Non-vacuity: a backend with no retry tier would re-throw on the first
	// reject → red.
	test("a transient reject is retried and the op still succeeds", async () => {
		const dir = scratch();
		const file = join(dir, "s.jsonl");
		const sink = recordingSink();
		sink.failTimes = 2; // two rejects, then the third attempt lands
		const backend = new TranscriptTeeBackend(sink, dir, {
			emitBackoffMs: [0, 0, 0, 0],
		});
		const warnSpy = spyOn(console, "warn").mockImplementation(() => {});

		try {
			await backend.append(file, "x\n", 1);
		} finally {
			warnSpy.mockRestore();
		}

		// Three emitDurable calls (2 fail + 1 success); the frame landed once.
		expect(sink.attempts).toBe(3);
		expect(sink.frames).toHaveLength(1);
		expect(transcript(sink.frames[0]).entryJson).toBe("x\n");
	});

	// The escalation contract: retries log warn, and the last retry before the
	// cap logs error. Non-vacuity: a flat log level (all warn, or all error)
	// fails one of the two assertions.
	test("retries escalate the log level warn → error before the cap", async () => {
		const dir = scratch();
		const file = join(dir, "s.jsonl");
		const sink = recordingSink();
		sink.failAll = new Error("unary give-up");
		// cap = 2: attempt 0 warns, attempt 1 (last before cap) errors, then fatal.
		const backend = new TranscriptTeeBackend(sink, dir, {
			emitBackoffMs: [0, 0],
		});
		const warnSpy = spyOn(console, "warn").mockImplementation(() => {});
		const errSpy = spyOn(console, "error").mockImplementation(() => {});

		try {
			await expect(backend.append(file, "x\n", 1)).rejects.toThrow(
				"unary give-up",
			);
			// attempt 0 → warn; attempt 1 → error (last before cap).
			expect(warnSpy).toHaveBeenCalledTimes(1);
			// One escalated-retry error + one FATAL error at the cap.
			expect(errSpy).toHaveBeenCalledTimes(2);
			expect(errSpy.mock.calls.at(-1)?.[0]).toContain("FATAL");
			// cap+1 emitDurable attempts (0,1,2) before the fatal latch.
			expect(sink.attempts).toBe(3);
		} finally {
			warnSpy.mockRestore();
			errSpy.mockRestore();
		}
		// The local write still happened (the tee follows the local commit).
		expect(readFileSync(file, "utf-8")).toBe("x\n");
	});

	// The fatal error latches at cap: every SUBSEQUENT op re-throws it, even once
	// the sink would succeed again — mirroring the SDK #diskFailure latch, a
	// durable-write failure is fatal-by-design. Non-vacuity: a backend that
	// recovered on the next op would resolve → red.
	test("subsequent ops re-throw the latched fatal error after the cap", async () => {
		const dir = scratch();
		const file = join(dir, "s.jsonl");
		const sink = recordingSink();
		sink.failAll = new Error("unary give-up");
		const backend = new TranscriptTeeBackend(sink, dir, { emitBackoffMs: [0] });
		const warnSpy = spyOn(console, "warn").mockImplementation(() => {});
		const errSpy = spyOn(console, "error").mockImplementation(() => {});

		try {
			await expect(backend.append(file, "x\n", 1)).rejects.toThrow(
				"unary give-up",
			);
			const attemptsAtLatch = sink.attempts;
			// Sink would work now — but the latch stands.
			sink.failAll = undefined;
			await expect(backend.append(file, "y\n", 2)).rejects.toThrow(
				"unary give-up",
			);
			await expect(backend.writeFull(file, "z\n", 3)).rejects.toThrow(
				"unary give-up",
			);
			// The latch short-circuits BEFORE the sink: no new emitDurable attempts.
			expect(sink.attempts).toBe(attemptsAtLatch);
		} finally {
			warnSpy.mockRestore();
			errSpy.mockRestore();
		}
	});
});

describe("TranscriptTeeBackend local-only + latch on the remaining methods", () => {
	// A semantic title update, the exact shape the SDK write vector carries.
	const titleUpdate = {
		title: "t",
		updatedAt: "2026-01-01T00:00:00.000Z",
	} satisfies NonNullable<
		Parameters<TranscriptTeeBackend["updateSessionTitle"]>[1]
	>;

	// (1a) The fatal latch is enforced by EVERY op, not just append/writeFull.
	// Mirror the 434-462 pattern: latch via a failAll append at cap, snapshot
	// attempts, clear failAll, then assert each remaining method re-throws the
	// latched error AND makes NO new durable-send attempt (the latch
	// short-circuits BEFORE the sink). Non-vacuity: a method missing its
	// #throwIfFatal() would run its local fs mutation and resolve → red.
	test("truncate/remove/move/updateSessionTitle re-throw the latched fatal after the cap", async () => {
		const dir = scratch();
		const file = join(dir, "s.jsonl");
		const file2 = join(dir, "s2.jsonl");
		const sink = recordingSink();
		sink.failAll = new Error("unary give-up");
		const backend = new TranscriptTeeBackend(sink, dir, { emitBackoffMs: [0] });
		const warnSpy = spyOn(console, "warn").mockImplementation(() => {});
		const errSpy = spyOn(console, "error").mockImplementation(() => {});

		try {
			await expect(backend.append(file, "x\n", 1)).rejects.toThrow(
				"unary give-up",
			);
			const attemptsAtLatch = sink.attempts;
			// Sink would work now — but the latch stands for every op.
			sink.failAll = undefined;

			await expect(backend.truncate(file, 2)).rejects.toThrow("unary give-up");
			await expect(backend.remove([file])).rejects.toThrow("unary give-up");
			await expect(backend.move(file, file2, 3)).rejects.toThrow(
				"unary give-up",
			);
			await expect(
				backend.updateSessionTitle(file, titleUpdate, 4),
			).rejects.toThrow("unary give-up");

			// The latch short-circuits BEFORE the sink: no new emitDurable attempts.
			expect(sink.attempts).toBe(attemptsAtLatch);
		} finally {
			warnSpy.mockRestore();
			errSpy.mockRestore();
		}
	});

	// (1b) updateSessionTitle overwrites the fixed-width title slot IN PLACE:
	// the first 256 bytes decode to the new title, and every byte after 256
	// (header + entries) is byte-identical to the original. Non-vacuity: a wrong
	// write offset (e.g. writing past the slot) reds the "bytes after 256"
	// assert; a no-op write reds the decoded-title assert.
	test("updateSessionTitle round-trips the slot in place, leaving bytes after 256 untouched", async () => {
		const dir = scratch();
		const file = join(dir, "s.jsonl");
		const sink = recordingSink();
		const backend = new TranscriptTeeBackend(sink, dir);

		// Build a full body directly (node fs, not writeFull — no tee): a 256-byte
		// title slot carrying "old", then a header + one entry line.
		const original = Buffer.from(
			`${serializeTitleSlot({ title: "old", updatedAt: "2026-01-01T00:00:00.000Z" })}` +
				`{"type":"session","version":3,"id":"f1"}\n` +
				`{"type":"message","id":"e1","message":{"role":"user","content":"hi"}}\n`,
			"utf-8",
		);
		writeFileSync(file, original);

		await backend.updateSessionTitle(
			file,
			{ title: "new", updatedAt: "2026-01-01T00:00:00.000Z" },
			1,
		);

		const after = readFileSync(file);
		// (i) the first 256 bytes decode to a title slot carrying "new".
		const slot = parseTitleSlotFromContent(
			after.subarray(0, SESSION_TITLE_SLOT_BYTES).toString("utf-8"),
		);
		expect(slot?.title).toBe("new");
		// (ii) all bytes AFTER byte 256 are byte-identical to the original.
		expect(
			after
				.subarray(SESSION_TITLE_SLOT_BYTES)
				.equals(original.subarray(SESSION_TITLE_SLOT_BYTES)),
		).toBe(true);
		// The slot write emits NO durable frame (local-only).
		expect(sink.frames).toHaveLength(0);
	});

	// (1c) truncate/move/remove are local-only: they mutate the file on disk AND
	// emit NO durable frame. Non-vacuity: a method that teed a frame would bump
	// the durable-send count → red.
	test("truncate/move/remove mutate locally and emit no durable frame", async () => {
		const dir = scratch();
		const file = join(dir, "s.jsonl");
		const moved = join(dir, "moved.jsonl");
		const sink = recordingSink();
		const backend = new TranscriptTeeBackend(sink, dir);

		// Create the file directly (fs, NOT writeFull — that tees a checkpoint).
		writeFileSync(file, "body\n");
		const durableSendsBefore = sink.attempts;

		await backend.truncate(file, 1);
		expect(readFileSync(file, "utf-8")).toBe("");

		await backend.move(file, moved, 2);
		expect(existsSync(file)).toBe(false);
		expect(existsSync(moved)).toBe(true);

		await backend.remove([moved]);
		expect(existsSync(moved)).toBe(false);

		// None of the three teed a frame.
		expect(sink.attempts).toBe(durableSendsBefore);
		expect(sink.frames).toHaveLength(0);
	});
});

describe("createTeeSessionStorage", () => {
	// Mirrors RedisSessionStorage.create: initialize()d storage + backend,
	// ready to inject. init() must have created the session dir.
	test("returns an initialized storage over a tee backend", async () => {
		const dir = join(scratch(), "nested-sessions");
		const sink = recordingSink();

		const { storage, backend } = await createTeeSessionStorage(sink, dir);

		expect(backend).toBeInstanceOf(TranscriptTeeBackend);
		// init() ran (mkdir -p): a fresh scan through the wrapper does not throw.
		expect(storage.listFilesSync(dir, "*.jsonl")).toEqual([]);
	});
});
