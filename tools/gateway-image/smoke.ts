#!/usr/bin/env bun

import { spawnSync } from "node:child_process";
import { randomBytes, randomUUID } from "node:crypto";
import { mkdirSync, rmSync, writeFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { EXIT, healthzOk, modelsListOk } from "./core.ts";

const workspaceRoot = join(dirname(fileURLToPath(import.meta.url)), "..", "..");
const laneDir = dirname(fileURLToPath(import.meta.url));

/** Ceiling for one podman or skopeo call; the image load is the slowest. */
const COMMAND_TIMEOUT_MS = 300_000;

type CommandResult = { status: number | null; stdout: string; stderr: string };

function failUsage(): never {
	console.error(
		"usage: smoke.ts [--out <dir>] [--fault no-broker-env|wrong-token]",
	);
	process.exit(EXIT.usage);
}

/** A deliberate fault proves the smoke can fail: each must exit 7. */
type Fault = "no-broker-env" | "wrong-token";

function parseArgs(argv: readonly string[]): { out: string; fault?: Fault } {
	const values = new Map<string, string>();
	for (let index = 0; index < argv.length; index += 2) {
		const name = argv[index];
		const value = argv[index + 1];
		if (
			(name !== "--out" && name !== "--fault") ||
			value === undefined ||
			value === "" ||
			values.has(name)
		) {
			failUsage();
		}
		values.set(name, value);
	}
	const fault = values.get("--fault");
	if (
		fault !== undefined &&
		fault !== "no-broker-env" &&
		fault !== "wrong-token"
	) {
		failUsage();
	}
	return {
		out: resolve(workspaceRoot, values.get("--out") ?? join(laneDir, "out")),
		fault,
	};
}

function run(
	command: string,
	args: readonly string[],
	options: { env?: NodeJS.ProcessEnv; timeoutMs?: number } = {},
): CommandResult {
	// A stalled podman client must not hang the smoke past its budget.
	const result = spawnSync(command, [...args], {
		cwd: workspaceRoot,
		encoding: "utf8",
		stdio: ["ignore", "pipe", "pipe"],
		env: options.env,
		timeout: options.timeoutMs ?? COMMAND_TIMEOUT_MS,
		killSignal: "SIGKILL",
	});
	return {
		status: result.status,
		stdout: result.stdout ?? "",
		stderr:
			result.stderr ??
			(result.error instanceof Error ? result.error.message : ""),
	};
}

function requireCommand(
	command: string,
	args: readonly string[],
): CommandResult {
	const result = run(command, args);
	if (result.status !== 0) {
		throw new Error(
			`${command} failed (exit ${result.status ?? 1}): ${result.stderr.trim()}`,
		);
	}
	return result;
}

async function waitFor(
	probe: (remainingMs: number) => Promise<boolean>,
	milliseconds: number,
): Promise<boolean> {
	const deadline = Date.now() + milliseconds;
	while (Date.now() < deadline) {
		try {
			// A probe that succeeds after the deadline is still a late service.
			if ((await probe(deadline - Date.now())) && Date.now() <= deadline)
				return true;
		} catch {
			// The service is not ready until the probe succeeds.
		}
		// The smoke contract polls every 500 ms for its bounded readiness checks.
		// biome-ignore lint/plugin: The smoke contract requires a 500 ms poll.
		await Bun.sleep(500);
	}
	return false;
}

const { out: outDir, fault } = parseArgs(process.argv.slice(2));
const suffix = randomUUID().slice(0, 8);
const smokeDir = join(outDir, `smoke-${suffix}`);
const tarPath = join(outDir, `smoke-${suffix}.tar`);
const tokenPath = join(smokeDir, "gateway.token");
const image = `compass-gateway-smoke:${suffix}`;
const network = `gw-smoke-${suffix}`;
const broker = `gw-broker-${suffix}`;
const gateway = `gw-gateway-${suffix}`;
let failed = false;

function check(name: string, passed: boolean): void {
	console.error(`gateway-image smoke: ${passed ? "PASS" : "FAIL"} ${name}`);
	if (!passed) failed = true;
}

try {
	mkdirSync(outDir, { recursive: true });
	const copied = requireCommand("skopeo", [
		"copy",
		`oci:${join(outDir, "oci")}`,
		`docker-archive:${tarPath}:${image}`,
	]);
	if (copied.stderr !== "") console.error(copied.stderr.trimEnd());
	requireCommand("podman", ["load", "-i", tarPath]);
	requireCommand("podman", ["network", "create", network]);
	requireCommand("podman", [
		"run",
		"-d",
		"--name",
		broker,
		"--network",
		network,
		"--entrypoint",
		"/usr/local/bin/omp",
		image,
		"auth-broker",
		"serve",
		"--bind=0.0.0.0:8765",
	]);

	const brokerReady = await waitFor(async (remainingMs) => {
		const probe = run(
			"podman",
			[
				"exec",
				broker,
				"bun",
				"-e",
				"fetch('http://127.0.0.1:8765/v1/healthz', { signal: AbortSignal.timeout(2000) }).then(r => process.exit(r.status === 200 ? 0 : 1)).catch(() => process.exit(1))",
			],
			{ timeoutMs: Math.max(remainingMs, 1) },
		);
		return probe.status === 0;
	}, 30_000);
	if (!brokerReady) {
		throw new Error("broker health check timed out after 30 seconds");
	}
	const brokerToken = requireCommand("podman", [
		"exec",
		broker,
		"cat",
		"/tmp/.omp/auth-broker.token",
	]).stdout.trim();
	if (brokerToken === "") throw new Error("broker returned an empty token");

	mkdirSync(smokeDir, { recursive: true });
	writeFileSync(tokenPath, randomBytes(32).toString("hex"), { mode: 0o600 });
	const gatewayToken =
		fault === "wrong-token"
			? randomBytes(32).toString("hex")
			: (await Bun.file(tokenPath).text()).trim();
	const gatewayRun = run(
		"podman",
		[
			"run",
			"-d",
			"--name",
			gateway,
			"--network",
			network,
			"-p",
			"127.0.0.1::4000",
			"-v",
			`${tokenPath}:/run/compass/gateway.token:ro,Z`,
			...(fault === "no-broker-env"
				? []
				: [
						"-e",
						`OMP_AUTH_BROKER_URL=http://gw-broker-${suffix}:8765`,
						"-e",
						"OMP_AUTH_BROKER_TOKEN",
					]),
			image,
		],
		{ env: { ...process.env, OMP_AUTH_BROKER_TOKEN: brokerToken } },
	);
	if (gatewayRun.status !== 0) {
		throw new Error(
			`podman run gateway failed (exit ${gatewayRun.status ?? 1}): ${gatewayRun.stderr.trim()}`,
		);
	}
	const portText = requireCommand("podman", [
		"port",
		gateway,
		"4000",
	]).stdout.trim();
	const portMatch = /(?:^|:)([0-9]+)$/.exec(portText);
	if (portMatch?.[1] === undefined) {
		throw new Error(`could not parse podman port output: ${portText}`);
	}
	const baseUrl = `http://127.0.0.1:${portMatch[1]}`;

	const healthy = await waitFor(async () => {
		const response = await fetch(`${baseUrl}/healthz`, {
			signal: AbortSignal.timeout(2500),
		});
		return healthzOk(response.status, await response.text());
	}, 30_000);
	check("/healthz returns ok within 30 seconds", healthy);

	let unauthorized = false;
	try {
		const response = await fetch(`${baseUrl}/v1/models`, {
			signal: AbortSignal.timeout(5000),
		});
		unauthorized = response.status === 401;
	} catch {
		unauthorized = false;
	}
	check("/v1/models rejects missing authorization", unauthorized);

	let authorized = false;
	try {
		const response = await fetch(`${baseUrl}/v1/models`, {
			headers: { Authorization: `Bearer ${gatewayToken}` },
			signal: AbortSignal.timeout(5000),
		});
		authorized = modelsListOk(response.status, await response.text());
	} catch {
		authorized = false;
	}
	check("/v1/models accepts the mounted gateway token", authorized);

	const stopped = run("podman", ["stop", "-t", "25", gateway]);
	const exitCode =
		stopped.status === 0
			? run("podman", ["inspect", "--format", "{{.State.ExitCode}}", gateway])
			: { status: stopped.status, stdout: "", stderr: stopped.stderr };
	check(
		"podman stop exits with container code 143",
		stopped.status === 0 &&
			exitCode.status === 0 &&
			exitCode.stdout.trim() === "143",
	);
} catch (error) {
	failed = true;
	console.error(
		`gateway-image smoke: FAIL ${error instanceof Error ? error.message : String(error)}`,
	);
} finally {
	if (failed) {
		for (const name of [broker, gateway]) {
			const logs = run("podman", ["logs", name]);
			if (logs.status !== 0 && logs.stderr !== "") continue;
			console.error(`gateway-image smoke: podman logs ${name}`);
			if (logs.stdout !== "") console.error(logs.stdout.trimEnd());
			if (logs.stderr !== "") console.error(logs.stderr.trimEnd());
		}
	}
	for (const args of [
		["rm", "-f", broker, gateway],
		["network", "rm", network],
		["rmi", image],
	]) {
		const result = run("podman", args);
		if (result.status !== 0) {
			failed = true;
			if (result.stderr !== "") {
				console.error(`gateway-image smoke cleanup: ${result.stderr.trim()}`);
			}
		}
	}
	for (const path of [smokeDir, tarPath]) {
		try {
			rmSync(path, { recursive: true, force: true });
		} catch (error) {
			failed = true;
			console.error(`gateway-image smoke cleanup: ${String(error)}`);
		}
	}
}

process.exitCode = failed ? EXIT.smokeFailed : 0;
