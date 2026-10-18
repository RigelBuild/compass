// The public declaration surface must not expose Effect or OTel types from the transport module.

import { expect, test } from "bun:test";
import { mkdtempSync, rmSync } from "node:fs";
import { join, resolve } from "node:path";
import * as ts from "typescript";

const PKG_ROOT = resolve(import.meta.dir, "..", "..");

// A type leaks iff its declaration file resolves inside one of these packages.
// Bun's isolated store nests each package at
// `node_modules/.bun/<pkg>@<ver>/node_modules/<pkg>/...`, so the real package
// path segment is present regardless of the store layout.
const FORBIDDEN =
	/[/\\]node_modules[/\\](effect|@effect[/\\]opentelemetry|@opentelemetry[/\\])/;

// biome-ignore lint/plugin: 130s is a deliberate slow-body budget (RIG-3784), not a crash guard: a correct tsc --emitDeclarationOnly emit runs ~5s local but 45-68s on a contended runner (~9-14x), and the sync spawnSync has no intermediate signal to event-gate on. See the tail comment for the near-floor measurement.
test("the package public type surface carries no effect/@effect/opentelemetry/@opentelemetry type", () => {
	// Emit under the package root so declaration resolution can find its dependencies; an OS temp dir would hide leaked types.
	const out = mkdtempSync(join(PKG_ROOT, ".dts-surface-"));
	try {
		// Emit signature-only declarations for the whole package.
		const tsc = Bun.resolveSync("typescript/bin/tsc", PKG_ROOT);
		const emit = Bun.spawnSync(
			[
				process.execPath,
				tsc,
				"--declaration",
				"--emitDeclarationOnly",
				"--noEmit",
				"false",
				"--outDir",
				out,
				"--skipLibCheck",
			],
			{ cwd: PKG_ROOT, stderr: "pipe", stdout: "pipe" },
		);
		expect(
			emit.exitCode,
			emit.stderr.toString() || emit.stdout.toString(),
		).toBe(0);

		// A program over the emitted declarations. `moduleResolution: bundler`
		// lets the checker resolve the emitted relative `.d.ts` edges and the
		// bare `effect`/`@opentelemetry/*` specifiers back to their real files.
		const entryDts = join(out, "src", "index.d.ts");
		const program = ts.createProgram([entryDts], {
			moduleResolution: ts.ModuleResolutionKind.Bundler,
			skipLibCheck: true,
			noEmit: true,
		});
		const checker = program.getTypeChecker();
		const entry = program.getSourceFile(entryDts);
		expect(entry, `emitted entry not found: ${entryDts}`).toBeDefined();
		const moduleSymbol = checker.getSymbolAtLocation(entry as ts.SourceFile);
		expect(moduleSymbol, "emitted entry is not a module").toBeDefined();

		const unalias = (symbol: ts.Symbol): ts.Symbol =>
			symbol.flags & ts.SymbolFlags.Alias
				? checker.getAliasedSymbol(symbol)
				: symbol;

		const resolveName = (name: ts.EntityName) => {
			let node: ts.Node = name;
			while (ts.isQualifiedName(node)) node = node.right;
			return checker.getSymbolAtLocation(node);
		};

		// Recurse through signature types only, including aliases, to find indirect leaks.
		const seen = new Set<ts.Symbol>();
		const files = new Set<string>();
		const collect = (symbol: ts.Symbol) => {
			const target = unalias(symbol);
			if (seen.has(target)) return;
			seen.add(target);
			for (const decl of target.getDeclarations() ?? []) {
				files.add(decl.getSourceFile().fileName);
				const visit = (node: ts.Node) => {
					if (ts.isTypeReferenceNode(node)) {
						const s = resolveName(node.typeName);
						if (s) collect(s);
					} else if (ts.isImportTypeNode(node) && node.qualifier) {
						const s = resolveName(node.qualifier);
						if (s) collect(s);
					} else if (ts.isExpressionWithTypeArguments(node)) {
						// Heritage bases are not TypeReferenceNodes, so resolve the expression symbol directly.
						const s = checker.getSymbolAtLocation(node.expression);
						if (s) collect(s);
					} else if (ts.isTypeQueryNode(node)) {
						// A `typeof <binding>` query names its target as an EntityName
						// under `.exprName`, not a TypeReferenceNode.
						const s = resolveName(node.exprName);
						if (s) collect(s);
					}
					ts.forEachChild(node, visit);
				};
				visit(decl);
			}
		};

		// Only exports declared in transport are roots; other package exports are outside this containment contract.
		const TRANSPORT_DECL = /[/\\]src[/\\]transport[/\\]/;
		for (const exp of checker.getExportsOfModule(moduleSymbol as ts.Symbol)) {
			const target = unalias(exp);
			const fromTransport = (target.getDeclarations() ?? []).some((d) =>
				TRANSPORT_DECL.test(d.getSourceFile().fileName),
			);
			if (fromTransport) collect(exp);
		}

		const leaked = [...files]
			.filter((f) => FORBIDDEN.test(f))
			.map((f) => f.replace(/^.*[/\\]node_modules[/\\]/, ""));

		// The walk actually traversed the surface (guards a silent no-op).
		expect(seen.size).toBeGreaterThan(1);
		expect(leaked).toEqual([]);
	} finally {
		rmSync(out, { recursive: true, force: true });
	}
	// Cold declaration emit and a second type walk are slow on contended CI runners.
}, 130_000);
