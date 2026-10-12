#!/usr/bin/env bun

import { spawnSync } from "node:child_process";
import { randomBytes, randomUUID } from "node:crypto";
import { mkdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import {
	EXIT,
	fakeCompassDoorResponse,
	healthzOk,
	modelsListOk,
} from "./core.ts";

const workspaceRoot = join(dirname(fileURLToPath(import.meta.url)), "..", "..");
const laneDir = dirname(fileURLToPath(import.meta.url));

/** Ceiling for one podman or skopeo call; the image load is the slowest. */
const COMMAND_TIMEOUT_MS = 300_000;

type CommandResult = { status: number | null; stdout: string; stderr: string };
type SmokeServer = {
	port: number | undefined;
	stop(closeActiveConnections?: boolean): void;
};

function failUsage(): never {
	console.error(
		"usage: smoke.ts [--out <dir>] [--fault no-server-env|wrong-token]",
	);
	process.exit(EXIT.usage);
}

/** A deliberate fault proves the smoke can fail: each must exit 7. */
type Fault = "no-server-env" | "wrong-token";

function parseArgs(argv: readonly string[]): { out: string; fault?: Fault } {
	if (argv.length === 1 && argv[0] === "--help") {
		console.log(
			"usage: smoke.ts [--out <dir>] [--fault no-server-env|wrong-token]",
		);
		process.exit(0);
	}

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
		fault !== "no-server-env" &&
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
	options: { env?: NodeJS.ProcessEnv; timeoutMs?: number; cwd?: string } = {},
): CommandResult {
	// A stalled podman client must not hang the smoke past its budget.
	const result = spawnSync(command, [...args], {
		cwd: options.cwd ?? workspaceRoot,
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
const serviceTokenPath = join(smokeDir, "service.token");
const certPath = join(smokeDir, "server-ca.crt");
const keyPath = join(smokeDir, "server-ca.key");
const image = `compass-gateway-smoke:${suffix}`;
const gateway = `gw-gateway-${suffix}`;
const serviceToken = randomBytes(32).toString("hex");
const agentToken = randomBytes(32).toString("hex");
let failed = false;
let activeFakeServer: SmokeServer | undefined;

function check(name: string, passed: boolean): void {
	console.error(`gateway-image smoke: ${passed ? "PASS" : "FAIL"} ${name}`);
	if (!passed) failed = true;
}

try {
	mkdirSync(outDir, { recursive: true });
	mkdirSync(smokeDir, { recursive: true });
	writeFileSync(serviceTokenPath, `${serviceToken}\n`, { mode: 0o600 });
	const certResult = run(
		"go",
		[
			"run",
			"./cmd/compass-gen-cert",
			"--cert-out",
			certPath,
			"--key-out",
			keyPath,
			"--hosts",
			"host.containers.internal,127.0.0.1",
		],
		{ cwd: join(workspaceRoot, "go") },
	);
	if (certResult.status !== 0) {
		throw new Error(
			`compass-gen-cert failed (exit ${certResult.status ?? 1}): ${certResult.stderr.trim()}`,
		);
	}
	if (certResult.stderr !== "") console.error(certResult.stderr.trimEnd());

	const fakeServer = Bun.serve({
		hostname: "127.0.0.1",
		port: 0,
		tls: { cert: readFileSync(certPath), key: readFileSync(keyPath) },
		// The wrong-token fault makes the door reject the mounted service token.
		fetch: (request) =>
			fakeCompassDoorResponse(request, {
				serviceToken:
					fault === "wrong-token"
						? randomBytes(32).toString("hex")
						: serviceToken,
				agentToken,
			}),
	});
	const fakeServerPort = fakeServer.port;
	if (fakeServerPort === undefined)
		throw new Error("fake Compass Server did not bind a port");
	activeFakeServer = fakeServer;

	const copied = requireCommand("skopeo", [
		"copy",
		`oci:${join(outDir, "oci")}`,
		`docker-archive:${tarPath}:${image}`,
	]);
	if (copied.stderr !== "") console.error(copied.stderr.trimEnd());
	requireCommand("podman", ["load", "-i", tarPath]);

	const gatewayRunArgs = [
		"run",
		"-d",
		"--name",
		gateway,
		"--network",
		"pasta:--map-host-loopback,169.254.1.2",
		"-p",
		"127.0.0.1::4000",
		"-v",
		`${serviceTokenPath}:/run/compass/service.token:ro,Z`,
		"-v",
		`${certPath}:/run/compass/server-ca.crt:ro,z`,
		"-e",
		"COMPASS_SERVICE_TOKEN_FILE=/run/compass/service.token",
		"-e",
		`COMPASS_SERVER_CA_FILE=/run/compass/server-ca.crt`,
		"-e",
		`COMPASS_GATEWAY_BIND=0.0.0.0:4000`,
		"-e",
		"COMPASS_GATEWAY_DRAIN_MS=20000",
		...(fault === "no-server-env"
			? []
			: [
					"-e",
					`COMPASS_SERVER_URL=https://host.containers.internal:${fakeServerPort}`,
				]),
		image,
	];
	const gatewayRun = run("podman", gatewayRunArgs);
	if (gatewayRun.status !== 0) {
		throw new Error(
			`podman run gateway failed (exit ${gatewayRun.status ?? 1}): ${gatewayRun.stderr.trim()}`,
		);
	}

	if (fault === "no-server-env") {
		const waited = run("podman", ["wait", gateway], { timeoutMs: 30_000 });
		const state = run("podman", [
			"inspect",
			"--format",
			"{{.State.ExitCode}} {{.State.Status}}",
			gateway,
		]);
		const exitedBeforeBind =
			waited.status === 0 &&
			state.status === 0 &&
			/^[1-9][0-9]* exited$/.test(state.stdout.trim());
		check(
			"unset COMPASS_SERVER_URL exits non-zero before the gateway binds",
			exitedBeforeBind,
		);
		throw new Error("fault --fault no-server-env verified the pre-bind exit");
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

	let authorized = false;
	try {
		const response = await fetch(`${baseUrl}/v1/models`, {
			headers: { Authorization: `Bearer ${agentToken}` },
			signal: AbortSignal.timeout(5000),
		});
		authorized = modelsListOk(response.status, await response.text());
	} catch {
		authorized = false;
	}
	check("/v1/models returns a model list for a valid agent token", authorized);

	let unauthorized = false;
	try {
		const response = await fetch(`${baseUrl}/v1/models`, {
			headers: { Authorization: `Bearer ${randomBytes(32).toString("hex")}` },
			signal: AbortSignal.timeout(5000),
		});
		unauthorized = response.status === 401;
	} catch {
		unauthorized = false;
	}
	check("/v1/models rejects a bad agent token with 401", unauthorized);

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
	activeFakeServer?.stop(true);
	if (failed) {
		const logs = run("podman", ["logs", gateway]);
		if (logs.status === 0 || logs.stderr === "") {
			console.error(`gateway-image smoke: podman logs ${gateway}`);
			if (logs.stdout !== "") console.error(logs.stdout.trimEnd());
			if (logs.stderr !== "") console.error(logs.stderr.trimEnd());
		}
	}
	for (const args of [
		["rm", "-f", gateway],
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
