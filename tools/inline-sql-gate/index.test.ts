// Unit tests for the inline-sql-gate's pure core + I/O wiring (index.ts).
//
// This gate is a CI oracle: it defines whether a pgx call site's SQL argument
// is banned inline SQL, so this suite defends the machine-readable contract —
// the across-newline tokenizer (the store's dominant shape puts the literal on
// the line AFTER the call), multi-line backtick literals, the structural
// exclusion of non-pgx Exec(ctx, id, spec) calls, and the fail-closed
// stale-allowlist ratchet.
//
// Conventions (mirroring tools/cx-token-gate/index.test.ts):
// - Literal paths, not values derived from the module constants.
// - `.snippet` is the trimmed source line, asserted by an identifying substring.

import { describe, expect, test } from "bun:test";
import {
	type Deps,
	isExcludedPath,
	runOnce,
	scanFiles,
	scanText,
	staleAllowlistEntries,
} from "./index.ts";

const STORE = "go/internal/store/example.go";

// ---------------------------------------------------------------------------
// scanText — the across-newline tokenizer.
// ---------------------------------------------------------------------------

describe("scanText", () => {
	test("SQL literal on the SAME line as the call is flagged", () => {
		const src = `func f() {
	_, err := s.pool.Exec(ctx, "DELETE FROM secrets WHERE name = $1", name)
}`;
		const fs = scanText(STORE, src);
		expect(fs.length).toBe(1);
		expect(fs[0]?.snippet).toContain("DELETE FROM secrets");
	});

	test("SQL literal on the line AFTER the call is flagged (dominant store shape)", () => {
		const src = `func f() {
	if _, err := tx.Exec(ctx,
		"INSERT INTO accounts (id, handle) VALUES ($1, $2)",
		id, handle,
	); err != nil {
		return err
	}
}`;
		const fs = scanText(STORE, src);
		expect(fs.length).toBe(1);
		// The finding points at the line the literal starts on, not the call line.
		expect(fs[0]?.snippet).toContain("INSERT INTO accounts");
	});

	test("a multi-line backtick literal is flagged", () => {
		const src = `func f() {
	rows, err := s.pool.Query(ctx,
		\`SELECT id, handle
		   FROM accounts
		  WHERE tenant_id = $1
		  ORDER BY id\`,
		tenant,
	)
}`;
		const fs = scanText(STORE, src);
		expect(fs.length).toBe(1);
		expect(fs[0]?.snippet).toContain("SELECT id, handle");
	});

	test("QueryRow with a backtick literal is flagged", () => {
		const src = `func f() {
	if err := q.QueryRow(ctx,
		\`SELECT EXISTS (SELECT 1 FROM channel_members WHERE channel_id = $1)\`,
		id,
	).Scan(&ok); err != nil {}
}`;
		const fs = scanText(STORE, src);
		expect(fs.length).toBe(1);
		expect(fs[0]?.snippet).toContain("SELECT EXISTS");
	});

	test("a +-concatenated literal spanning lines is flagged", () => {
		const src = `func f() {
	_, err := tx.Exec(ctx,
		"INSERT INTO channel_members (channel_id, account_id) VALUES ($1, $2) " +
			"ON CONFLICT (channel_id, account_id) DO NOTHING",
		ch, acc,
	)
}`;
		const fs = scanText(STORE, src);
		expect(fs.length).toBe(1);
		expect(fs[0]?.snippet).toContain("INSERT INTO channel_members");
	});

	test("every SQL verb keyword is recognised", () => {
		for (const verb of [
			"SELECT",
			"INSERT",
			"UPDATE",
			"DELETE",
			"WITH",
			"CREATE",
			"DROP",
		]) {
			const src = `x.Exec(ctx, "${verb} something here")`;
			expect(scanText(STORE, src).length).toBe(1);
		}
	});
});

// ---------------------------------------------------------------------------
// The structural exclusion of non-pgx Exec calls — the false-positive guard.
// ---------------------------------------------------------------------------

describe("non-SQL Exec is NOT flagged", () => {
	test("runtime/compute Exec(ctx, id, spec) with identifier args is not flagged", () => {
		const src = `func f() {
	out, err := r.runtime.Exec(ctx, handle.id, spec)
	if err != nil {
		return err
	}
}`;
		expect(scanText("go/internal/runtime/agent.go", src)).toEqual([]);
	});

	test("Exec whose string arg carries no SQL keyword is not flagged", () => {
		const src = `x.Exec(ctx, id, NewExecSpec("sh", "-c", script))`;
		expect(scanText("go/internal/runtime/agent.go", src)).toEqual([]);
	});

	test("a .Exec( appearing inside a string literal is not a call site", () => {
		const src = `msg := "call tx.Exec(ctx, \\"SELECT 1\\") to run it"`;
		expect(scanText(STORE, src)).toEqual([]);
	});

	test("a .Exec( inside a comment is not a call site", () => {
		const src = `// historically this used tx.Exec(ctx, "SELECT 1")
func f() {}`;
		expect(scanText(STORE, src)).toEqual([]);
	});
});

// ---------------------------------------------------------------------------
// isExcludedPath — generated package + test files.
// ---------------------------------------------------------------------------

describe("isExcludedPath", () => {
	test("the generated db/ package is excluded", () => {
		expect(isExcludedPath("go/internal/store/db/accounts.sql.go")).toBe(true);
	});
	test("test files are excluded", () => {
		expect(isExcludedPath("go/internal/store/accounts_test.go")).toBe(true);
		expect(isExcludedPath("go/internal/store/accounts_pgtest_test.go")).toBe(
			true,
		);
	});
	test("an ordinary store file is in scope", () => {
		expect(isExcludedPath("go/internal/store/accounts.go")).toBe(false);
	});
});

// ---------------------------------------------------------------------------
// scanFiles — aggregation excludes generated + test files, sorts.
// ---------------------------------------------------------------------------

describe("scanFiles", () => {
	test("generated + test files are never subjects; findings are sorted", () => {
		const fs = scanFiles([
			{
				path: "go/internal/store/db/x.sql.go",
				text: `x.Exec(ctx, "SELECT 1")`,
			},
			{ path: "go/internal/store/a_test.go", text: `x.Exec(ctx, "SELECT 1")` },
			{ path: "go/internal/store/zeta.go", text: `x.Exec(ctx, "SELECT 1")` },
			{
				path: "go/internal/store/alpha.go",
				text: `x.Exec(ctx, "INSERT INTO t VALUES (1)")`,
			},
		]);
		expect(fs.map((f) => f.file)).toEqual([
			"go/internal/store/alpha.go",
			"go/internal/store/zeta.go",
		]);
	});
});

// ---------------------------------------------------------------------------
// staleAllowlistEntries — the fail-closed ratchet.
// ---------------------------------------------------------------------------

describe("staleAllowlistEntries", () => {
	test("an allowlist entry with no matching finding is stale", () => {
		const raw = scanFiles([
			{
				path: "go/internal/store/accounts.go",
				text: `x.Exec(ctx, "SELECT 1")`,
			},
		]);
		const stale = staleAllowlistEntries(raw, [
			"go/internal/store/accounts.go", // has a finding — not stale
			"go/internal/store/migrated.go", // no finding — stale
		]);
		expect(stale).toEqual(["go/internal/store/migrated.go"]);
	});

	test("a fully-matched allowlist is not stale", () => {
		const raw = scanFiles([
			{
				path: "go/internal/store/accounts.go",
				text: `x.Exec(ctx, "SELECT 1")`,
			},
		]);
		expect(
			staleAllowlistEntries(raw, ["go/internal/store/accounts.go"]),
		).toEqual([]);
	});
});

// ---------------------------------------------------------------------------
// runOnce — exit-code posture.
// ---------------------------------------------------------------------------

const deps = (files: Record<string, string>, allowlist: string[]): Deps => ({
	root: "/fake",
	allowlist,
	listGoFiles: () => Object.keys(files),
	readText: (_r, rel) => files[rel] ?? null,
	log: () => {},
	err: () => {},
});

describe("runOnce", () => {
	test("a clean tree (no inline SQL, empty allowlist) exits 0", () => {
		const code = runOnce(
			deps(
				{ "go/internal/runtime/agent.go": `r.runtime.Exec(ctx, id, spec)` },
				[],
			),
		);
		expect(code).toBe(0);
	});

	test("an allowlisted finding keeps the gate green (the ratchet, armed)", () => {
		const code = runOnce(
			deps(
				{
					"go/internal/store/accounts.go": `x.Exec(ctx, "INSERT INTO t VALUES (1)")`,
				},
				["go/internal/store/accounts.go"],
			),
		);
		expect(code).toBe(0);
	});

	test("a NEW (non-allowlisted) finding exits 1", () => {
		const code = runOnce(
			deps(
				{ "go/internal/store/newfile.go": `x.Exec(ctx, "DELETE FROM t")` },
				[],
			),
		);
		expect(code).toBe(1);
	});

	test("a stale allowlist entry exits 1 (fail-closed)", () => {
		const code = runOnce(
			deps({ "go/internal/store/clean.go": `r.runtime.Exec(ctx, id, spec)` }, [
				"go/internal/store/clean.go", // no finding → stale → fail
			]),
		);
		expect(code).toBe(1);
	});

	test("removing a live allowlist entry surfaces the finding (exit 1)", () => {
		const files = {
			"go/internal/store/accounts.go": `x.Exec(ctx, "INSERT INTO t VALUES (1)")`,
		};
		expect(runOnce(deps(files, ["go/internal/store/accounts.go"]))).toBe(0);
		expect(runOnce(deps(files, []))).toBe(1);
	});
});
