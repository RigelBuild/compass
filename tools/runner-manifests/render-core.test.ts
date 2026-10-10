// Render tests pin operator-visible Kubernetes behavior and the privilege boundary.

import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import {
	assertRunnerImageDigest,
	assertRunnerMaxUnavailable,
	RUNNER_UID,
	type RunnerDeployValues,
	renderRunnerManifests,
} from "./render-core.ts";

const values: RunnerDeployValues = {
	namespace: "compass-runners",
	image: `registry.example.test/compass/runner@sha256:${"a".repeat(64)}`,
	nodeSelector: { key: "workload", value: "compass" },
	toleration: { key: "workload", value: "compass", effect: "NoSchedule" },
	kvmResourceName: "devices.example.test/kvm",
	seccompProfilePath: "profiles/compass-runner.json",
	sessionCapacity: 2,
	guestMemoryMiB: 1024,
	guestCpus: 2,
	runnerOverheadMiB: 512,
	hostPaths: {
		sessionVolumeRoot: "/srv/compass/sessions",
		runtimeRoot: "/srv/compass/runtime",
	},
	serverAddr: "https://compass-server.example.test:7443",
	runnerTokenSecret: { name: "compass-runner-token", key: "token" },
	maxUnavailable: 1,
	perSessionReapSeconds: 180,
	priorityClassValue: 1000000,
};

type JsonObject = Record<string, unknown>;

function object(value: unknown): JsonObject {
	if (typeof value !== "object" || value === null || Array.isArray(value)) {
		throw new Error("expected an object");
	}
	return value as JsonObject;
}

function nested(value: unknown, ...keys: string[]): unknown {
	let current = value;
	for (const key of keys) current = object(current)[key];
	return current;
}

function runnerDaemonSet(manifests: readonly unknown[]): JsonObject {
	const daemonSet = manifests.find(
		(manifest) => object(manifest).kind === "DaemonSet",
	);
	if (daemonSet === undefined) throw new Error("rendered DaemonSet is missing");
	return object(daemonSet);
}

function containers(daemonSet: JsonObject): JsonObject[] {
	const list = nested(daemonSet, "spec", "template", "spec", "containers");
	if (!Array.isArray(list)) throw new Error("expected container list");
	return list.map(object);
}

function assertPrivilegeShape(daemonSet: unknown): void {
	const podSpec = nested(daemonSet, "spec", "template", "spec");
	const runner = containers(object(daemonSet))[0];
	if (runner === undefined) throw new Error("runner container is missing");
	const security = object(runner.securityContext);

	expect(podSpec).toHaveProperty("hostUsers", false);
	expect(podSpec).toHaveProperty("hostNetwork", false);
	expect(podSpec).toHaveProperty("hostPID", false);
	expect(podSpec).toHaveProperty("hostIPC", false);
	expect(object(podSpec)).not.toHaveProperty("supplementalGroups");
	expect(security.runAsNonRoot).toBe(true);
	expect(security.runAsUser).toBe(RUNNER_UID);
	expect(security.runAsGroup).toBe(RUNNER_UID);
	expect(security.allowPrivilegeEscalation).toBe(false);
	expect(security.procMount).toBe("Unmasked");
	expect(security.appArmorProfile).toEqual({ type: "Unconfined" });
	expect(security.seccompProfile).toEqual({
		type: "Localhost",
		localhostProfile: values.seccompProfilePath,
	});
	expect(nested(security, "capabilities", "drop")).toEqual(["ALL"]);
	expect(security.privileged ?? false).toBe(false);
	expect(security).not.toHaveProperty("supplementalGroups");
	expect(runner).not.toHaveProperty("livenessProbe");

	const volumes = nested(podSpec, "volumes");
	if (!Array.isArray(volumes)) throw new Error("expected volume list");
	const hostPaths = volumes.map((volume) => object(object(volume).hostPath));
	expect(hostPaths).toHaveLength(2);
	expect(hostPaths.map((hostPath) => hostPath.type).sort()).toEqual([
		"Directory",
		"Directory",
	]);
	expect(
		hostPaths.some((hostPath) => String(hostPath.path).startsWith("/dev")),
	).toBe(false);
}

describe("renderRunnerManifests", () => {
	test("keeps the complete non-privileged user-namespace shape", () => {
		const daemonSet = runnerDaemonSet(renderRunnerManifests(values));
		assertPrivilegeShape(daemonSet);

		const podFields = ["hostUsers", "hostNetwork", "hostPID", "hostIPC"];
		for (const field of podFields) {
			const broken = structuredClone(daemonSet);
			delete object(nested(broken, "spec", "template", "spec"))[field];
			expect(() => assertPrivilegeShape(broken)).toThrow();
		}
		const privileged = structuredClone(daemonSet);
		const runner = containers(privileged)[0];
		if (runner === undefined) throw new Error("runner container is missing");
		object(runner.securityContext).privileged = true;
		expect(() => assertPrivilegeShape(privileged)).toThrow();
	});

	test("fails the privilege assertion when any required security field is removed", () => {
		const daemonSet = runnerDaemonSet(renderRunnerManifests(values));
		for (const field of [
			"runAsNonRoot",
			"runAsUser",
			"runAsGroup",
			"allowPrivilegeEscalation",
			"procMount",
			"seccompProfile",
			"appArmorProfile",
			"capabilities",
		]) {
			const copy = structuredClone(daemonSet);
			const runner = containers(copy)[0];
			if (runner === undefined) throw new Error("runner container is missing");
			delete object(runner.securityContext)[field];
			expect(() => assertPrivilegeShape(copy)).toThrow();
		}
	});

	test("sizes equal memory requests and limits from guest capacity, including one session", () => {
		const oneSession = { ...values, sessionCapacity: 1 };
		const daemonSet = runnerDaemonSet(renderRunnerManifests(oneSession));
		const runner = containers(daemonSet)[0];
		if (runner === undefined) throw new Error("runner container is missing");
		const resources = object(runner.resources);
		const requests = object(resources.requests);
		const limits = object(resources.limits);
		expect(requests.memory).toBe("1536Mi");
		expect(limits.memory).toBe(requests.memory);
		expect(requests.cpu).toBe("2");
		expect(limits).not.toHaveProperty("cpu");

		const twoSessionRunner = containers(
			runnerDaemonSet(renderRunnerManifests(values)),
		)[0];
		if (twoSessionRunner === undefined)
			throw new Error("runner container is missing");
		const twoSessionResources = object(twoSessionRunner.resources);
		expect(object(twoSessionResources.requests).memory).toBe("2560Mi");
		expect(object(twoSessionResources.requests).cpu).toBe("4");
	});

	test("mounts only the session tree and runtime tree at the Runner paths", () => {
		const daemonSet = runnerDaemonSet(renderRunnerManifests(values));
		const podSpec = object(nested(daemonSet, "spec", "template", "spec"));
		const runner = containers(daemonSet)[0];
		if (runner === undefined) throw new Error("runner container is missing");
		const volumes = podSpec.volumes;
		const mounts = runner.volumeMounts;
		if (!Array.isArray(volumes) || !Array.isArray(mounts))
			throw new Error("expected volumes and mounts");
		expect(volumes).toHaveLength(2);
		expect(mounts.map((mount) => object(mount).mountPath).sort()).toEqual([
			"/run/compass",
			"/var/lib/compass/sessions",
		]);
		const hostPaths = volumes.map((volume) => object(object(volume).hostPath));
		expect(hostPaths).toEqual([
			{ path: values.hostPaths.sessionVolumeRoot, type: "Directory" },
			{ path: values.hostPaths.runtimeRoot, type: "Directory" },
		]);
	});

	test("provisions the runtime host path and validates qualified device resources", () => {
		const daemonSet = runnerDaemonSet(renderRunnerManifests(values));
		const podSpec = nested(daemonSet, "spec", "template", "spec");
		const volumes = nested(podSpec, "volumes");
		if (!Array.isArray(volumes)) throw new Error("expected volume list");
		const runtime = volumes
			.map(object)
			.find((volume) => volume.name === "runner-runtime");
		if (runtime === undefined) throw new Error("runtime volume missing");
		expect(runtime.hostPath).toEqual({
			path: values.hostPaths.runtimeRoot,
			type: "Directory",
		});
		expect(() =>
			renderRunnerManifests({ ...values, kvmResourceName: "kvm" }),
		).toThrow();
		expect(() =>
			renderRunnerManifests({
				...values,
				kvmResourceName: "kubernetes.io/kvm",
			}),
		).toThrow();
		expect(() =>
			renderRunnerManifests({
				...values,
				kvmResourceName: "devices.example.com/kvm",
			}),
		).not.toThrow();
	});

	test("requires an HTTPS Server address for bearer tokens", () => {
		expect(() =>
			renderRunnerManifests({
				...values,
				serverAddr: "http://compass-server.example.test:7443",
			}),
		).toThrow();
	});

	test("uses fieldRef, secretKeyRef, and runtime settings consumed by compass-runner", () => {
		const daemonSet = runnerDaemonSet(renderRunnerManifests(values));
		const runner = containers(daemonSet)[0];
		if (runner === undefined || !Array.isArray(runner.env))
			throw new Error("runner env is missing");
		const env = Object.fromEntries(
			runner.env.map((entry) => [String(object(entry).name), object(entry)]),
		);
		expect(nested(env, "COMPASS_RUNNER_ID", "valueFrom")).toEqual({
			fieldRef: { fieldPath: "spec.nodeName" },
		});
		expect(nested(env, "COMPASS_RUNNER_TOKEN", "valueFrom")).toEqual({
			secretKeyRef: {
				name: values.runnerTokenSecret.name,
				key: values.runnerTokenSecret.key,
			},
		});
		expect(nested(env, "COMPASS_SERVER_ADDR", "value")).toBe(values.serverAddr);
		expect(nested(env, "COMPASS_MICROVM_RUNROOT", "value")).toBe(
			"/run/compass/microvm",
		);
		expect(nested(env, "COMPASS_MICROVM_VOLUME_ROOT", "value")).toBe(
			"/var/lib/compass/sessions",
		);
		expect(nested(env, "COMPASS_MICROVM_MEMORY_MB", "value")).toBe("1024");
		expect(nested(env, "COMPASS_MICROVM_CPUS", "value")).toBe("2");
		expect(nested(env, "COMPASS_MICROVM_QUOTA_REQUIRED", "value")).toBe("true");
	});

	test("derives termination grace from the reap budget and bounds the rollout", () => {
		const daemonSet = runnerDaemonSet(renderRunnerManifests(values));
		expect(
			nested(
				daemonSet,
				"spec",
				"template",
				"spec",
				"terminationGracePeriodSeconds",
			),
		).toBe(360);
		expect(
			nested(
				daemonSet,
				"spec",
				"updateStrategy",
				"rollingUpdate",
				"maxUnavailable",
			),
		).toBe(1);
	});

	test("rejects mutable image tags and accepts a digest reference", () => {
		expect(() =>
			assertRunnerImageDigest("registry.example.test/runner:latest"),
		).toThrow();
		expect(() =>
			assertRunnerImageDigest(
				`registry.example.test/runner:tag@sha256:${"a".repeat(64)}`,
			),
		).toThrow();
		expect(() => assertRunnerImageDigest(values.image)).not.toThrow();
	});

	test("rejects rollout concurrency outside the safe single-node bound", () => {
		expect(() => assertRunnerMaxUnavailable(0)).toThrow();
		expect(() => assertRunnerMaxUnavailable(2)).toThrow();
		expect(() => assertRunnerMaxUnavailable(1)).not.toThrow();
	});

	test("requests the operator-named KVM extended resource without a device hostPath", () => {
		const daemonSet = runnerDaemonSet(renderRunnerManifests(values));
		const runner = containers(daemonSet)[0];
		if (runner === undefined) throw new Error("runner container is missing");
		expect(nested(runner, "resources", "limits", values.kvmResourceName)).toBe(
			1,
		);
		assertPrivilegeShape(daemonSet);
	});

	test("keeps uid parity with the image and stages the required seccomp syscalls", () => {
		const dockerfile = readFileSync(
			join(import.meta.dir, "../../runner-image/Dockerfile"),
			"utf8",
		);
		expect(dockerfile).toMatch(
			new RegExp(`^USER ${RUNNER_UID}:${RUNNER_UID}$`, "m"),
		);
		const profileText = readFileSync(
			join(import.meta.dir, "seccomp/compass-runner.json"),
			"utf8",
		);
		const profile: unknown = JSON.parse(profileText);
		const syscalls = nested(profile, "syscalls");
		if (!Array.isArray(syscalls)) throw new Error("seccomp syscalls missing");
		const names = new Set(
			syscalls.flatMap((entry) => {
				const syscall = object(entry);
				return Array.isArray(syscall.names)
					? syscall.names.filter(
							(name): name is string => typeof name === "string",
						)
					: [];
			}),
		);
		for (const name of [
			"unshare",
			"mount",
			"umount2",
			"pivot_root",
			"setns",
			"clone",
			"clone3",
		]) {
			expect(names.has(name)).toBe(true);
		}
		expect(nested(profile, "defaultAction")).toMatch(
			/^SCMP_ACT_(ERRNO|KILL|TRAP)$/,
		);
		// OCI profiles have no Docker-only conditions; a runtime would drop them and allow unconditionally.
		expect(object(profile)).not.toHaveProperty("archMap");
		for (const entry of syscalls) {
			expect(object(entry)).not.toHaveProperty("includes");
			expect(object(entry)).not.toHaveProperty("excludes");
		}
		// The pod drops every capability, so capability-gated syscalls must stay denied.
		for (const name of [
			"bpf",
			"init_module",
			"reboot",
			"kexec_load",
			"chroot",
		]) {
			expect(names.has(name)).toBe(false);
		}
	});

	test("disables Kubernetes API credentials for its unused ServiceAccount", () => {
		const manifests = renderRunnerManifests(values);
		const serviceAccount = manifests.find(
			(manifest) => object(manifest).kind === "ServiceAccount",
		);
		if (serviceAccount === undefined) throw new Error("ServiceAccount missing");
		expect(object(serviceAccount).automountServiceAccountToken).toBe(false);
		expect(manifests.some((manifest) => object(manifest).kind === "Role")).toBe(
			false,
		);
	});
});
