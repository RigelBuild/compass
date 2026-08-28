// Env-secrecy gate: the app's env files must stay uncommittable, because Vite
// bakes every VITE_* value into the production bundle as a string literal — so a
// secret in a tracked `.env` ships in `dist/` to every browser that loads the
// app. No moon task consumes these paths (they are inputs to nothing that
// lints, tests, or scans), so review is the ONLY thing between a committed token
// and an exfiltrated one — and review is exactly what missed it the first time
// (RIG-1539). This gate gives the `.gitignore` rules teeth so they cannot
// silently re-widen, and — the property that actually matters — pins the real
// tracked set so it can never grow past the one dev-defaults file.
//
// Two layers, because `git check-ignore` and `git ls-files` answer different
// questions and each alone has a hole:
//   1. check-ignore — do the IGNORE RULES cover the dangerous filenames? Catches
//      a `.gitignore` edit that un-ignores `.env` / `.env.production`. But it
//      reports rule MATCHES, not tracking: a file force-added (`git add -f`) or
//      committed before the rule existed stays tracked and ships, while
//      check-ignore still calls it "ignored".
//   2. ls-files — what is ACTUALLY tracked, regardless of the rules? This is the
//      only ground truth about what leaves in the bundle, and it closes the
//      force-add / pre-rule blind spot layer 1 cannot see.
//
// Scope boundary: this gate covers WHICH files are tracked, not their CONTENTS.
// A VITE_* secret pasted into the one allowed tracked file (`.env.development`)
// still ships and passes both layers; that file's secret-freedom rests on
// convention and review, not this gate. Closing that hole needs a content-scan
// layer, tracked as a follow-up.

import { describe, expect, test } from "bun:test";
import { resolve } from "node:path";
import { $ } from "bun";

// The git-ignore rules under test live in `apps/ui/.gitignore`, and the tracked
// dev-defaults file is `apps/ui/.env.development`. Resolve from this test file
// (`apps/ui/src/`) so the check is invocation-cwd independent.
const UI_DIR = resolve(import.meta.dir, "..");

// The env filenames a developer is most likely to create for real credentials.
// Vite loads `.env` in EVERY mode (production included) and `.env.production` in
// prod, so a token in either ships in the built bundle; `.env.local` overrides
// per-machine and is the natural place to paste a bearer.
const MUST_BE_IGNORED = [".env", ".env.local", ".env.production"];

// The one exception: loopback dev defaults, no secret, tracked so it documents
// the required VITE_* keys by existing.
const DEV_DEFAULTS = ".env.development";

// `git check-ignore -q` exits 0 when the path matches an ignore rule, 1 when it
// does not. `.nothrow()` because exit 1 is a valid answer here, not a failure;
// `.quiet()` suppresses the shell echo.
async function isIgnored(file: string): Promise<boolean> {
	const { exitCode } = await $`git check-ignore -q ${file}`
		.cwd(UI_DIR)
		.nothrow()
		.quiet();
	return exitCode === 0;
}

describe("env-secrecy gate (no new committable .env; tracked env set pinned to dev defaults)", () => {
	test.each(MUST_BE_IGNORED)(
		"%s is covered by the .gitignore rules",
		async (file) => {
			expect(await isIgnored(file)).toBe(true);
		},
	);

	test(`${DEV_DEFAULTS} stays trackable (documents the required VITE_* keys)`, async () => {
		expect(await isIgnored(DEV_DEFAULTS)).toBe(false);
	});

	test("the only tracked apps/ui env file is the dev defaults", async () => {
		// Ground truth for what ships: what git actually tracks under apps/ui,
		// regardless of the ignore rules. `git ls-files` from apps/ui lists tracked
		// paths relative to that dir; filter to env-shaped basenames (`.env`,
		// `.env.<mode>`, and any nested one, but not a `.env.d.ts` type stub) and
		// assert the set is exactly the dev-defaults file. A force-added or pre-rule
		// secret lands here as a new entry and reddens this test even though layer 1
		// would still pass. (Its CONTENTS are out of scope — see the header.)
		const tracked = (await $`git ls-files`.cwd(UI_DIR).quiet().text())
			.split("\n")
			.filter((line) => line.length > 0);
		const envFiles = tracked
			.filter(
				(path) => /(^|\/)\.env(\.|$)/.test(path) && !path.endsWith(".d.ts"),
			)
			.sort();
		const unexpected = envFiles.filter((f) => f !== DEV_DEFAULTS);
		expect(
			envFiles,
			unexpected.length > 0
				? `Unexpected tracked env file(s) under apps/ui: [${unexpected.join(", ")}]. A VITE_* value in any tracked env file is baked into dist/ and shipped to every browser (RIG-1539). Untrack it (git rm --cached <file>); only ${DEV_DEFAULTS} (loopback dev defaults, no secret) may be tracked.`
				: "",
		).toEqual([DEV_DEFAULTS]);
	});
});
