// AsyncBuffer allows one parked consumer; a second pull must reject, not strand the first promise.

import { expect, test } from "bun:test";
import { AsyncBuffer, type Queued } from "./buffer";

function queued(seq: bigint, input: string): Queued {
	return { op: { kind: "prompt", input }, seq };
}

type Settled<T> =
	| { state: "pending" }
	| { state: "fulfilled"; value: T }
	| { state: "rejected"; reason: unknown };

// Observe settlement without awaiting, so a wedged promise can be asserted instead of hanging the test.
async function settled<T>(p: Promise<T>): Promise<Settled<T>> {
	let out: Settled<T> = { state: "pending" };
	void p.then(
		(value) => {
			out = { state: "fulfilled", value };
		},
		(reason: unknown) => {
			out = { state: "rejected", reason };
		},
	);
	for (let i = 0; i < 8; i++) await Promise.resolve();
	return out;
}

// The rejection reason, or a description of what happened INSTEAD — so a pull
// that fulfilled or (the regression) parked forever reds with a readable value
// rather than an undefined dereference.
function reasonOf(s: Settled<unknown>): unknown {
	return s.state === "rejected" ? s.reason : `did not reject (${s.state})`;
}

test("a second concurrent pull() is refused BY NAME — it rejects as single-consumer instead of silently displacing the first", async () => {
	// Non-vacuity: restore the plain `this.#waiting = { resolve, reject }` body
	// (drop the guard) → the second pull parks instead of rejecting, `settled`
	// reports "pending", and the message assertion reds.
	const buf = new AsyncBuffer();
	const first = buf.pull();
	const second = buf.pull();
	expect(String(reasonOf(await settled(second)))).toContain("single-consumer");
	// The refusal is the SECOND caller's alone: the first is still a live,
	// unsettled pull, not collateral damage.
	expect(await settled(first)).toEqual({ state: "pending" });
});

test("the refused second pull() does NOT abandon the first — a later push() still resolves the FIRST pull with that value", async () => {
	// The first pull must remain reachable; replacing its waiting record would leave the control loop wedged.
	const buf = new AsyncBuffer();
	const first = buf.pull();
	const second = buf.pull();
	expect(String(reasonOf(await settled(second)))).toContain("single-consumer");
	buf.push(queued(7n, "hello"));
	expect(await settled(first)).toEqual({
		state: "fulfilled",
		value: { value: queued(7n, "hello"), done: false },
	});
});

test("the real single-consumer path is untouched: sequential pull()/push() resolve in order, twice in a row", async () => {
	// The negative control — proof the guard does not over-fire. `#waiting` is
	// cleared by the handoff, so the NEXT pull is a first pull, not a second one.
	// Non-vacuity: make the guard unconditional (`if (true) reject(...)`) or have
	// push() leave `#waiting` set → the second sequential pull rejects → red.
	const buf = new AsyncBuffer();
	const one = buf.pull();
	buf.push(queued(1n, "one"));
	expect(await settled(one)).toEqual({
		state: "fulfilled",
		value: { value: queued(1n, "one"), done: false },
	});
	const two = buf.pull();
	buf.push(queued(2n, "two"));
	expect(await settled(two)).toEqual({
		state: "fulfilled",
		value: { value: queued(2n, "two"), done: false },
	});
});

test("close() after a refused concurrent pull() still ends the FIRST parked pull with done: true", async () => {
	// close() must settle the original waiting caller, not a displaced second pull.
	const buf = new AsyncBuffer();
	const first = buf.pull();
	expect(String(reasonOf(await settled(buf.pull())))).toContain(
		"single-consumer",
	);
	buf.close();
	expect(await settled(first)).toEqual({
		state: "fulfilled",
		value: { value: undefined, done: true },
	});
});

test("fail(err) after a refused concurrent pull() still rejects the FIRST parked pull with that same error", async () => {
	// The drop path (reconnect budget spent) must surface the REAL cause to the
	// parked consumer, by identity — not the guard's error, and not a hang.
	// Non-vacuity: drop the guard → fail() rejects the second, displaced record
	// and `first` stays "pending", so the identity assertion reds.
	const boom = new Error("reconnect budget spent");
	const buf = new AsyncBuffer();
	const first = buf.pull();
	expect(String(reasonOf(await settled(buf.pull())))).toContain(
		"single-consumer",
	);
	buf.fail(boom);
	expect(reasonOf(await settled(first))).toBe(boom);
});
