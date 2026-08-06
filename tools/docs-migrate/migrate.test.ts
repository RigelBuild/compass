import { describe, expect, test } from "bun:test";
import { mkdtemp, rm } from "node:fs/promises";
import {
	gate,
	isProtectedHeading,
	sanitize,
	sanitizeWithChanges,
	transformLine,
} from "./migrate";

describe("class 1: linear.app link strip", () => {
	test("inline form → bare SEA id", () => {
		const out = sanitize(
			"Tracked in [SEA-1234](https://linear.app/sealedsecurity/issue/SEA-1234/title).",
		);
		expect(out).not.toContain("linear.app");
		expect(out).not.toContain("[SEA-1234]");
		expect(out).toContain("SEA-1234");
		expect(out).toBe("Tracked in SEA-1234.");
	});

	test("reference-definition form: usage kept as bare id, def line dropped", () => {
		const md = [
			"Tracked in [SEA-1234].",
			"",
			"[SEA-1234]: https://linear.app/sealedsecurity/issue/SEA-1234",
		].join("\n");
		const out = sanitize(md);
		expect(out).not.toContain("linear.app");
		expect(out).not.toContain("[SEA-1234]");
		expect(out).toContain("SEA-1234");
		expect(out).toBe(["Tracked in SEA-1234.", ""].join("\n"));
	});

	test("broad label reference-definition form is dropped", () => {
		const md = "[some label]: https://linear.app/x";
		expect(sanitize(md)).toBe("");
	});
});

describe("class 2: oss/compass path-prefix strip", () => {
	test("code span", () => {
		expect(sanitize("`oss/compass/go/internal/runtime/image.go`")).toBe(
			"`go/internal/runtime/image.go`",
		);
	});

	test("inline link target", () => {
		expect(sanitize("[x](oss/compass/apps/ui/src/stub-data.ts)")).toBe(
			"[x](apps/ui/src/stub-data.ts)",
		);
	});

	test("prose path", () => {
		const out = sanitize("See oss/compass/proto/compass/v1/compass.proto.");
		expect(out).not.toContain("oss/compass");
		expect(out).toBe("See proto/compass/v1/compass.proto.");
	});
});

describe("class 3: seal-*.md de-link", () => {
	test("relative seal record → plain prose", () => {
		expect(sanitize("[the seal restructure record](seal-restructure.md)")).toBe(
			"the seal restructure record",
		);
	});

	test("seal record with anchor → plain prose", () => {
		expect(sanitize("[x](seal-config-path-collision.md#foo)")).toBe("x");
	});
});

describe("class 4: security sections NO-TOUCH", () => {
	const protectedBody = [
		"Path oss/compass/go/foo.go and [SEA-9999](https://linear.app/x/SEA-9999).",
	].join("\n");

	test("threat-model section preserved byte-for-byte, sibling section transformed", () => {
		const md = [
			"## Threat model",
			"",
			protectedBody,
			"",
			"## Other",
			"",
			protectedBody,
			"",
		].join("\n");
		const out = sanitize(md);

		const threatBlock = ["## Threat model", "", protectedBody, ""].join("\n");
		expect(out.startsWith(threatBlock)).toBe(true);
		// Protected region keeps forbidden forms verbatim.
		expect(out).toContain("oss/compass/go/foo.go");
		expect(out).toContain("linear.app");
		// The sibling ## Other section IS transformed.
		const otherPart = out.slice(out.indexOf("## Other"));
		expect(otherPart).not.toContain("oss/compass");
		expect(otherPart).not.toContain("linear.app");
		expect(otherPart).toContain("go/foo.go");
		expect(otherPart).toContain("SEA-9999");
	});

	test("deeper heading nested under protected section stays protected", () => {
		const md = [
			"## Security boundary",
			"",
			"### Sub",
			"",
			protectedBody,
			"",
			"## Other",
			"",
			protectedBody,
			"",
		].join("\n");
		const out = sanitize(md);
		const protectedPart = out.slice(0, out.indexOf("## Other"));
		expect(protectedPart).toContain("oss/compass/go/foo.go");
		expect(protectedPart).toContain("linear.app");
		expect(protectedPart).toContain("### Sub");
	});

	test("egress heading (case-insensitive) is protected", () => {
		const md = ["## EGRESS Policy", "", protectedBody, ""].join("\n");
		const out = sanitize(md);
		expect(out).toContain("oss/compass/go/foo.go");
		expect(out).toContain("linear.app");
	});
});

describe("isProtectedHeading", () => {
	test.each([
		"Threat model",
		"Threat-model",
		"threatmodel",
		"Security",
		"Security boundary",
		"Egress",
	])("protects %p", (heading) => {
		expect(isProtectedHeading(heading)).toBe(true);
	});

	test.each([
		"Overview",
		"Approach",
		"Plan",
		"Other",
		"Module boundary",
		"Insecurity of the design",
	])("does not protect %p", (heading) => {
		expect(isProtectedHeading(heading)).toBe(false);
	});
});

describe("MEDIUM: isProtectedHeading does not over-match", () => {
	test("a '## Module boundary' section citing oss/compass is SANITIZED", () => {
		const md = [
			"## Module boundary",
			"",
			"Vendored at oss/compass/go/x.go.",
			"",
		].join("\n");
		const out = sanitize(md);
		expect(out).not.toContain("oss/compass");
		expect(out).toContain("go/x.go");
		expect(gate(out).ok).toBe(true);
	});

	test("a real '## Threat model' section citing a seal link stays verbatim + gate-exempt", () => {
		const md = [
			"## Threat model",
			"",
			"See [rec](seal-restructure.md) for the boundary analysis.",
			"",
		].join("\n");
		const out = sanitize(md);
		expect(out).toBe(md);
		expect(out).toContain("seal-restructure.md");
		expect(gate(out).ok).toBe(true);
	});
});

describe("gate", () => {
	test("clean output passes with empty residue", () => {
		expect(gate("just clean prose SEA-1234 and go/foo.go")).toEqual({
			ok: true,
			residue: [],
		});
	});

	test("residue lists both offenders", () => {
		// The linear offender is now the tracker-URL form, matching what the
		// transforms strip — a bare `linear.app` hostname is not residue (below).
		const g = gate("has https://linear.app/x/board and oss/compass");
		expect(g.ok).toBe(false);
		expect(g.residue).toContain("linear.app");
		expect(g.residue).toContain("oss/compass");
	});

	test("a bare linear.app hostname literal is NOT residue (only tracker URLs are)", () => {
		// Regression fixture drawn from compass-issue-model/design.md: a proto
		// field comment documents the Linear-as-forge SaaS host as the bare
		// string "linear.app". It is not a private tracker link, the transforms
		// (line-local, fence-skipping) correctly leave it, and the gate must not
		// red on it — else a legitimate corpus record can never pass migration.
		const forgeRefComment = [
			"```proto",
			"message ForgeRef {",
			'  string host = 2;  // "github.com" or, for a SaaS-only tracker-as-forge',
			'                    // (Linear) the constant service host, "linear.app"',
			"}",
			"```",
		].join("\n");
		expect(gate(forgeRefComment).ok).toBe(true);
		expect(gate(forgeRefComment).residue).not.toContain("linear.app");
	});

	test("a real tracker URL still reds even next to a bare hostname literal", () => {
		// The other direction: proves the relaxation did not blind the gate to a
		// genuine surviving private-tracker link.
		const g = gate(
			'host is "linear.app" but [SEA-9](https://linear.app/sealedsecurity/issue/SEA-9) leaked',
		);
		expect(g.ok).toBe(false);
		expect(g.residue).toContain("linear.app");
	});
});

describe("negative / untouched", () => {
	test("bare SEA id untouched", () => {
		expect(sanitize("Tracked in SEA-1234 as usual.")).toBe(
			"Tracked in SEA-1234 as usual.",
		);
	});

	test("non-linear link untouched", () => {
		expect(sanitize("[x](https://example.com)")).toBe(
			"[x](https://example.com)",
		);
	});

	test("non-seal relative link untouched", () => {
		expect(sanitize("[y](other.md)")).toBe("[y](other.md)");
	});
});

describe("idempotency", () => {
	test("sanitize is idempotent on a mixed fixture", () => {
		const md = [
			"See [SEA-1](https://linear.app/x/SEA-1) and `oss/compass/go/a.go`.",
			"",
			"Read [the record](seal-restructure.md) and [SEA-2].",
			"",
			"[SEA-2]: https://linear.app/x/SEA-2",
			"",
		].join("\n");
		const once = sanitize(md);
		expect(sanitize(once)).toBe(once);
	});
});

describe("transformLine", () => {
	test("drops a linear reference-definition line (returns null)", () => {
		expect(transformLine("[SEA-1234]: https://linear.app/x/SEA-1234")).toBe(
			null,
		);
	});

	test("returns transformed line otherwise", () => {
		expect(transformLine("path oss/compass/go/a.go")).toBe("path go/a.go");
	});

	test("returns line unchanged when nothing matches", () => {
		expect(transformLine("nothing here")).toBe("nothing here");
	});
});

describe("LOW: bare oss/compass rewrite is boundary-anchored", () => {
	test("a suffixed token like oss/compass-tools is NOT clipped", () => {
		expect(transformLine("vendored under oss/compass-tools/x")).toBe(
			"vendored under oss/compass-tools/x",
		);
	});

	test("a preserved suffixed token does not wedge the gate (sanitize→gate)", () => {
		// The sanitizer intentionally keeps `oss/compass-tools` verbatim; the gate
		// MUST agree it is not residue, or the two-phase barrier deadlocks on a
		// literal that is by-design un-rewritable and non-exempt.
		const out = sanitize("vendored under oss/compass-tools/x");
		expect(out).toBe("vendored under oss/compass-tools/x");
		expect(gate(out).ok).toBe(true);
	});

	test("the slash form still strips fully (line-117 order preserved)", () => {
		expect(transformLine("see oss/compass/go/x.go")).toBe("see go/x.go");
	});

	test("a bare oss/compass at a word boundary still rewrites to compass", () => {
		expect(transformLine("root oss/compass here")).toBe("root compass here");
	});
});

describe("sanitizeWithChanges: per-file diff", () => {
	test("returns a non-empty change list on a mixed fixture", () => {
		const md = [
			"See [SEA-1](https://linear.app/x/SEA-1).",
			"`oss/compass/go/a.go`",
			"[rec](seal-restructure.md)",
			"[SEA-2]: https://linear.app/x/SEA-2",
		].join("\n");
		const { output, changes } = sanitizeWithChanges(md);
		expect(changes.length).toBeGreaterThan(0);
		// The dropped ref-def line records after === null.
		const dropped = changes.find((c) => c.after === null);
		expect(dropped).toBeDefined();
		expect(dropped?.before).toContain("linear.app");
		expect(gate(output).ok).toBe(true);
	});

	test("clean input yields no changes", () => {
		const md = "clean SEA-1234\n\ngo/foo.go\n";
		const { output, changes } = sanitizeWithChanges(md);
		expect(changes).toEqual([]);
		expect(output).toBe(md);
	});
});

describe("acceptance: grep-gate over a mixed fixture", () => {
	test("sanitized output has zero linear.app and zero oss/compass", () => {
		// Per the contract, this fixture carries the forbidden forms ONLY outside
		// security sections, so a passing gate proves every non-protected form was
		// stripped (not merely preserved inside a protected region).
		const md = [
			"# Record",
			"",
			"Tracked in [SEA-1234](https://linear.app/sealedsecurity/issue/SEA-1234).",
			"",
			"Source: `oss/compass/go/internal/runtime/image.go`.",
			"",
			"See [the seal restructure record](seal-restructure.md).",
			"",
			"Also [SEA-5678].",
			"",
			"[SEA-5678]: https://linear.app/sealedsecurity/issue/SEA-5678",
			"",
		].join("\n");
		const out = sanitize(md);
		expect(gate(out).ok).toBe(true);
		expect(out).not.toContain("linear.app");
		expect(out).not.toContain("oss/compass");
	});
});

describe("HIGH: fenced-code-block blindness", () => {
	test("a `#`/`##` comment inside a fence does NOT close a protected section", () => {
		// The fence sits inside `## Threat model`. Its `#`/`##` comment lines carry
		// NO security keyword, so a heading-blind walk would parse `# step one` as
		// a level-1 heading and CLOSE the section — then the security prose AFTER
		// the fence gets silently sanitized. Fence-aware, it stays byte-verbatim.
		const md = [
			"## Threat model",
			"",
			"```bash",
			"# step one",
			"## build",
			"grep oss/compass/go/foo.go",
			"```",
			"",
			"Attacker path oss/compass/go/bar.go and [SEA-9](https://linear.app/x/SEA-9)",
			"and [rec](seal-restructure.md).",
			"",
		].join("\n");
		const out = sanitize(md);
		// The whole protected block (fence + post-fence prose) is preserved verbatim.
		expect(out).toContain("grep oss/compass/go/foo.go");
		expect(out).toContain("oss/compass/go/bar.go");
		expect(out).toContain("linear.app");
		expect(out).toContain("[SEA-9]");
		expect(out).toContain("[rec](seal-restructure.md)");
		expect(out).toBe(md);
	});

	test("a `# security` comment inside a fence in a NON-protected section does not open a false protected region", () => {
		const md = [
			"## Setup",
			"",
			"```bash",
			"# security hardening",
			"echo hi",
			"```",
			"",
			"Path oss/compass/go/foo.go and [SEA-9](https://linear.app/x/SEA-9).",
			"",
		].join("\n");
		const out = sanitize(md);
		// The fence body is verbatim...
		expect(out).toContain("# security hardening");
		// ...but the prose after the fence is NOT falsely protected: it is sanitized.
		expect(out).not.toContain("oss/compass");
		expect(out).not.toContain("linear.app");
		expect(out).toContain("go/foo.go");
		expect(out).toContain("SEA-9");
	});

	test("non-protected fenced code is scrubbed; fence delimiters survive", () => {
		// Fenced code is IN transform scope (class 3: code spans included). The
		// inner ``` (shorter than the ```` opener) still does NOT close the fence,
		// but membership no longer exempts a forbidden literal from the rewrite.
		const md = [
			"````markdown",
			"```",
			"see oss/compass/go/inner.go",
			"~~~",
			"````",
			"",
			"then oss/compass/go/outer.go",
			"",
		].join("\n");
		const out = sanitize(md);
		// Forbidden literal is scrubbed inside the fence too.
		expect(out).not.toContain("oss/compass/go/inner.go");
		expect(out).toContain("see go/inner.go");
		// Fence delimiter lines survive intact.
		expect(out).toContain("````markdown");
		expect(out).toContain("~~~");
		// After the fence: sanitized as before.
		expect(out).toContain("then go/outer.go");
		expect(gate(out).ok).toBe(true);
	});
});

describe("HIGH: non-protected fence/heading are IN transform scope", () => {
	test("a ```bash fence in a NON-protected ## Example section is scrubbed and gate ok", () => {
		const md = [
			"## Example",
			"",
			"```bash",
			"grep oss/compass/go/foo.go",
			"```",
			"",
		].join("\n");
		const out = sanitize(md);
		expect(out).not.toContain("oss/compass");
		expect(out).toContain("grep go/foo.go");
		expect(gate(out).ok).toBe(true);
	});

	test("a non-protected fenced linear.app link is scrubbed and gate ok", () => {
		const md = [
			"## Example",
			"",
			"```md",
			"See [SEA-9](https://linear.app/x/SEA-9).",
			"```",
			"",
		].join("\n");
		const out = sanitize(md);
		expect(out).not.toContain("linear.app");
		expect(out).toContain("SEA-9");
		expect(gate(out).ok).toBe(true);
	});

	test("a forbidden literal in a NON-protected heading is rewritten", () => {
		const md = ["## The oss/compass/go layout", ""].join("\n");
		const out = sanitize(md);
		expect(out).toContain("## The go layout");
		expect(out).not.toContain("oss/compass");
		expect(gate(out).ok).toBe(true);
	});
});

describe("MEDIUM: gate is protected-section-aware", () => {
	test("a linear.app link legitimately inside a protected section passes the gate", () => {
		const md = [
			"## Threat model",
			"",
			"Analysis references [SEA-9](https://linear.app/x/SEA-9) verbatim.",
			"",
		].join("\n");
		const out = sanitize(md);
		// Class 4: the section is untouched...
		expect(out).toBe(md);
		expect(out).toContain("linear.app");
		// ...and the gate does not wedge on the verbatim-kept literal.
		expect(gate(out).ok).toBe(true);
	});

	test("an oss/compass path inside a protected section passes the gate", () => {
		const md = [
			"## Security",
			"",
			"Vendored at oss/compass/go/foo.go.",
			"",
		].join("\n");
		const out = sanitize(md);
		expect(out).toBe(md);
		expect(gate(out).ok).toBe(true);
	});

	test("the SAME literal outside a protected section still fails the gate", () => {
		expect(gate("Vendored at oss/compass/go/foo.go.").ok).toBe(false);
	});
});

describe("MEDIUM: class 3 seal-*.md — gate backstop + SEAL_LINK forms", () => {
	test("title-attr seal link is de-linked to prose", () => {
		expect(
			sanitize('See [the record](seal-restructure.md "private note").'),
		).toBe("See the record.");
	});

	test("title-attr seal link with anchor is de-linked to prose", () => {
		expect(sanitize('[x](seal-config.md#foo "note")')).toBe("x");
	});

	test("reference-style seal link + ref-def: def dropped, usage handled", () => {
		const md = [
			"See [the record][rec] for detail.",
			"",
			"[rec]: seal-restructure.md",
			"",
		].join("\n");
		const out = sanitize(md);
		// The private path must not survive anywhere in the output.
		expect(out).not.toContain("seal-restructure.md");
		expect(gate(out).ok).toBe(true);
	});

	test("seal ref-def line is dropped", () => {
		expect(transformLine("[rec]: seal-restructure.md")).toBe(null);
		expect(transformLine("[rec]: ./seal-config.md#foo")).toBe(null);
	});

	test("gate flags a surviving inline seal link (fail-closed backstop)", () => {
		expect(gate("See [x](seal-foo.md) here.").ok).toBe(false);
		expect(gate("See [x](seal-foo.md) here.").residue).toContain("seal-*.md");
	});

	test("gate flags a surviving seal ref-def line", () => {
		expect(gate("[rec]: seal-restructure.md").ok).toBe(false);
	});

	test("gate does not flag a benign 'seal-' mention that is not a link", () => {
		expect(gate("the seal-the-product restructure, private").ok).toBe(true);
	});
});

describe("MEDIUM: bare oss/compass (no slash) rewrite/gate agree", () => {
	test("bare oss/compass in prose is rewritten to compass, gate passes", () => {
		const out = sanitize("This is vendored under oss/compass, historically.");
		expect(out).toBe("This is vendored under compass, historically.");
		expect(gate(out).ok).toBe(true);
	});

	test("the slash form still strips fully (not to compass/…)", () => {
		expect(sanitize("`oss/compass/go/a.go`")).toBe("`go/a.go`");
	});
});

describe("MEDIUM: --write two-phase write barrier", () => {
	test("a file whose sanitized output has residue is NOT written; exit non-zero", async () => {
		const dir = await mkdtemp("/tmp/docs-migrate-test-");
		const clean = `${dir}/clean.md`;
		const dirty = `${dir}/dirty.md`;
		// `dirty` carries a NON-SEA linear.app link: the rewrite regexes only touch
		// `[SEA-<n>]` ids, so this link survives sanitization and the gate catches
		// it as residue. `clean` sanitizes to zero residue.
		const cleanIn =
			"Path oss/compass/go/a.go and [SEA-1](https://linear.app/x/SEA-1).\n";
		const dirtyIn = "See [tracker](https://linear.app/x/board) here.\n";
		await Bun.write(clean, cleanIn);
		await Bun.write(dirty, dirtyIn);

		const proc = Bun.spawn(
			["bun", `${import.meta.dir}/migrate.ts`, "--write", clean, dirty],
			{ stdout: "pipe", stderr: "pipe" },
		);
		const code = await proc.exited;
		expect(code).not.toBe(0);

		// Neither file is written when the batch fails the gate (all-or-nothing).
		expect(await Bun.file(clean).text()).toBe(cleanIn);
		expect(await Bun.file(dirty).text()).toBe(dirtyIn);

		await rm(dir, { recursive: true, force: true });
	});

	test("a clean --write batch is written", async () => {
		const dir = await mkdtemp("/tmp/docs-migrate-test-");
		const file = `${dir}/rec.md`;
		await Bun.write(file, "Path oss/compass/go/a.go.\n");
		const proc = Bun.spawn(
			["bun", `${import.meta.dir}/migrate.ts`, "--write", file],
			{
				stdout: "pipe",
				stderr: "pipe",
			},
		);
		const code = await proc.exited;
		expect(code).toBe(0);
		expect(await Bun.file(file).text()).toBe("Path go/a.go.\n");
		await rm(dir, { recursive: true, force: true });
	});
});

describe("LOW: LINEAR_SHORTCUT does not corrupt non-shortcut SEA usages", () => {
	test("collapsed/full reference usage [SEA-1][ref] is left for its ref-def handling", () => {
		// `[SEA-1][ref]` is a reference link, not a shortcut. It must NOT be
		// rewritten to a bare `SEA-1[ref]`.
		const md = [
			"Tracked in [SEA-1][ref].",
			"",
			"[ref]: https://linear.app/x/SEA-1",
			"",
		].join("\n");
		const out = sanitize(md);
		expect(out).not.toContain("[SEA-1]SEA");
		expect(out).not.toContain("SEA-1[ref]");
		// The linear ref-def is dropped; no linear.app residue.
		expect(gate(out).ok).toBe(true);
	});

	test("a line-start [SEA-1]: def is not shortcut-rewritten mid-line", () => {
		// Non-linear ref-def for a SEA id must not be corrupted into `SEA-1: …`.
		expect(transformLine("[SEA-1]: ./other.md")).toBe("[SEA-1]: ./other.md");
	});

	test("a genuine shortcut [SEA-1] in prose is still stripped to a bare id", () => {
		expect(transformLine("Tracked in [SEA-1] as usual.")).toBe(
			"Tracked in SEA-1 as usual.",
		);
	});
});
