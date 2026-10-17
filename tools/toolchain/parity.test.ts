// Drives the SHIPPED parity.ts end to end against a stub `nix`, so the Meissa
// half (rumdl/biome from gate-tools.nix's `meissa` output) is proven able to
// fail: a missing or foreign rumdl/biome on PATH must red the gate.

import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import { execFileSync } from "node:child_process";
import { chmod, mkdir, mkdtemp, readFile, rm, symlink } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

const SCRIPT_REL = "tools/toolchain/parity.ts";
const CORE_REL = "tools/toolchain/parity-core.ts";

// The gate resolves commands with `sh -c 'command -v …'`, so PATH must reach sh
// and nothing else ambient (a host rumdl would otherwise answer the probe):
// the resolved sh is symlinked alone into the stub bin dir.
const SH = execFileSync("sh", ["-c", "command -v sh"]).toString().trim();
// Stub `nix eval --json -f gate-tools.nix <output> …`: prints <output>.json.
// Shell builtins only — PATH holds no coreutils, so `cat` is unavailable.
const STUB_NIX = `#!/bin/sh
for a in "$@"; do
  case "$a" in langs|goAnalysis|meissa|identity)
    while IFS= read -r line || [ -n "$line" ]; do printf '%s\\n' "$line"; done <"$STUB_JSON/$a.json"
    exit 0 ;;
  esac
done
exit 1
`;

type Identity = { version: string; store: string; bins: string[] };

let repo: string;

/** A fake derivation: <repo>/store/<name>/bin/<bin>, executable. */
async function derivation(name: string, bin: string): Promise<Identity> {
	const store = join(repo, "store", name);
	await mkdir(join(store, "bin"), { recursive: true });
	await Bun.write(join(store, "bin", bin), "#!/bin/sh\n");
	await chmod(join(store, "bin", bin), 0o755);
	return { version: "1", store, bins: [bin] };
}

async function writeSet(output: string, set: Record<string, Identity>) {
	await Bun.write(join(repo, "json", `${output}.json`), JSON.stringify(set));
}

async function runParity(
	pathDirs: readonly string[],
	args: string[] = [],
	extraEnv: Record<string, string> = {},
) {
	const proc = Bun.spawn([process.execPath, SCRIPT_REL, ...args], {
		cwd: repo,
		env: {
			PATH: [join(repo, "stubbin"), ...pathDirs].join(":"),
			STUB_JSON: join(repo, "json"),
			...extraEnv,
		},
		stdout: "pipe",
		stderr: "pipe",
	});
	const exitCode = await proc.exited;
	const output =
		(await new Response(proc.stdout).text()) +
		(await new Response(proc.stderr).text());
	return { exitCode, output };
}

// The four tools gate-tools.nix's goAnalysis output exposes.
const GO_ANALYSIS_TOOLS = [
	"golangci-lint",
	"govulncheck",
	"go-licenses",
	"nilaway",
] as const;

let goodPath: string[];

beforeEach(async () => {
	repo = await mkdtemp(join(tmpdir(), "parity-meissa-"));
	await mkdir(join(repo, "tools", "toolchain"), { recursive: true });
	for (const rel of [SCRIPT_REL, CORE_REL]) {
		await Bun.write(
			join(repo, rel),
			await readFile(join(import.meta.dir, "..", "..", rel), "utf8"),
		);
	}
	await Bun.write(
		join(repo, "devenv.nix"),
		"{\n  packages = (with pkgs; [\n    buf\n  ])\n  ++ [ toolchainTools.bun ];\n}\n",
	);
	await mkdir(join(repo, "stubbin"), { recursive: true });
	await Bun.write(join(repo, "stubbin", "nix"), STUB_NIX);
	await chmod(join(repo, "stubbin", "nix"), 0o755);
	await symlink(SH, join(repo, "stubbin", "sh"));

	const buf = await derivation("buf", "buf");
	const bun = await derivation("bun", "bun");
	const rumdl = await derivation("meissa-rumdl", "rumdl");
	const biome = await derivation("meissa-biome", "biome");
	await writeSet("identity", { buf });
	await writeSet("langs", { bun });
	const battery: Record<string, Identity> = {};
	for (const tool of GO_ANALYSIS_TOOLS) {
		battery[tool] = await derivation(tool, tool);
	}
	await writeSet("goAnalysis", battery);
	await writeSet("meissa", { rumdl, biome });
	goodPath = [buf, bun, ...Object.values(battery), rumdl, biome].map((d) =>
		join(d.store, "bin"),
	);
});

afterEach(async () => {
	await rm(repo, { recursive: true, force: true });
});

describe("parity.ts Meissa linters", () => {
	// Control: the harness must be able to pass, or the red cases prove nothing.
	test("passes when PATH resolves Meissa's rumdl and biome", async () => {
		const res = await runParity(goodPath);
		expect(res.output).toContain("All 8 pinned tools match");
		expect(res.exitCode).toBe(0);
	});

	test("fails when Meissa's rumdl is absent from PATH", async () => {
		const res = await runParity(goodPath.filter((d) => !d.includes("rumdl")));
		expect(res.exitCode).toBe(1);
		expect(res.output).toMatch(/UNVERIFIABLE rumdl\s+-\s+not on PATH/);
	});

	// The case this change exists for: a nixpkgs rumdl still first on PATH.
	test("fails when PATH resolves a different rumdl derivation", async () => {
		const foreign = await derivation("nixpkgs-rumdl", "rumdl");
		const res = await runParity([join(foreign.store, "bin"), ...goodPath]);
		expect(res.exitCode).toBe(1);
		expect(res.output).toContain("MISMATCH     rumdl");
		expect(res.output).toContain(foreign.store);
	});

	test("refuses a pass when the meissa identity set is empty", async () => {
		await writeSet("meissa", {});
		const res = await runParity(goodPath);
		expect(res.exitCode).toBe(1);
		expect(res.output).toContain("0 Meissa tools");
	});
});

describe("parity.ts Go analysis battery", () => {
	for (const tool of GO_ANALYSIS_TOOLS) {
		test(`fails when the battery's ${tool} is absent from PATH`, async () => {
			const res = await runParity(
				goodPath.filter((d) => !d.includes(`/store/${tool}/`)),
			);
			expect(res.exitCode).toBe(1);
			expect(res.output).toMatch(
				new RegExp(`UNVERIFIABLE ${tool}\\s+-\\s+not on PATH`),
			);
		});
	}

	test("fails when PATH resolves a different golangci-lint derivation", async () => {
		const foreign = await derivation("nixpkgs-golangci-lint", "golangci-lint");
		const res = await runParity([join(foreign.store, "bin"), ...goodPath]);
		expect(res.exitCode).toBe(1);
		expect(res.output).toContain("MISMATCH     golangci-lint");
	});

	test("refuses a pass when the goAnalysis identity set is empty", async () => {
		await writeSet("goAnalysis", {});
		const res = await runParity(goodPath);
		expect(res.exitCode).toBe(1);
		expect(res.output).toContain("0 Go analysis tools");
	});

	test("refuses a pass when the langs identity set is empty", async () => {
		await writeSet("langs", {});
		const res = await runParity(goodPath);
		expect(res.exitCode).toBe(1);
		expect(res.output).toContain("0 language toolchains");
	});
});

describe("parity.ts TOOLCHAIN_PARITY_SCOPE", () => {
	const core = { TOOLCHAIN_PARITY_SCOPE: "core" };
	const battery = (d: string) =>
		GO_ANALYSIS_TOOLS.some((tool) => d.includes(`/store/${tool}/`));

	test("core passes without the battery and never evaluates goAnalysis", async () => {
		await rm(join(repo, "json", "goAnalysis.json"));
		const res = await runParity(
			goodPath.filter((d) => !battery(d)),
			[],
			core,
		);
		expect(res.exitCode).toBe(0);
		expect(res.output).toContain(
			"out of scope: goAnalysis (TOOLCHAIN_PARITY_SCOPE=core)",
		);
		expect(res.output).toContain("All 4 pinned tools match");
	});

	test("core still fails a mismatched core tool", async () => {
		const foreign = await derivation("nixpkgs-rumdl", "rumdl");
		const res = await runParity(
			[join(foreign.store, "bin"), ...goodPath],
			[],
			core,
		);
		expect(res.exitCode).toBe(1);
		expect(res.output).toContain("MISMATCH     rumdl");
	});

	test("unset scope still checks the battery", async () => {
		const res = await runParity(goodPath.filter((d) => !battery(d)));
		expect(res.exitCode).toBe(1);
		expect(res.output).not.toContain("out of scope");
	});

	for (const value of ["full", "Core", "go"]) {
		test(`refuses unknown scope ${value}`, async () => {
			const res = await runParity(goodPath, [], {
				TOOLCHAIN_PARITY_SCOPE: value,
			});
			expect(res.exitCode).toBe(1);
			expect(res.output).toContain(`TOOLCHAIN_PARITY_SCOPE=${value}`);
		});
	}
});

describe("parity.ts --print-nix-attrs", () => {
	// CI calls this before the closed sets are built, so it must not evaluate them.
	test("prints the devenv attrs without evaluating any closed set", async () => {
		for (const set of ["langs", "goAnalysis", "meissa"]) {
			await rm(join(repo, "json", `${set}.json`));
		}
		const res = await runParity(goodPath, ["--print-nix-attrs"]);
		expect(res.exitCode).toBe(0);
		expect(res.output.trim()).toBe('["buf"]');
	});

	test("refuses an empty devenv parse", async () => {
		await Bun.write(join(repo, "devenv.nix"), "{ }\n");
		const res = await runParity(goodPath, ["--print-nix-attrs"]);
		expect(res.exitCode).toBe(1);
		expect(res.output).toContain("0 devenv.nix packages");
	});
});
