// Manual single-writer mutation check for transport abort and ack-cursor guards.
// It edits source files in place; a hard crash can leave them mutated.

import { $ } from "bun";

// The pump's abort branches live in the Control source; the progress signal
// they are bounded against lives in the ack cursor beside it. A mutant names
// which pair it mutates, defaulting to the source + its socket-level suite.
const SRC = new URL("../src/transport/control-source.ts", import.meta.url)
	.pathname;
const ACK_CURSOR_SRC = new URL(
	"../src/transport/control/ack-cursor.ts",
	import.meta.url,
).pathname;
const TEST = "src/transport/control-source.test.ts";
const ACK_CURSOR_TEST = "src/transport/control/ack-cursor.test.ts";
const PKG = new URL("..", import.meta.url).pathname;

type Mutant = {
	/** Name of the branch whose coverage this proves. */
	readonly name: string;
	/** Exact source text to replace — must match exactly once, unless `occurrence` is set. */
	readonly find: string;
	/** For code that repeats: `find` must match exactly `of` times; replace the `nth` (1-based). */
	readonly occurrence?: { readonly of: number; readonly nth: number };
	/** Replacement standing in for the branch being removed/neutered. */
	readonly replace: string;
	/** What a correct suite must report when this branch is gone. */
	readonly expect: string;
	/** File to mutate; defaults to the Control source. */
	readonly src?: string;
	/** Suite to run against the mutation; defaults to the source's own. */
	readonly test?: string;
	/**
	 * Set when the branch is UNTESTABLE at the public surface, with the
	 * measurement and reasoning recorded at the callsite. A survivor marked
	 * here is the healthy state and does not fail the run; a marked mutant
	 * that starts getting KILLED fails just as loudly, because it means a new
	 * test now pins the branch and this entry is stale.
	 */
	readonly expectedSurvivor?: string;
};

/**
 * The branches whose coverage is proven here. `find` strings are matched
 * against the file verbatim and asserted to occur the expected number of times, so a refactor
 * that moves or reshapes a branch fails loudly here instead of silently
 * verifying nothing — the same vacuity trap this script exists to catch.
 */
export const MUTANTS: readonly Mutant[] = [
	{
		name: "top-of-loop abort guard",
		find: "\t\t\tif (abort.signal.aborted) return;\n",
		occurrence: { of: 2, nth: 1 },
		replace: "",
		expect:
			"a return()'d source must not re-open the subscription on the next loop turn",
		expectedSurvivor:
			"Became UNTESTABLE at the public surface with the T4 Effect migration " +
			"(design record §T4). The pump is now a forked fiber, and the iterator's " +
			"return() interrupts it (Fiber.interrupt) in addition to abort.abort(). " +
			"The only public path that fires the abort signal is return(), which " +
			"ALWAYS also interrupts the fiber — so once the abort wakes the backoff " +
			"wait, the fiber is interrupted at its next yield boundary and never " +
			"reaches transport.control() again, whether or not this guard is present. " +
			"The interrupt absorbs the difference this guard used to make, exactly as " +
			"the catch-side guard below is absorbed by THIS one. Measured: with the " +
			"guard removed, F4's `controlCalls() === DROPS_TO_DEEPEST_BACKOFF` (no 5th " +
			"open) stays green because the interrupted fiber cannot re-open. There is " +
			"no seam to abort-without-interrupt, so no black-box test can kill it. " +
			"The guard is kept as correct defense-in-depth (deleting it would be a " +
			"control-flow change with no behavioural payoff); this entry records that " +
			"the migration, not a lost test, is why it now survives.",
	},
	{
		name: "catch-side abort guard",
		find: "\t\t\tif (abort.signal.aborted) return;\n",
		occurrence: { of: 2, nth: 2 },
		replace: "",
		expect:
			"an abort landing mid-stream must end quietly, never reconnect or fail() the buffer",
		expectedSurvivor:
			"Masked by the top-of-loop guard: with this line gone an abort runs the " +
			"budget bookkeeping, falls into the backoff, loops, and the top-of-loop " +
			"guard returns. buffer.fail() is a no-op after return() closed the " +
			"buffer, so every consumer-visible outcome is identical. " +
			"THIS DEPENDS ON `sleepOrAbort`'s already-aborted fast path: without it, " +
			"`addEventListener` on an already-aborted signal never fires (WHATWG) and " +
			"the mutant sits out the FULL backoff holding a live listener — measured " +
			"2001ms on a 2000ms sleep, and observable at the public surface as " +
			"liveAbortListeners() === 1 via the test file's observingTransport seam. " +
			"Remove that fast path and this entry becomes a real gap, not a survivor. " +
			"The other divergence is internal: the mutant consumes a slot of reconnect " +
			"budget (noProgress 0 vs 1), but it is private to pump's " +
			"closure with no public projection, so pinning it needs an internals " +
			"assertion or a now()-sample count — the implementation coupling F2(a)/(b) " +
			"was rewritten to remove. Documented above F4 in the suite instead.",
	},
	{
		name: "abortable backoff sleep",
		find: "\t\t\tyield* Effect.promise(() => sleepOrAbort(delay, abort.signal));",
		replace: "\t\t\tyield* Effect.sleep(delay);",
		expect:
			"an abort during the backoff must wake immediately, not sit out the full delay",
	},
	// Abort must precede interrupt: the uninterruptible backoff wait needs abort to wake it.
	{
		name: "return() aborts before awaiting interrupt",
		find: "\t\t\t\t\tabort.abort();\n\t\t\t\t\tbuffer.close();",
		replace: "\t\t\t\t\tbuffer.close();",
		expect:
			"return()'s awaited teardown must not wedge on the uninterruptible backoff wait — abort unparks the fiber the interrupt then joins",
	},
	// Re-acks must not reset the no-progress budget; only a new application counts.
	{
		name: "progress novelty guard (re-ack is not progress)",
		src: ACK_CURSOR_SRC,
		test: ACK_CURSOR_TEST,
		find: "if (seq > this.#cursor && !this.#above.has(seq)) {",
		replace: "if (seq > this.#cursor) {",
		expect:
			"a redelivered op still sitting in applied_above must not count as a new application",
	},
	{
		name: "progress counted once per application",
		src: ACK_CURSOR_SRC,
		test: ACK_CURSOR_TEST,
		find:
			"while (this.#above.delete(this.#cursor + 1n)) this.#cursor += 1n;\n" +
			"\t\t\tthis.#applied += 1;",
		replace:
			"while (this.#above.delete(this.#cursor + 1n)) {\n" +
			"\t\t\t\tthis.#cursor += 1n;\n" +
			"\t\t\t\tthis.#applied += 1;\n" +
			"\t\t\t}\n" +
			"\t\t\tthis.#applied += 1;",
		expect:
			"filling a gap applies ONE op, however long the contiguous run it unblocks",
	},
];

/** Apply one mutation to `source`, asserting the target occurs the expected number of times. */
export function mutate(source: string, m: Mutant): string {
	const parts = source.split(m.find);
	const hits = parts.length - 1;
	const want = m.occurrence?.of ?? 1;
	if (hits !== want) {
		throw new Error(
			`[${m.name}] expected exactly ${want} occurrence${want === 1 ? "" : "s"} of the target, found ${hits}. ` +
				`The branch moved or was reshaped — this mutant is no longer verifying ` +
				`what it names. Update the find string or its occurrence.`,
		);
	}
	const nth = m.occurrence?.nth ?? 1;
	if (!Number.isInteger(nth) || nth < 1 || nth > want) {
		throw new Error(`[${m.name}] occurrence.nth ${nth} is outside 1..${want}.`);
	}
	return (
		parts.slice(0, nth).join(m.find) + m.replace + parts.slice(nth).join(m.find)
	);
}

/**
 * True when a `bun test` run reported at least one FAILING test.
 *
 * Parses the count out of bun's ` N fail` summary rather than matching the
 * word: the summary prints ` 0 fail` on a fully green run, so a bare
 * substring/word match reports failure for every run and can never tell a
 * green suite from a red one. A detector that returns the same answer in both
 * states is not a detector — precisely the vacuity this script hunts.
 *
 * Absent a summary line at all (the run crashed before reporting), treat it as
 * failure: no evidence of success is not evidence of success.
 */
export function suiteFailed(output: string): boolean {
	const m = output.match(/^\s*(\d+)\s+fail\b/m);
	if (m?.[1] === undefined) return true;
	return Number(m[1]) > 0;
}

/** One mutant's outcome, classified against what the table expected. */
export type Verdict =
	/** Killed, as required — a test noticed the branch vanish. */
	| { readonly kind: "killed"; readonly name: string }
	/** Survived where a test was required. A real coverage gap. */
	| { readonly kind: "gap"; readonly name: string; readonly expect: string }
	/** Survived, as documented — masked at the public surface. */
	| { readonly kind: "known-survivor"; readonly name: string }
	/**
	 * Killed despite being marked UNTESTABLE. Not a pass: something now pins
	 * the branch, so the recorded reasoning is stale and must be re-checked.
	 */
	| { readonly kind: "stale-entry"; readonly name: string };

/**
 * Classify a mutant run. Split from the runner so both directions are unit
 * testable without mutating a real source file.
 *
 * Both mismatches are failures. Treating only the survivor direction as news
 * would let an entry sit marked UNTESTABLE forever after a test starts
 * covering it, and the next reader would trust a stale claim — the same
 * one-way blindness as a detector that can only ever report MATCH.
 */
export function classify(m: Mutant, killed: boolean): Verdict {
	if (m.expectedSurvivor === undefined) {
		return killed
			? { kind: "killed", name: m.name }
			: { kind: "gap", name: m.name, expect: m.expect };
	}
	return killed
		? { kind: "stale-entry", name: m.name }
		: { kind: "known-survivor", name: m.name };
}

/** True when a set of verdicts should fail the run. */
export function isFailing(verdicts: readonly Verdict[]): boolean {
	return verdicts.some((v) => v.kind === "gap" || v.kind === "stale-entry");
}

async function runSuite(test: string): Promise<string> {
	// bun test exits non-zero on failure, which is the EXPECTED case here, so
	// the throw is swallowed and the decision is made from the output text.
	const r = await $`bun test ${test}`.cwd(PKG).nothrow().quiet();
	return r.stdout.toString() + r.stderr.toString();
}

async function main(): Promise<void> {
	// Snapshot every file any mutant touches, so restore is total regardless of
	// which one was in flight.
	const sources = [...new Set(MUTANTS.map((m) => m.src ?? SRC))];
	const originals = new Map<string, string>(
		await Promise.all(
			sources.map(
				async (f) => [f, await Bun.file(f).text()] as [string, string],
			),
		),
	);
	// Latched on the in-flight promise, not a boolean: a second signal (SIGTERM
	// then SIGINT, say) must AWAIT the first restore rather than short-circuit a
	// `restored = true` set before its writes land and exit out from under them.
	let restoring: Promise<void> | undefined;
	const restore = (): Promise<void> => {
		restoring ??= Promise.all(
			[...originals].map(([f, text]) => Bun.write(f, text)),
		).then(() => undefined);
		return restoring;
	};
	// A signal must not leave a mutated source behind.
	process.on("SIGINT", () => void restore().then(() => process.exit(130)));
	process.on("SIGTERM", () => void restore().then(() => process.exit(143)));

	const verdicts: Verdict[] = [];
	try {
		for (const test of [...new Set(MUTANTS.map((m) => m.test ?? TEST))]) {
			const baseline = await runSuite(test);
			if (suiteFailed(baseline)) {
				console.error(
					`BASELINE IS RED (${test}) — fix the suite before verifying ` +
						"mutants. A mutant cannot be shown to kill a test that is " +
						"already failing.",
				);
				console.error(baseline.split("\n").slice(-15).join("\n"));
				process.exit(1);
			}
		}
		console.log("baseline: green\n");

		for (const m of MUTANTS) {
			const src = m.src ?? SRC;
			const original = originals.get(src);
			if (original === undefined)
				throw new Error(`[${m.name}] no snapshot for ${src}`);
			await Bun.write(src, mutate(original, m));
			const out = await runSuite(m.test ?? TEST);
			await Bun.write(src, original);

			const v = classify(m, suiteFailed(out));
			verdicts.push(v);
			switch (v.kind) {
				case "killed":
					console.log(`KILLED    ${m.name}`);
					break;
				case "gap":
					console.log(`SURVIVED  ${m.name}`);
					console.log(`          uncovered: ${m.expect}`);
					break;
				case "known-survivor":
					console.log(`SURVIVED  ${m.name}  (expected — untestable)`);
					break;
				case "stale-entry":
					console.log(`KILLED    ${m.name}  (UNEXPECTED)`);
					break;
			}
		}
	} finally {
		await restore();
		for (const [f, text] of originals) {
			if ((await Bun.file(f).text()) !== text) {
				console.error(`FATAL: ${f} not restored cleanly — check git diff.`);
				process.exit(2);
			}
		}
	}

	const gaps = verdicts.filter((v) => v.kind === "gap");
	const stale = verdicts.filter((v) => v.kind === "stale-entry");

	if (gaps.length > 0) {
		console.error(
			`\n${gaps.length} mutant(s) SURVIVED unexpectedly. These branches have ` +
				`no test that notices their removal:\n  ${gaps
					.map((v) => v.name)
					.join("\n  ")}`,
		);
	}
	if (stale.length > 0) {
		console.error(
			`\n${stale.length} mutant(s) marked UNTESTABLE were KILLED:\n  ${stale
				.map((v) => v.name)
				.join("\n  ")}\n` +
				"A test now covers the branch, so the recorded reasoning is stale. " +
				"Drop expectedSurvivor from that entry.",
		);
	}
	if (isFailing(verdicts)) process.exit(1);

	const known = verdicts.filter((v) => v.kind === "known-survivor").length;
	console.log(
		`\nall ${MUTANTS.length} mutants accounted for` +
			(known > 0 ? ` (${known} documented untestable)` : ""),
	);
}

if (import.meta.main) await main();
