// The export-surface containment test: the frozen rule of the parent adoption
// record (docs/designs/platform/compass-agent-effect-adoption/design.md, Global
// Constraints — "the public `.d.ts` stays free of the `effect` package") made
// mechanical, extended by this record
// (docs/designs/platform/compass-agent-effect-otel/design.md, O1a) to cover
// `@effect/opentelemetry` and `@opentelemetry/*` now that the OTel layer lives
// inside src/transport/.
//
// The invariant: NO type from `effect`, `@effect/opentelemetry`, or
// `@opentelemetry/*` is reachable through the package's PUBLIC type surface — the
// types a consumer of `@compass/agent` can name starting from the package entry
// (src/index.ts, which re-exports the transport surface: createUnixSocketTransport,
// RunnerTransport, createSocketControlSource, createSocketFrameSink).
//
// This is TYPE-SURFACE reachability, not MODULE reachability, and the distinction
// is the whole point of the parent record's containment mechanism. Internal
// modules carry effect types freely: runtime-channel.ts exports `TransportRuntime
// = ManagedRuntime<never, never>` and threads it through a module-private WeakMap,
// and publish-spine.ts's `createPublishSpine` takes it as a parameter — but
// neither is re-exported from the package entry, and the exported `PublishSpine`
// interface names none of it, so `effect` never surfaces to a consumer.
//
// Mechanism: emit the package's declarations with tsc, then walk the exported
// SYMBOLS from the emitted index.d.ts with the TypeScript checker, recursing
// through each symbol's declaration. Running over the emitted `.d.ts` (not the
// `.ts` source) is deliberate: a `.d.ts` declaration is SIGNATURE-ONLY — tsc has
// already stripped function bodies — so walking declaration syntax IS walking the
// public type surface, with no implementation-detail noise (a bare
// `ManagedRuntime.make(...)` in a function body never appears). A leak is a
// referenced type whose declaration file resolves inside a forbidden package.
//
// This is a red test: writing an exported signature that returns a bare
// effect/otel type, or re-exporting otel-layer/runtime-channel from an entry,
// reddens it.

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

test("the package public type surface carries no effect/@effect/opentelemetry/@opentelemetry type", () => {
	// Emit INTO the package's own `node_modules/` (not the package root, not the
	// OS tmpdir). Two constraints pin this location:
	//   1. Bare-specifier resolution — node/tsc bundler resolution probes
	//      `<ancestor>/node_modules` at each ancestor directory of the emitted
	//      `.d.ts`, so from `<pkg>/node_modules/.dts-surface-*/src/…` the ancestor
	//      `<pkg>` still reaches `<pkg>/node_modules`, where `effect` /
	//      `@opentelemetry/*` resolve to their real declaration files. Emitted to
	//      the OS tmpdir they would not resolve, and a re-exported effect type
	//      would show ZERO declaration files — a silent containment hole. (The
	//      parent `<pkg>/node_modules` is guaranteed present: it is the same tree
	//      the resolution walk-up needs, so its absence fails `Bun.resolveSync`
	//      below first, more loudly than this `mkdtempSync`.)
	//   2. No VCS-walk race — under the main-only full sweep (`moon run :ci`, max
	//      concurrency) moon's git-based hasher walks the working tree for other
	//      tasks while this test creates and `rmSync`s its temp dir. A dir under
	//      the package ROOT is inside that walk, so git racing the teardown dies
	//      `exit 128 … No such file`. `node_modules/` is pruned whole from the git
	//      walk, so the transient emit dir is invisible to the hasher. (Trade-off:
	//      a crash before the `finally` rmSync leaves git-invisible litter under
	//      node_modules, cleared by a reinstall; accepted over the CI race.)
	const out = mkdtempSync(join(PKG_ROOT, "node_modules", ".dts-surface-"));
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

		// Walk the type closure of a symbol over the emitted declarations:
		// record each declaration's file, then resolve every type reference in
		// it and recurse. Because the declarations are signature-only, this
		// visits exactly the parameter/return/member/heritage types a consumer
		// can name — and recursing through our own type aliases (TransportRuntime
		// et al., were they ever exposed) would surface an indirect leak too.
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
						// A heritage base — `interface X extends Base` or
						// `extends Ns.Base<T>` — names its base as an
						// ExpressionWithTypeArguments whose `.expression` is an
						// Identifier (`Base`) or PropertyAccessExpression (`Ns.Base`),
						// NOT a TypeReferenceNode. The base's type ARGUMENTS are caught
						// by the generic recursion below; the base type itself is only
						// reached here. getSymbolAtLocation resolves both node shapes.
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

		// Roots = the package exports whose declaration lives under
		// src/transport/. That is exactly the record's rule ("no effect type in
		// any signature exported FROM src/transport/"): createUnixSocketTransport,
		// RunnerTransport, createSocketControlSource, createSocketFrameSink. Other
		// package exports (CompassAgent et al.) reach @opentelemetry/api through
		// the OMP SDK's own pre-existing dependency — outside this record's
		// transport-containment scope, so they are not roots here.
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
}, 60_000);
