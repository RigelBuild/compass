// Env-secrecy gate: the app's env files must stay uncommittable, because Vite bakes every
// VITE_* value into the production bundle, so a secret in a tracked `.env` ships to every
// browser (review missed it once, RIG-1539). Two layers: check-ignore (do the rules cover the
// filenames) + ls-files (what is ACTUALLY tracked, closing the force-add / pre-rule hole).

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
