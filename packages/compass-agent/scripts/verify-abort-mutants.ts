// Mutation verification for the Control source's reconnect-pump abort branches
// and the ack cursor's progress signal — the branches whose removal the suite
// must NOTICE. A reviewer proved the abort branches survive OUTRIGHT DELETION
// with the suite fully green, so the tests that cover them must be shown to
// DIE under each mutation, not merely to pass beside it.
//
// This exists because "the suite is green" and "the suite would notice" are
// different claims, and only the second one is worth anything. A test that
// passes against both the code and its deletion asserts nothing about it.
//
// Each mutant applies one textual edit to ITS target source, runs that source's
// suite, and requires a FAILURE. A mutant that survives (suite still green) is
// reported as SURVIVED and exits non-zero. Every file the table touches is
// snapshotted up front and restored from that snapshot, including on throw or
// signal — a crash here must never leave a mutated source on disk.
//
// WHAT A SURVIVAL DOES AND DOES NOT MEAN. These branches partly mask each
// other, so SURVIVED is two different findings wearing one label:
//
//   - UNTESTED — a test could catch this and none does. A real gap; write one.
//   - UNTESTABLE at the public surface — no test could catch it without
//     reaching into internals, because another branch absorbs the difference.
//
// Deleting the catch-side abort guard is the worked example: the pump falls
// through to the backoff, which returns at once on an already-aborted signal,
// loops, and the TOP-OF-LOOP guard returns. buffer.fail() is a no-op after
// return() closed the buffer, so nothing differs at the public surface. That
// early return is load-bearing for this claim and is a real code path, not an
// assumption — see `sleepOrAbort`'s already-aborted fast path. (It is still
// killable — the mutant runs the no-progress bookkeeping first and consumes a
// slot of that budget, which IS observable — but that took a specific
// contract to reach, not a straightforward assertion.)
//
// So do not read a survival as "write a test here" without first checking
// which of the two it is. Manufacturing an assertion for an unkillable branch
// produces exactly the vacuous test this script exists to expose.
//
// SINGLE-WRITER. This mutates control-source.ts AND control/ack-cursor.ts IN
// PLACE. Running it while another agent holds either file exposes a
// mid-mutation read of a source that looks subtly wrong (`if (false) {` where a
// real check belongs). Do not run it concurrently with anyone editing the
// transport.
//
// Crash safety is VERIFIED, not asserted: the run was killed with SIGTERM at a
// moment the source was observed mutated on disk (a different blob than HEAD),
// and the handler restored it byte-identical. Killing it at an arbitrary moment
// proves nothing — the file is unmutated for most of a run, so the restore path
// never executes and the test cannot fail. Catch the mutation window first.
//
// SIGKILL and a hard crash cannot be trapped, so either can leave a mutated
// file on disk — `control-source.ts` (`if (false) {` where a real check
// belongs) or `control/ack-cursor.ts` (the novelty predicate reduced to
// `if (seq > this.#cursor) {`, i.e. the re-ack-counts-as-progress defect).
// Both are git-tracked, so recovery is always:
//   git checkout -- src/transport/
//
// MANUAL TOOL, NOT A CI GATE. Nothing runs this script automatically: the
// package's `test` task covers only the pure helpers pinned in
// verify-abort-mutants.test.ts beside it, so the mutants' actual kill/survive
// status is ungated. It stays manual for the SINGLE-WRITER reason above, not
// by oversight. Run it by hand after touching those branches or the counter:
//   cd oss/compass/packages/compass-agent && bun run scripts/verify-abort-mutants.ts

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
	/** Exact source text to replace — must match exactly once. */
	readonly find: string;
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
 * against the file verbatim and asserted to occur EXACTLY once, so a refactor
 * that moves or reshapes a branch fails loudly here instead of silently
 * verifying nothing — the same vacuity trap this script exists to catch.
 */
export const MUTANTS: readonly Mutant[] = [
	{
		name: "top-of-loop abort guard",
		find: "\t\t\tif (abort.signal.aborted) return;\n\t\t\t// Uptime is measured from STREAM ESTABLISHMENT",
		replace: "\t\t\t// Uptime is measured from STREAM ESTABLISHMENT",
		expect:
			"a return()'d source must not re-open the subscription on the next loop turn",
	},
	{
		name: "catch-side abort guard",
		find: "\t\t\t\tif (abort.signal.aborted) return;\n\t\t\t\t// No-progress bound",
		replace: "\t\t\t\t// No-progress bound",
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
		find: "\t\t\t\tawait sleepOrAbort(delay, abort.signal);",
		replace:
			"\t\t\t\tawait new Promise<void>((resolve) => setTimeout(resolve, delay));",
		expect:
			"an abort during the backoff must wake immediately, not sit out the full delay",
	},
	// The no-progress budget's SIGNAL, not a branch of the pump. It earns a slot
	// here because it is the pump's only termination for a slow flap: if a
	// re-ack can inflate the applied count, a Runner redelivering one op forever
	// resets the budget forever — the unbounded reconnect the budget replaced a
	// wall-clock window to fix. Mutating the novelty predicate is how that claim
	// stays proven rather than merely asserted in a comment.
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

/** Apply one mutation to `source`, asserting the target occurs exactly once. */
export function mutate(source: string, m: Mutant): string {
	const hits = source.split(m.find).length - 1;
	if (hits !== 1) {
		throw new Error(
			`[${m.name}] expected exactly 1 occurrence of the target, found ${hits}. ` +
				`The branch moved or was reshaped — this mutant is no longer verifying ` +
				`what it names. Update the find string.`,
		);
	}
	return source.replace(m.find, m.replace);
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
