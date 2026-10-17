import { readdirSync, readFileSync } from "node:fs";
import { relative, sep } from "node:path";

type Suite = "pgtest" | "microvm";

type SuiteConfig = {
	harnessPath: string;
	skipPattern: RegExp;
	requirePattern: RegExp;
	skipLabel: string;
};

const SUITE_CONFIG = {
	pgtest: {
		harnessPath: "internal/pgshare/pgshare.go",
		skipPattern: /t\.Skip\("(no COMPASS_TEST_DATABASE_DSN[^"]*)"\)/,
		requirePattern: /\bpgtest\.RequireDSN\s*\(/,
		skipLabel: "real-Postgres",
	},
	microvm: {
		harnessPath: "internal/microvmtest/microvmtest.go",
		skipPattern: /t\.Skip\("(\/dev\/kvm is not openable[^"]*)"\)/,
		requirePattern: /\bmicrovmtest\.Require\s*\(/,
		skipLabel: "microVM",
	},
} satisfies Record<Suite, SuiteConfig>;

export type SuiteAssertionResult = {
	checkedPackages: string[];
	failures: string[];
};

/** Parse space-delimited Go package arguments supplied by the setup output. */
export function parsePackageList(value: string | undefined): string[] {
	if (value === undefined || value.trim() === "") return [];
	const roots = value
		.trim()
		.split(/\s+/)
		.map((entry) => {
			const root = entry.startsWith("./") ? entry.slice(2) : "";
			if (
				root === "" ||
				root === "..." ||
				root
					.split("/")
					.some(
						(part) =>
							part === "" ||
							part === "." ||
							part === ".." ||
							part === "..." ||
							part.includes("\\"),
					)
			) {
				throw new Error(`invalid Go package argument: ${entry}`);
			}
			return root;
		});
	if (new Set(roots).size !== roots.length) {
		throw new Error("selected Go package arguments contain duplicates");
	}
	return roots;
}

/** Check selected callers only, so skipped Go packages cannot mask missing suites. */
export function assertSuiteResults(input: {
	suite: Suite;
	modulePath: string;
	selectedPackages: readonly string[];
	callerPackages: readonly string[];
	skipMessage: string;
	testOutput: string;
}): SuiteAssertionResult {
	const failures: string[] = [];
	if (input.selectedPackages.length === 0) {
		failures.push(`${input.suite}: selected package list is empty`);
	}
	if (
		input.skipMessage !== "" &&
		input.testOutput.includes(input.skipMessage)
	) {
		failures.push(`${input.suite}: test harness reported a skip`);
	}
	const selected = new Set(input.selectedPackages);
	const selectedCallers = [...new Set(input.callerPackages)]
		.filter((pkg) => selected.has(pkg))
		.sort();
	const prefix = `${input.modulePath}/`;
	for (const pkg of selectedCallers) {
		const importPath = `${prefix}${pkg}`;
		const escaped = importPath.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
		if (!new RegExp(`^ok\\s+${escaped}(?:\\s|$)`, "m").test(input.testOutput)) {
			failures.push(
				`${input.suite}: selected package '${pkg}' did not report ok`,
			);
		}
	}
	return { checkedPackages: selectedCallers, failures };
}

function listGoFiles(directory: string): string[] {
	return readdirSync(directory, { withFileTypes: true }).flatMap((entry) => {
		const path = `${directory}/${entry.name}`;
		if (entry.isDirectory()) return listGoFiles(path);
		return entry.isFile() && entry.name.endsWith(".go") ? [path] : [];
	});
}

function callerPackages(goRoot: string, requirePattern: RegExp): string[] {
	return listGoFiles(goRoot).flatMap((path) => {
		if (!requirePattern.test(readFileSync(path, "utf8"))) return [];
		const packagePath = relative(goRoot, path)
			.split(sep)
			.slice(0, -1)
			.join("/");
		return packagePath === "" ? [] : [packagePath];
	});
}

function modulePath(goRoot: string): string {
	const match = readFileSync(`${goRoot}/go.mod`, "utf8").match(
		/^module\s+(\S+)\s*$/m,
	);
	if (!match?.[1]) throw new Error("could not read module path from go.mod");
	return match[1];
}

function main(): void {
	const suite = process.argv[2];
	if (suite !== "pgtest" && suite !== "microvm") {
		throw new Error("usage: suite-assertions.ts <pgtest|microvm> <test-log>");
	}
	const logPath = process.argv[3];
	if (logPath === undefined) {
		throw new Error("test log path is required");
	}
	const goRoot = process.cwd();
	const config = SUITE_CONFIG[suite];
	const harness = readFileSync(`${goRoot}/${config.harnessPath}`, "utf8");
	const skipMatch = harness.match(config.skipPattern);
	if (!skipMatch?.[1]) {
		throw new Error(
			`could not read the skip message from ${config.harnessPath}`,
		);
	}
	const packageEnv =
		process.env[suite === "pgtest" ? "PGTEST_PACKAGES" : "MICROVM_PACKAGES"];
	const result = assertSuiteResults({
		suite,
		modulePath: modulePath(goRoot),
		selectedPackages: parsePackageList(packageEnv),
		callerPackages: callerPackages(goRoot, config.requirePattern),
		skipMessage: skipMatch[1],
		testOutput: readFileSync(logPath, "utf8"),
	});
	for (const failure of result.failures) console.error(`::error::${failure}`);
	if (result.failures.length > 0) {
		process.exitCode = 1;
		return;
	}
	console.log(
		`${suite}: checked ${result.checkedPackages.length} ${config.skipLabel} packages`,
	);
}

if (import.meta.main) main();
