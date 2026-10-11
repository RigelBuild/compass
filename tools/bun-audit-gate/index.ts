import { resolve } from "node:path";

export type AllowlistEntry = {
	id: string;
	package: string;
	reason: string;
};

export type AuditFinding = {
	id: string;
	package: string;
	title: string;
	severity: string;
	vulnerableVersions: string;
};

export type LockfileAudit = {
	directory: string;
	findings: ReadonlyArray<AuditFinding>;
};

const LOCKFILE_DIRS: ReadonlyArray<string> = ["."];

type UnknownRecord = { [key: string]: unknown };

function isRecord(value: unknown): value is UnknownRecord {
	return typeof value === "object" && value !== null && !Array.isArray(value);
}

function parseAllowlist(value: unknown): AllowlistEntry[] {
	if (!Array.isArray(value)) {
		throw new Error("allowlist must be an array");
	}

	const entries: AllowlistEntry[] = [];
	for (const [index, entry] of value.entries()) {
		if (
			!isRecord(entry) ||
			typeof entry.id !== "string" ||
			typeof entry.package !== "string" ||
			typeof entry.reason !== "string"
		) {
			throw new Error(
				`allowlist entry ${index} must have string id, package, and reason fields`,
			);
		}
		entries.push({
			id: entry.id,
			package: entry.package,
			reason: entry.reason,
		});
	}
	return entries;
}

function parseAuditOutput(output: string): AuditFinding[] {
	const payload: unknown = JSON.parse(output);
	if (!isRecord(payload)) {
		throw new Error("expected an object keyed by package name");
	}

	const findings: AuditFinding[] = [];
	for (const [packageName, records] of Object.entries(payload)) {
		if (!Array.isArray(records)) {
			throw new Error(`expected an advisory array for package ${packageName}`);
		}
		for (const record of records) {
			if (
				!isRecord(record) ||
				typeof record.id !== "number" ||
				!Number.isSafeInteger(record.id) ||
				typeof record.url !== "string" ||
				typeof record.title !== "string" ||
				typeof record.severity !== "string" ||
				typeof record.vulnerable_versions !== "string"
			) {
				throw new Error(`invalid advisory record for package ${packageName}`);
			}

			const advisoryId = new URL(record.url).pathname
				.split("/")
				.filter(Boolean)
				.at(-1);
			if (advisoryId === undefined || !advisoryId.startsWith("GHSA-")) {
				throw new Error(`invalid advisory URL for package ${packageName}`);
			}
			findings.push({
				id: advisoryId,
				package: packageName,
				title: record.title,
				severity: record.severity,
				vulnerableVersions: record.vulnerable_versions,
			});
		}
	}
	return findings;
}

export function evaluateAuditFindings(
	audits: ReadonlyArray<LockfileAudit>,
	allowlist: ReadonlyArray<AllowlistEntry>,
): string[] {
	const allowedById = new Map(allowlist.map((entry) => [entry.id, entry]));
	const observedIds = new Set<string>();
	const failures: string[] = [];

	for (const audit of audits) {
		for (const finding of audit.findings) {
			if (!allowedById.has(finding.id)) {
				failures.push(
					`${finding.id} in ${finding.package} (${finding.severity}) from ${audit.directory}: ${finding.title} [${finding.vulnerableVersions}]`,
				);
			} else {
				observedIds.add(finding.id);
			}
		}
	}

	for (const entry of allowlist) {
		if (!observedIds.has(entry.id)) {
			failures.push(
				`stale allowlist entry ${entry.id} for ${entry.package}: no lockfile reports this advisory`,
			);
		}
	}
	return failures;
}

async function auditDirectory(
	directory: string,
	root: string,
): Promise<LockfileAudit> {
	const auditProcess = Bun.spawn(["bun", "audit", "--json"], {
		cwd: resolve(root, directory),
		stdout: "pipe",
		stderr: "pipe",
	});
	const [stdout, stderr, exitCode] = await Promise.all([
		new Response(auditProcess.stdout).text(),
		new Response(auditProcess.stderr).text(),
		auditProcess.exited,
	]);
	if (exitCode !== 0 && exitCode !== 1) {
		throw new Error(
			`bun audit failed for ${directory} (exit ${exitCode})${stderr.trim().length > 0 ? `: ${stderr.trim()}` : ""}`,
		);
	}
	return { directory, findings: parseAuditOutput(stdout) };
}

async function runGate(): Promise<number> {
	try {
		const allowlistValue: unknown = await Bun.file(
			new URL("./allowlist.json", import.meta.url),
		).json();
		const allowlist = parseAllowlist(allowlistValue);
		const root = resolve(process.env.GATE_ROOT ?? process.cwd());
		const audits = await Promise.all(
			LOCKFILE_DIRS.map((directory) => auditDirectory(directory, root)),
		);
		const failures = evaluateAuditFindings(audits, allowlist);
		if (failures.length > 0) {
			console.error(`bun-audit-gate: ${failures.length} advisory failure(s):`);
			for (const failure of failures) console.error(`  ${failure}`);
			return 1;
		}
		console.log(`bun-audit-gate: OK — audited ${LOCKFILE_DIRS.join(", ")}`);
		return 0;
	} catch (error) {
		const message = error instanceof Error ? error.message : String(error);
		console.error(`bun-audit-gate: ${message}`);
		return 1;
	}
}

if (import.meta.main) {
	process.exitCode = await runGate();
}
