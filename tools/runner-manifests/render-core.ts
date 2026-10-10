export const DEFAULT_SECCOMP_PROFILE_PATH = "profiles/compass-runner.json";
export const RUNNER_UID = 65532;

const RUNNER_NAME = "compass-runner";
const SESSION_MOUNT_PATH = "/var/lib/compass/sessions";
const RUNTIME_MOUNT_PATH = "/run/compass";
const RUNNER_TOKEN_PATH = "/var/run/secrets/compass/runner/token";
const DEFAULT_TOKEN_LIFETIME_SECONDS = 600;
const DEFAULT_ADMISSION_CONTROLLERS = [
	"system:serviceaccount:kube-system:daemon-set-controller",
	"system:kube-controller-manager",
] as const;
const DEFAULT_ADMISSION_DEPLOYERS = [
	"system:serviceaccount:flux-system:kustomize-controller",
] as const;

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
	tokenExpirationSeconds?: number;
	maxTokenLifetimeSeconds?: number;
	admission?: {
		controllers?: readonly string[];
		deployers?: readonly string[];
	};
	maxUnavailable: number;
	perSessionReapSeconds: number;
	priorityClassValue: number;
}

type ValidatedRunnerDeployValues = Omit<
	RunnerDeployValues,
	| "seccompProfilePath"
	| "tokenExpirationSeconds"
	| "maxTokenLifetimeSeconds"
	| "admission"
> & {
	seccompProfilePath: string;
	tokenExpirationSeconds: number;
	maxTokenLifetimeSeconds: number;
	admission: { controllers: readonly string[]; deployers: readonly string[] };
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
	const [prefix, resource] = parts;
	const domain = prefix?.toLowerCase();
	if (
		parts.length !== 2 ||
		prefix === undefined ||
		resource === undefined ||
		!/^[A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?$/.test(resource) ||
		!/^[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?$/.test(prefix) ||
		domain === "kubernetes.io" ||
		domain.endsWith(".kubernetes.io")
	) {
		throw new Error(
			"kvmResourceName must be a qualified Kubernetes extended resource name",
		);
	}
	const domainLabels = prefix.split(".");
	if (
		domainLabels.some(
			(label) =>
				label.length === 0 ||
				label.length > 63 ||
				!/^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$/.test(label),
		)
	) {
		throw new Error("kvmResourceName must use a DNS-qualified domain");
	}
}

function validateTokenLifetimes(values: ValidatedRunnerDeployValues): void {
	assertPositiveInteger(
		"tokenExpirationSeconds",
		values.tokenExpirationSeconds,
	);
	if (values.tokenExpirationSeconds < DEFAULT_TOKEN_LIFETIME_SECONDS) {
		throw new Error("tokenExpirationSeconds must be at least 600 seconds");
	}
	assertPositiveInteger(
		"maxTokenLifetimeSeconds",
		values.maxTokenLifetimeSeconds,
	);
	if (values.maxTokenLifetimeSeconds < DEFAULT_TOKEN_LIFETIME_SECONDS) {
		throw new Error("maxTokenLifetimeSeconds must be at least 600 seconds");
	}
	if (values.tokenExpirationSeconds > values.maxTokenLifetimeSeconds) {
		throw new Error(
			"tokenExpirationSeconds must not exceed maxTokenLifetimeSeconds",
		);
	}
}

function validateAdmissionNames(
	admission: ValidatedRunnerDeployValues["admission"],
): void {
	for (const [name, entries] of Object.entries(admission)) {
		if (entries.length === 0)
			throw new Error(`admission.${name} must not be empty`);
		for (const entry of entries) {
			assertNonEmpty(`admission.${name} entry`, entry);
			if (entry.includes("'") || entry.includes("\\")) {
				throw new Error(
					`admission.${name} entries must not contain quotes or backslashes`,
				);
			}
		}
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
	validateTokenLifetimes(values);
	validateAdmissionNames(values.admission);
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
		const serverAddr = new URL(values.serverAddr);
		if (serverAddr.protocol !== "https:") {
			throw new Error("serverAddr must use HTTPS");
		}
	} catch {
		throw new Error("serverAddr must be an absolute HTTPS URL");
	}
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
	const admissionInput =
		root.admission === undefined
			? undefined
			: objectValue(root.admission, "admission");
	const controllers =
		admissionInput?.controllers === undefined
			? DEFAULT_ADMISSION_CONTROLLERS
			: admissionInput.controllers;
	const deployers =
		admissionInput?.deployers === undefined
			? DEFAULT_ADMISSION_DEPLOYERS
			: admissionInput.deployers;
	if (
		!Array.isArray(controllers) ||
		!controllers.every((item) => typeof item === "string")
	) {
		throw new Error("admission.controllers must be an array of strings");
	}
	if (
		!Array.isArray(deployers) ||
		!deployers.every((item) => typeof item === "string")
	) {
		throw new Error("admission.deployers must be an array of strings");
	}
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
		tokenExpirationSeconds:
			root.tokenExpirationSeconds === undefined
				? DEFAULT_TOKEN_LIFETIME_SECONDS
				: numberValue(root, "tokenExpirationSeconds", "tokenExpirationSeconds"),
		maxTokenLifetimeSeconds:
			root.maxTokenLifetimeSeconds === undefined
				? DEFAULT_TOKEN_LIFETIME_SECONDS
				: numberValue(
						root,
						"maxTokenLifetimeSeconds",
						"maxTokenLifetimeSeconds",
					),
		admission: { controllers, deployers },
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
				updateStrategy: {
					type: "RollingUpdate",
					rollingUpdate: { maxSurge: 0, maxUnavailable: values.maxUnavailable },
				},
				template: {
					metadata: { labels },
					spec: {
						hostUsers: false,
						hostNetwork: false,
						hostPID: false,
						hostIPC: false,
						serviceAccountName: RUNNER_NAME,
						automountServiceAccountToken: false,
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
									{
										name: "compass-runner-token",
										mountPath: "/var/run/secrets/compass/runner",
										readOnly: true,
									},
								],
								// No liveness probe: a pid-1 restart tears down all node sessions.
								env: [
									{ name: "COMPASS_SERVER_ADDR", value: values.serverAddr },
									{
										name: "COMPASS_RUNNER_TOKEN_FILE",
										value: RUNNER_TOKEN_PATH,
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
									type: "Directory",
								},
							},
							{
								name: "compass-runner-token",
								projected: {
									sources: [
										{
											serviceAccountToken: {
												audience: "compass-runner",
												expirationSeconds: values.tokenExpirationSeconds,
												path: "token",
											},
										},
									],
								},
							},
						],
					},
				},
			},
		},
		{
			apiVersion: "admissionregistration.k8s.io/v1",
			kind: "ValidatingAdmissionPolicy",
			metadata: { name: "compass-runner-identity" },
			spec: {
				failurePolicy: "Fail",
				matchConstraints: {
					namespaceSelector: {
						matchLabels: {
							"kubernetes.io/metadata.name": values.namespace,
						},
					},
					resourceRules: [
						{
							apiGroups: [""],
							apiVersions: ["v1"],
							operations: ["CREATE"],
							resources: ["pods", "serviceaccounts/token"],
						},
						{
							apiGroups: [""],
							apiVersions: ["v1"],
							operations: ["CREATE", "UPDATE"],
							resources: ["replicationcontrollers"],
						},
						{
							apiGroups: ["apps"],
							apiVersions: ["v1"],
							operations: ["CREATE", "UPDATE"],
							resources: [
								"daemonsets",
								"deployments",
								"replicasets",
								"statefulsets",
							],
						},
						{
							apiGroups: ["batch"],
							apiVersions: ["v1"],
							operations: ["CREATE", "UPDATE"],
							resources: ["jobs", "cronjobs"],
						},
					],
				},
				variables: [
					{
						name: "controllers",
						expression: `[${values.admission.controllers.map((item) => `'${item}'`).join(", ")}]`,
					},
					{
						name: "deployers",
						expression: `[${values.admission.deployers.map((item) => `'${item}'`).join(", ")}]`,
					},
					{ name: "res", expression: "request.resource.resource" },
					{
						name: "podSpec",
						expression:
							"variables.res == 'pods' ? object.spec : variables.res == 'cronjobs' ? object.spec.jobTemplate.spec.template.spec : object.spec.template.spec",
					},
					{
						name: "usesRunnerSA",
						expression:
							"has(variables.podSpec.serviceAccountName) && variables.podSpec.serviceAccountName == 'compass-runner'",
					},
					{
						name: "isRunnerDS",
						expression:
							"variables.res == 'daemonsets' && object.metadata.name == 'compass-runner'",
					},
				],
				validations: [
					{
						expression:
							"variables.res != 'serviceaccounts' || request.name != 'compass-runner' || request.userInfo.username.startsWith('system:node:')",
						message: "only kubelets may request a compass-runner token",
					},
					{
						expression:
							"variables.res != 'pods' || !variables.usesRunnerSA || (request.userInfo.username in variables.controllers && has(object.metadata.ownerReferences) && object.metadata.ownerReferences.exists(r, has(r.controller) && r.controller && r.apiVersion == 'apps/v1' && r.kind == 'DaemonSet' && r.name == 'compass-runner'))",
						message:
							"compass-runner pods must come from the compass-runner DaemonSet",
					},
					{
						expression:
							"variables.res in ['pods', 'serviceaccounts'] || variables.isRunnerDS || !variables.usesRunnerSA",
						message:
							"only the compass-runner DaemonSet may use the compass-runner ServiceAccount",
					},
					{
						expression:
							"!variables.isRunnerDS || request.userInfo.username in variables.deployers || (request.operation == 'UPDATE' && object.spec == oldObject.spec)",
						message:
							"only a deployer may create or change the compass-runner DaemonSet",
					},
				],
			},
		},
		{
			apiVersion: "admissionregistration.k8s.io/v1",
			kind: "ValidatingAdmissionPolicyBinding",
			metadata: { name: "compass-runner-identity" },
			spec: {
				policyName: "compass-runner-identity",
				validationActions: ["Deny"],
			},
		},
	];
}
