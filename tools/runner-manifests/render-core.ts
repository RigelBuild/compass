export const DEFAULT_SECCOMP_PROFILE_PATH = "profiles/compass-runner.json";
export const RUNNER_UID = 65532;

const RUNNER_NAME = "compass-runner";
const SESSION_MOUNT_PATH = "/var/lib/compass/sessions";
const RUNTIME_MOUNT_PATH = "/run/compass";

export interface RunnerDeployValues {
	namespace: string;
	image: string;
	nodeSelector: { key: string; value: string };
	toleration: {
		key: string;
		value: string;
		effect: "NoSchedule" | "PreferNoSchedule" | "NoExecute";
	};
	kvmResourceName: string;
	seccompProfilePath?: string;
	sessionCapacity: number;
	guestMemoryMiB: number;
	guestCpus: number;
	runnerOverheadMiB: number;
	hostPaths: { sessionVolumeRoot: string; runtimeRoot: string };
	serverAddr: string;
	runnerTokenSecret: { name: string; key: string };
	maxUnavailable: number;
	perSessionReapSeconds: number;
	priorityClassValue: number;
}

type ValidatedRunnerDeployValues = Omit<
	RunnerDeployValues,
	"seccompProfilePath"
> & {
	seccompProfilePath: string;
};

export function assertRunnerImageDigest(image: string): void {
	const match = /^([A-Za-z0-9][A-Za-z0-9._:/-]*)@sha256:([a-f0-9]{64})$/.exec(
		image,
	);
	const repository = match?.[1];
	const finalComponent = repository?.split("/").at(-1);
	if (
		match === null ||
		finalComponent === undefined ||
		finalComponent.length === 0 ||
		finalComponent.includes(":")
	) {
		throw new Error(
			"runner image must be an untagged repository@sha256 digest reference",
		);
	}
}

export function assertRunnerMaxUnavailable(maxUnavailable: number): void {
	if (!Number.isSafeInteger(maxUnavailable) || maxUnavailable !== 1) {
		throw new Error("maxUnavailable must be the integer 1");
	}
}

function assertPositiveInteger(name: string, value: number): void {
	if (!Number.isSafeInteger(value) || value < 1) {
		throw new Error(`${name} must be a positive safe integer`);
	}
}

function assertNonNegativeInteger(name: string, value: number): void {
	if (!Number.isSafeInteger(value) || value < 0) {
		throw new Error(`${name} must be a non-negative safe integer`);
	}
}

function assertNonEmpty(name: string, value: string): void {
	if (value.trim().length === 0) throw new Error(`${name} must not be empty`);
}

function assertPath(name: string, value: string): void {
	assertNonEmpty(name, value);
	if (!value.startsWith("/") || value.includes("\0")) {
		throw new Error(`${name} must be an absolute path`);
	}
}

function assertNamespace(namespace: string): void {
	if (
		namespace.length > 63 ||
		!/^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$/.test(namespace)
	) {
		throw new Error("namespace must be a DNS label");
	}
}

function assertResourceName(name: string): void {
	const parts = name.split("/");
	const resource = parts.at(-1);
	const prefix = parts.length === 2 ? parts[0] : undefined;
	if (
		parts.length > 2 ||
		resource === undefined ||
		!/^[A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?$/.test(resource) ||
		(prefix !== undefined && !/^[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?$/.test(prefix))
	) {
		throw new Error(
			"kvmResourceName must be a Kubernetes extended resource name",
		);
	}
}

function validateValues(values: ValidatedRunnerDeployValues): void {
	assertNamespace(values.namespace);
	assertRunnerImageDigest(values.image);
	assertNonEmpty("nodeSelector.key", values.nodeSelector.key);
	assertNonEmpty("nodeSelector.value", values.nodeSelector.value);
	assertNonEmpty("toleration.key", values.toleration.key);
	assertNonEmpty("toleration.value", values.toleration.value);
	assertResourceName(values.kvmResourceName);
	assertNonEmpty("seccompProfilePath", values.seccompProfilePath);
	if (
		values.seccompProfilePath.startsWith("/") ||
		values.seccompProfilePath.includes("..")
	) {
		throw new Error(
			"seccompProfilePath must be relative to the kubelet seccomp root",
		);
	}
	assertPositiveInteger("sessionCapacity", values.sessionCapacity);
	assertPositiveInteger("guestMemoryMiB", values.guestMemoryMiB);
	assertPositiveInteger("guestCpus", values.guestCpus);
	assertNonNegativeInteger("runnerOverheadMiB", values.runnerOverheadMiB);
	assertPath("hostPaths.sessionVolumeRoot", values.hostPaths.sessionVolumeRoot);
	assertPath("hostPaths.runtimeRoot", values.hostPaths.runtimeRoot);
	try {
		new URL(values.serverAddr);
	} catch {
		throw new Error("serverAddr must be an absolute URL");
	}
	assertNonEmpty("runnerTokenSecret.name", values.runnerTokenSecret.name);
	assertNonEmpty("runnerTokenSecret.key", values.runnerTokenSecret.key);
	assertRunnerMaxUnavailable(values.maxUnavailable);
	assertPositiveInteger("perSessionReapSeconds", values.perSessionReapSeconds);
	if (
		!Number.isSafeInteger(values.priorityClassValue) ||
		values.priorityClassValue < 0 ||
		values.priorityClassValue > 1_000_000_000
	) {
		throw new Error("priorityClassValue must be from 0 through 1000000000");
	}
	const memoryMiB =
		values.sessionCapacity * values.guestMemoryMiB + values.runnerOverheadMiB;
	const cpuRequest = values.sessionCapacity * values.guestCpus;
	const graceSeconds = values.perSessionReapSeconds * values.sessionCapacity;
	if (
		!Number.isSafeInteger(memoryMiB) ||
		!Number.isSafeInteger(cpuRequest) ||
		!Number.isSafeInteger(graceSeconds) ||
		graceSeconds > 2_147_483_647
	) {
		throw new Error(
			"capacity-derived pod resources exceed Kubernetes integer limits",
		);
	}
}

function objectValue(value: unknown, name: string): Record<string, unknown> {
	if (typeof value !== "object" || value === null || Array.isArray(value)) {
		throw new Error(`${name} must be an object`);
	}
	return value as Record<string, unknown>;
}

function stringValue(
	record: Record<string, unknown>,
	key: string,
	name: string,
): string {
	const value = record[key];
	if (typeof value !== "string") throw new Error(`${name} must be a string`);
	return value;
}

function numberValue(
	record: Record<string, unknown>,
	key: string,
	name: string,
): number {
	const value = record[key];
	if (typeof value !== "number") throw new Error(`${name} must be a number`);
	return value;
}

export function parseRunnerDeployValues(
	input: unknown,
): ValidatedRunnerDeployValues {
	const root = objectValue(input, "values");
	const selector = objectValue(root.nodeSelector, "nodeSelector");
	const toleration = objectValue(root.toleration, "toleration");
	const hostPaths = objectValue(root.hostPaths, "hostPaths");
	const secret = objectValue(root.runnerTokenSecret, "runnerTokenSecret");
	const effect = stringValue(toleration, "effect", "toleration.effect");
	if (
		effect !== "NoSchedule" &&
		effect !== "PreferNoSchedule" &&
		effect !== "NoExecute"
	) {
		throw new Error(
			"toleration.effect must be NoSchedule, PreferNoSchedule, or NoExecute",
		);
	}
	const values: ValidatedRunnerDeployValues = {
		namespace: stringValue(root, "namespace", "namespace"),
		image: stringValue(root, "image", "image"),
		nodeSelector: {
			key: stringValue(selector, "key", "nodeSelector.key"),
			value: stringValue(selector, "value", "nodeSelector.value"),
		},
		toleration: {
			key: stringValue(toleration, "key", "toleration.key"),
			value: stringValue(toleration, "value", "toleration.value"),
			effect,
		},
		kvmResourceName: stringValue(root, "kvmResourceName", "kvmResourceName"),
		seccompProfilePath:
			root.seccompProfilePath === undefined
				? DEFAULT_SECCOMP_PROFILE_PATH
				: stringValue(root, "seccompProfilePath", "seccompProfilePath"),
		sessionCapacity: numberValue(root, "sessionCapacity", "sessionCapacity"),
		guestMemoryMiB: numberValue(root, "guestMemoryMiB", "guestMemoryMiB"),
		guestCpus: numberValue(root, "guestCpus", "guestCpus"),
		runnerOverheadMiB: numberValue(
			root,
			"runnerOverheadMiB",
			"runnerOverheadMiB",
		),
		hostPaths: {
			sessionVolumeRoot: stringValue(
				hostPaths,
				"sessionVolumeRoot",
				"hostPaths.sessionVolumeRoot",
			),
			runtimeRoot: stringValue(
				hostPaths,
				"runtimeRoot",
				"hostPaths.runtimeRoot",
			),
		},
		serverAddr: stringValue(root, "serverAddr", "serverAddr"),
		runnerTokenSecret: {
			name: stringValue(secret, "name", "runnerTokenSecret.name"),
			key: stringValue(secret, "key", "runnerTokenSecret.key"),
		},
		maxUnavailable: numberValue(root, "maxUnavailable", "maxUnavailable"),
		perSessionReapSeconds: numberValue(
			root,
			"perSessionReapSeconds",
			"perSessionReapSeconds",
		),
		priorityClassValue: numberValue(
			root,
			"priorityClassValue",
			"priorityClassValue",
		),
	};
	validateValues(values);
	return values;
}

export function renderRunnerManifests(
	input: RunnerDeployValues,
): Record<string, unknown>[] {
	const values = parseRunnerDeployValues(input);
	const labels = { "app.kubernetes.io/name": RUNNER_NAME };
	const memoryMiB =
		values.sessionCapacity * values.guestMemoryMiB + values.runnerOverheadMiB;
	const cpuRequest = values.sessionCapacity * values.guestCpus;
	const runtimePath = `${RUNTIME_MOUNT_PATH}/microvm`;

	return [
		{
			apiVersion: "v1",
			kind: "ServiceAccount",
			metadata: { name: RUNNER_NAME, namespace: values.namespace },
			automountServiceAccountToken: false,
		},
		{
			apiVersion: "scheduling.k8s.io/v1",
			kind: "PriorityClass",
			metadata: { name: RUNNER_NAME },
			value: values.priorityClassValue,
			globalDefault: false,
			description:
				"Prioritizes the node-local Compass Runner over ordinary workloads.",
		},
		{
			apiVersion: "apps/v1",
			kind: "DaemonSet",
			metadata: { name: RUNNER_NAME, namespace: values.namespace, labels },
			spec: {
				selector: { matchLabels: labels },
				updateStrategy: {
					type: "RollingUpdate",
					rollingUpdate: { maxUnavailable: values.maxUnavailable },
				},
				template: {
					metadata: { labels },
					spec: {
						hostUsers: false,
						hostNetwork: false,
						hostPID: false,
						hostIPC: false,
						serviceAccountName: RUNNER_NAME,
						priorityClassName: RUNNER_NAME,
						nodeSelector: {
							[values.nodeSelector.key]: values.nodeSelector.value,
						},
						tolerations: [
							{
								key: values.toleration.key,
								operator: "Equal",
								value: values.toleration.value,
								effect: values.toleration.effect,
							},
						],
						terminationGracePeriodSeconds:
							values.perSessionReapSeconds * values.sessionCapacity,
						containers: [
							{
								name: RUNNER_NAME,
								image: values.image,
								imagePullPolicy: "IfNotPresent",
								securityContext: {
									runAsNonRoot: true,
									runAsUser: RUNNER_UID,
									runAsGroup: RUNNER_UID,
									allowPrivilegeEscalation: false,
									capabilities: { drop: ["ALL"] },
									procMount: "Unmasked",
									seccompProfile: {
										type: "Localhost",
										localhostProfile: values.seccompProfilePath,
									},
									appArmorProfile: { type: "Unconfined" },
								},
								resources: {
									// No CPU limit avoids VMM throttling; requests still reflect guest vCPUs.
									requests: {
										memory: `${memoryMiB}Mi`,
										cpu: String(cpuRequest),
									},
									limits: {
										memory: `${memoryMiB}Mi`,
										[values.kvmResourceName]: 1,
									},
								},
								volumeMounts: [
									{ name: "session-volumes", mountPath: SESSION_MOUNT_PATH },
									{ name: "runner-runtime", mountPath: RUNTIME_MOUNT_PATH },
								],
								// No liveness probe: a pid-1 restart tears down all node sessions.
								env: [
									{
										name: "COMPASS_RUNNER_ID",
										valueFrom: { fieldRef: { fieldPath: "spec.nodeName" } },
									},
									{ name: "COMPASS_SERVER_ADDR", value: values.serverAddr },
									{
										name: "COMPASS_RUNNER_TOKEN",
										valueFrom: { secretKeyRef: values.runnerTokenSecret },
									},
									{ name: "COMPASS_MICROVM_RUNROOT", value: runtimePath },
									{
										name: "COMPASS_MICROVM_VOLUME_ROOT",
										value: SESSION_MOUNT_PATH,
									},
									{
										name: "COMPASS_MICROVM_MEMORY_MB",
										value: String(values.guestMemoryMiB),
									},
									{
										name: "COMPASS_MICROVM_CPUS",
										value: String(values.guestCpus),
									},
									{ name: "COMPASS_MICROVM_QUOTA_REQUIRED", value: "true" },
								],
							},
						],
						volumes: [
							{
								name: "session-volumes",
								hostPath: {
									path: values.hostPaths.sessionVolumeRoot,
									type: "Directory",
								},
							},
							{
								name: "runner-runtime",
								hostPath: {
									path: values.hostPaths.runtimeRoot,
									type: "DirectoryOrCreate",
								},
							},
						],
					},
				},
			},
		},
	];
}
