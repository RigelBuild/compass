import { createHash, createPublicKey } from "node:crypto";

import type { renderRunnerManifests } from "../runner-manifests/render-core.ts";
import { assertRunnerImageDigest } from "../runner-manifests/render-core.ts";

const SERVER_NAME = "compass-server";
const NATS_NAME = "compass-nats";
const POSTGRES_NAME = "compass-postgres";
const RUN_AS_UID = 65532;
const COMPASS_RUN_VOLUME = "compass-run";
const RUN_ROOT = "/run/compass";
const NATS_STORE_DIR = "/var/lib/nats";
const SERVER_LABEL = { "app.kubernetes.io/name": SERVER_NAME };
const NATS_LABEL = { "app.kubernetes.io/name": NATS_NAME };
const POSTGRES_LABEL = { "app.kubernetes.io/name": POSTGRES_NAME };
const RESOURCE_LABELS = { "app.kubernetes.io/part-of": "compass" };

type JsonObject = Record<string, unknown>;

export type RunnerClusterValues = {
	name: string;
	issuer: string;
	audience?: string;
	namespace: string;
	serviceAccount: string;
	maxTokenLifetime?: string;
	jwksURI?: string;
	jwks?: string;
	caFile?: string;
};

export type ServerDeployValues = {
	namespace: string;
	serverImage: string;
	natsImage: string;
	initImage: string;
	postgres?: { image: string; storage: string; secretName: string };
	natsStorage: string;
	tlsSecretName: string;
	dsnSecretName: string;
	natsSecretName: string;
	serverSecretsSecretName: string;
	adminTokenSecretName: string;
	s3SecretName?: string;
	corsAllowedOrigin?: string;
	otlpEndpoint?: string;
	external?: { port: number; loadBalancerClass?: string };
	runnerClusters: RunnerClusterValues[];
};

export function assertServerImageDigest(ref: string): void {
	try {
		assertRunnerImageDigest(ref);
	} catch {
		throw new Error(
			"serverImage must be an untagged repository@sha256 digest reference",
		);
	}
}

function isPlainObject(value: unknown): value is Record<string, unknown> {
	return typeof value === "object" && value !== null && !Array.isArray(value);
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
	field: string,
): string {
	const value = record[key];
	if (typeof value !== "string") throw new Error(`${field} must be a string`);
	return value;
}

function optionalStringValue(
	record: Record<string, unknown>,
	key: string,
	field: string,
): string | undefined {
	const value = record[key];
	if (value === undefined) return undefined;
	if (typeof value !== "string") throw new Error(`${field} must be a string`);
	return value;
}

function assertDNS1123(name: string, field: string, maxLength = 63): void {
	if (
		name.length > maxLength ||
		!/^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$/.test(name)
	) {
		throw new Error(
			`${field} must be a DNS-1123 name of at most ${maxLength} characters`,
		);
	}
}

function assertKubernetesQuantity(value: string, field: string): void {
	if (
		!/^[+]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:Ki|Mi|Gi|Ti|Pi|Ei|m|k|M|G|T|P|E|[eE][+-]?[0-9]+)?$/.test(
			value,
		) ||
		!/[1-9]/.test(value)
	) {
		throw new Error(`${field} must be a positive Kubernetes quantity`);
	}
}

function assertHTTPSURL(value: string, field: string): void {
	if (
		!value.startsWith("https://") ||
		/\s/.test(value) ||
		value.includes("\\") ||
		value.slice("https://".length).split(/[/?#]/, 1)[0] === ""
	) {
		throw new Error(`${field} must be an https:// URL`);
	}
	try {
		const url = new URL(value);
		if (url.protocol === "https:" && url.host.length > 0) return;
	} catch {
		// Keep one field-specific error for parse and scheme failures.
	}
	throw new Error(`${field} must be an https:// URL`);
}

function assertOnlyKeys(
	record: Record<string, unknown>,
	allowed: readonly string[],
	field: string,
): void {
	for (const key of Object.keys(record)) {
		if (!allowed.includes(key))
			throw new Error(
				`${field.length === 0 ? key : `${field}.${key}`}: unknown key`,
			);
	}
}

const GO_MAX_DURATION_NS = 2n ** 63n - 1n;
const MIN_TOKEN_LIFETIME_NS = 600n * 1_000_000_000n;
const DURATION_UNIT_NS: Record<string, bigint> = {
	ns: 1n,
	us: 1_000n,
	µs: 1_000n,
	μs: 1_000n,
	ms: 1_000_000n,
	s: 1_000_000_000n,
	m: 60_000_000_000n,
	h: 3_600_000_000_000n,
};

// Exact int64 nanoseconds, as Go's time.ParseDuration computes them, so the
// range check cannot round past what the loader accepts.
function parseGoDurationNanos(duration: string, field: string): bigint {
	const pattern = /(?:[0-9]+(?:\.[0-9]+)?)(?:ns|us|µs|μs|ms|s|m|h)/g;
	const pieces = duration.match(pattern);
	if (pieces === null || pieces.join("") !== duration) {
		throw new Error(`${field} must be a valid Go duration`);
	}
	let nanos = 0n;
	for (const piece of pieces) {
		const match = /^([0-9]+)(?:\.([0-9]+))?(ns|us|µs|μs|ms|s|m|h)$/.exec(piece);
		const whole = match?.[1];
		const unit =
			match?.[3] === undefined ? undefined : DURATION_UNIT_NS[match[3]];
		if (whole === undefined || unit === undefined) {
			throw new Error(`${field} must be a valid Go duration`);
		}
		const frac = match?.[2] ?? "";
		nanos +=
			BigInt(whole) * unit +
			(frac === "" ? 0n : (BigInt(frac) * unit) / 10n ** BigInt(frac.length));
		if (nanos > GO_MAX_DURATION_NS) {
			throw new Error(`${field} exceeds Go's maximum duration`);
		}
	}
	if (nanos < MIN_TOKEN_LIFETIME_NS) {
		throw new Error(`${field} must be at least 600s`);
	}
	return nanos;
}

const EC_CURVES = new Set(["P-256", "P-384", "P-521"]);

// Mirrors the Server's JWKS loader, which drops private, symmetric and
// incomplete keys and refuses to start when none remain.
function isUsablePublicJWK(key: unknown): boolean {
	if (!isPlainObject(key) || key.d !== undefined) return false;
	let publicJWK:
		| { kty: "RSA"; n: string; e: string }
		| { kty: "EC"; crv: string; x: string; y: string }
		| { kty: "OKP"; crv: "Ed25519"; x: string };
	switch (key.kty) {
		case "RSA": {
			const n = key.n;
			const e = key.e;
			if (
				typeof n !== "string" ||
				n === "" ||
				typeof e !== "string" ||
				e === ""
			)
				return false;
			publicJWK = { kty: "RSA", n, e };
			break;
		}
		case "EC": {
			const crv = key.crv;
			const x = key.x;
			const y = key.y;
			if (
				typeof crv !== "string" ||
				!EC_CURVES.has(crv) ||
				typeof x !== "string" ||
				x === "" ||
				typeof y !== "string" ||
				y === ""
			)
				return false;
			publicJWK = { kty: "EC", crv, x, y };
			break;
		}
		case "OKP": {
			const crv = key.crv;
			const x = key.x;
			if (crv !== "Ed25519" || typeof x !== "string" || x === "") return false;
			publicJWK = { kty: "OKP", crv, x };
			break;
		}
		default:
			return false;
	}
	try {
		const publicKey = createPublicKey({ key: publicJWK, format: "jwk" });
		if (
			publicKey.asymmetricKeyType === "rsa" &&
			publicKey.asymmetricKeyDetails?.modulusLength === 0
		)
			return false;
		return true;
	} catch {
		return false;
	}
}

function publicJWKThumbprint(key: Record<string, unknown>): string {
	let canonical: string;
	switch (key.kty) {
		case "RSA":
			canonical = JSON.stringify({ e: key.e, kty: key.kty, n: key.n });
			break;
		case "EC":
			canonical = JSON.stringify({
				crv: key.crv,
				kty: key.kty,
				x: key.x,
				y: key.y,
			});
			break;
		case "OKP":
			canonical = JSON.stringify({ crv: key.crv, kty: key.kty, x: key.x });
			break;
		default:
			throw new Error("unsupported JWK key type");
	}
	return createHash("sha256").update(canonical).digest("base64url");
}

function inlineJWSThumbprints(jwksText: string, field: string): string[] {
	let jwksValue: unknown;
	try {
		jwksValue = JSON.parse(jwksText);
	} catch {
		throw new Error(`${field} must be valid JSON with a keys array`);
	}
	const jwks = objectValue(jwksValue, field);
	if (!Array.isArray(jwks.keys)) {
		throw new Error(`${field} must be valid JSON with a keys array`);
	}
	const usableKeys = jwks.keys.filter(isUsablePublicJWK);
	if (usableKeys.length === 0)
		throw new Error(`${field} holds no usable public key`);
	return usableKeys.flatMap((key) =>
		isPlainObject(key) ? [publicJWKThumbprint(key)] : [],
	);
}

function validateRunnerCluster(
	cluster: RunnerClusterValues,
	index: number,
): string[] {
	const field = `runnerClusters[${index}]`;
	assertDNS1123(cluster.name, `${field}.name`);
	assertHTTPSURL(cluster.issuer, `${field}.issuer`);
	assertDNS1123(cluster.namespace, `${field}.namespace`);
	assertDNS1123(cluster.serviceAccount, `${field}.serviceAccount`);
	if (cluster.jwksURI !== undefined && cluster.jwks !== undefined) {
		throw new Error(`${field}.jwksURI and jwks are mutually exclusive`);
	}
	if (cluster.jwksURI !== undefined) {
		assertHTTPSURL(cluster.jwksURI, `${field}.jwksURI`);
	}
	if (cluster.maxTokenLifetime !== undefined) {
		parseGoDurationNanos(cluster.maxTokenLifetime, `${field}.maxTokenLifetime`);
	}
	return cluster.jwks === undefined
		? []
		: inlineJWSThumbprints(cluster.jwks, `${field}.jwks`);
}

function validateRunnerClusters(clusters: RunnerClusterValues[]): void {
	const names = new Set<string>();
	const issuers = new Set<string>();
	const thumbprints = new Map<string, number>();
	clusters.forEach((cluster, index) => {
		const clusterThumbprints = validateRunnerCluster(cluster, index);
		if (names.has(cluster.name)) {
			throw new Error(
				`runnerClusters[${index}].name duplicate name ${cluster.name}`,
			);
		}
		names.add(cluster.name);
		const normalizedIssuer = cluster.issuer.replace(/\/$/, "");
		if (issuers.has(normalizedIssuer)) {
			throw new Error(
				`runnerClusters[${index}].issuer is already used by another runner cluster`,
			);
		}
		issuers.add(normalizedIssuer);
		for (const thumbprint of clusterThumbprints) {
			const previousCluster = thumbprints.get(thumbprint);
			if (previousCluster !== undefined && previousCluster !== index) {
				throw new Error(
					`runnerClusters[${index}].jwks contains the same public key`,
				);
			}
			thumbprints.set(thumbprint, index);
		}
	});
}

export function parseServerDeployValues(raw: unknown): ServerDeployValues {
	const values = parseServerDeployValuesWithoutValidation(raw);
	validateServerDeployValues(values);
	return values;
}

function parseServerDeployValuesWithoutValidation(
	raw: unknown,
): ServerDeployValues {
	const root = objectValue(raw, "values");
	assertOnlyKeys(
		root,
		[
			"namespace",
			"serverImage",
			"natsImage",
			"initImage",
			"postgres",
			"natsStorage",
			"tlsSecretName",
			"dsnSecretName",
			"natsSecretName",
			"serverSecretsSecretName",
			"adminTokenSecretName",
			"s3SecretName",
			"corsAllowedOrigin",
			"otlpEndpoint",
			"external",
			"runnerClusters",
		],
		"",
	);
	const postgresInput =
		root.postgres === undefined
			? undefined
			: objectValue(root.postgres, "postgres");
	const externalInput =
		root.external === undefined
			? undefined
			: objectValue(root.external, "external");
	if (postgresInput !== undefined)
		assertOnlyKeys(
			postgresInput,
			["image", "storage", "secretName"],
			"postgres",
		);
	if (externalInput !== undefined)
		assertOnlyKeys(externalInput, ["port", "loadBalancerClass"], "external");
	const clustersRaw = root.runnerClusters;
	if (!Array.isArray(clustersRaw))
		throw new Error("runnerClusters must be an array");
	const runnerClusters: RunnerClusterValues[] = clustersRaw.map(
		(value, index) => {
			const item = objectValue(value, `runnerClusters[${index}]`);
			assertOnlyKeys(
				item,
				[
					"name",
					"issuer",
					"namespace",
					"serviceAccount",
					"audience",
					"maxTokenLifetime",
					"jwksURI",
					"jwks",
					"caFile",
				],
				`runnerClusters[${index}]`,
			);
			const cluster: RunnerClusterValues = {
				name: stringValue(item, "name", `runnerClusters[${index}].name`),
				issuer: stringValue(item, "issuer", `runnerClusters[${index}].issuer`),
				namespace: stringValue(
					item,
					"namespace",
					`runnerClusters[${index}].namespace`,
				),
				serviceAccount: stringValue(
					item,
					"serviceAccount",
					`runnerClusters[${index}].serviceAccount`,
				),
			};
			for (const key of [
				"audience",
				"maxTokenLifetime",
				"jwksURI",
				"jwks",
				"caFile",
			] as const) {
				const value = optionalStringValue(
					item,
					key,
					`runnerClusters[${index}].${key}`,
				);
				if (value !== undefined) cluster[key] = value;
			}
			return cluster;
		},
	);
	const values: ServerDeployValues = {
		namespace: stringValue(root, "namespace", "namespace"),
		serverImage: stringValue(root, "serverImage", "serverImage"),
		natsImage: stringValue(root, "natsImage", "natsImage"),
		initImage: stringValue(root, "initImage", "initImage"),
		natsStorage: stringValue(root, "natsStorage", "natsStorage"),
		tlsSecretName: stringValue(root, "tlsSecretName", "tlsSecretName"),
		dsnSecretName: stringValue(root, "dsnSecretName", "dsnSecretName"),
		natsSecretName: stringValue(root, "natsSecretName", "natsSecretName"),
		serverSecretsSecretName: stringValue(
			root,
			"serverSecretsSecretName",
			"serverSecretsSecretName",
		),
		adminTokenSecretName: stringValue(
			root,
			"adminTokenSecretName",
			"adminTokenSecretName",
		),
		runnerClusters,
	};
	for (const key of [
		"s3SecretName",
		"corsAllowedOrigin",
		"otlpEndpoint",
	] as const) {
		const value = optionalStringValue(root, key, key);
		if (value !== undefined) values[key] = value;
	}
	if (externalInput !== undefined) {
		const port = externalInput.port;
		if (
			typeof port !== "number" ||
			!Number.isSafeInteger(port) ||
			port < 1 ||
			port > 65535
		) {
			throw new Error("external.port must be an integer from 1 through 65535");
		}
		const external: NonNullable<ServerDeployValues["external"]> = { port };
		const loadBalancerClass = optionalStringValue(
			externalInput,
			"loadBalancerClass",
			"external.loadBalancerClass",
		);
		if (loadBalancerClass !== undefined)
			external.loadBalancerClass = loadBalancerClass;
		values.external = external;
	}
	if (postgresInput !== undefined) {
		values.postgres = {
			image: stringValue(postgresInput, "image", "postgres.image"),
			storage: stringValue(postgresInput, "storage", "postgres.storage"),
			secretName: stringValue(
				postgresInput,
				"secretName",
				"postgres.secretName",
			),
		};
	}
	return values;
}

function validateServerDeployValues(values: ServerDeployValues): void {
	assertDNS1123(values.namespace, "namespace");
	for (const [field, name] of [
		["tlsSecretName", values.tlsSecretName],
		["dsnSecretName", values.dsnSecretName],
		["natsSecretName", values.natsSecretName],
		["serverSecretsSecretName", values.serverSecretsSecretName],
		["adminTokenSecretName", values.adminTokenSecretName],
		...(values.s3SecretName === undefined
			? []
			: [["s3SecretName", values.s3SecretName]]),
		...(values.postgres === undefined
			? []
			: [["postgres.secretName", values.postgres.secretName]]),
	] as const)
		assertDNS1123(name, field);
	assertServerImageDigest(values.serverImage);
	for (const [field, image] of [
		["natsImage", values.natsImage],
		["initImage", values.initImage],
		...(values.postgres === undefined
			? []
			: [["postgres.image", values.postgres.image]]),
	] as const) {
		try {
			assertRunnerImageDigest(image);
		} catch {
			throw new Error(
				`${field} must be an untagged repository@sha256 digest reference`,
			);
		}
	}
	assertKubernetesQuantity(values.natsStorage, "natsStorage");
	if (values.postgres !== undefined) {
		assertKubernetesQuantity(values.postgres.storage, "postgres.storage");
	}
	validateRunnerClusters(values.runnerClusters);
}

function secretEnv(name: string, secretName: string, key: string): JsonObject {
	return { name, valueFrom: { secretKeyRef: { name: secretName, key } } };
}

function postgresNetworkPolicy(namespace: string): JsonObject {
	return {
		apiVersion: "networking.k8s.io/v1",
		kind: "NetworkPolicy",
		metadata: {
			name: `${POSTGRES_NAME}-ingress`,
			namespace,
			labels: RESOURCE_LABELS,
		},
		spec: {
			podSelector: { matchLabels: POSTGRES_LABEL },
			policyTypes: ["Ingress"],
			ingress: [
				{
					from: [{ podSelector: { matchLabels: SERVER_LABEL } }],
					ports: [{ protocol: "TCP", port: 5432 }],
				},
			],
		},
	};
}

function natsNetworkPolicy(namespace: string): JsonObject {
	return {
		apiVersion: "networking.k8s.io/v1",
		kind: "NetworkPolicy",
		metadata: {
			name: `${NATS_NAME}-ingress`,
			namespace,
			labels: RESOURCE_LABELS,
		},
		spec: {
			podSelector: { matchLabels: NATS_LABEL },
			policyTypes: ["Ingress"],
			ingress: [
				{
					from: [{ podSelector: { matchLabels: SERVER_LABEL } }],
					ports: [{ protocol: "TCP", port: 4222 }],
				},
			],
		},
	};
}

function runnerClustersYAML(clusters: RunnerClusterValues[]): string {
	return Bun.YAML.stringify({
		clusters: clusters.map((cluster) => {
			const entry: Record<string, string> = {
				name: cluster.name,
				issuer: cluster.issuer,
				namespace: cluster.namespace,
				serviceAccount: cluster.serviceAccount,
			};
			if (cluster.audience !== undefined) entry.audience = cluster.audience;
			if (cluster.maxTokenLifetime !== undefined)
				entry.maxTokenLifetime = cluster.maxTokenLifetime;
			if (cluster.jwksURI !== undefined) entry.jwksURI = cluster.jwksURI;
			else if (cluster.jwks !== undefined)
				entry.jwksFile = `/etc/compass/runner-clusters/${cluster.name}.jwks.json`;
			if (cluster.caFile !== undefined) entry.caFile = cluster.caFile;
			return entry;
		}),
	});
}
function runnerClustersConfigMapData(
	clusters: RunnerClusterValues[],
): Record<string, string> {
	const data: Record<string, string> = {
		"clusters.yaml": runnerClustersYAML(clusters),
	};
	for (const cluster of clusters) {
		if (cluster.jwks !== undefined)
			data[`${cluster.name}.jwks.json`] = cluster.jwks;
	}
	return data;
}
function runnerClustersConfigMapChecksum(data: Record<string, string>): string {
	const stableData = Object.fromEntries(
		Object.entries(data).sort(([a], [b]) => a.localeCompare(b)),
	);
	return createHash("sha256").update(JSON.stringify(stableData)).digest("hex");
}

function serverDeployment(
	values: ServerDeployValues,
	runnerClusters: Record<string, string>,
): JsonObject {
	const serverEnv: JsonObject[] = [
		secretEnv("COMPASS_DATABASE_DSN", values.dsnSecretName, "dsn"),
		secretEnv("COMPASS_NATS_URL", values.natsSecretName, "url"),
		{ name: "XDG_STATE_HOME", value: `${RUN_ROOT}/xdg-state` },
	];
	if (values.otlpEndpoint !== undefined)
		serverEnv.push({
			name: "OTEL_EXPORTER_OTLP_ENDPOINT",
			value: values.otlpEndpoint,
		});
	const args = [
		"--listen",
		"0.0.0.0:50052",
		"--tls-cert",
		"/etc/compass/tls/tls.crt",
		"--tls-key",
		"/etc/compass/tls/tls.key",
		"--state-dir",
		`${RUN_ROOT}/state`,
		"--socket",
		`${RUN_ROOT}/state/server.sock`,
		"--admin-token-file",
		`${RUN_ROOT}/bootstrap/admin-token`,
		"--runner-clusters",
		"/etc/compass/runner-clusters/clusters.yaml",
		"--secret-provider",
		"dotenv:///etc/compass/secrets/server.env",
	];
	if (values.corsAllowedOrigin !== undefined)
		args.push("--cors-allowed-origin", values.corsAllowedOrigin);
	const volumes: JsonObject[] = [
		{ name: COMPASS_RUN_VOLUME, emptyDir: {} },
		{ name: "tls", secret: { secretName: values.tlsSecretName } },
		{
			name: "server-secrets",
			secret: {
				secretName: values.serverSecretsSecretName,
				items: [{ key: "server.env", path: "server.env" }],
			},
		},
		{
			name: "admin-token",
			secret: { secretName: values.adminTokenSecretName, defaultMode: 0o440 },
		},
		{
			name: "runner-clusters",
			configMap: { name: `${SERVER_NAME}-runner-clusters` },
		},
	];
	const init: JsonObject = {
		name: "init-admin-token",
		image: values.initImage,
		imagePullPolicy: "IfNotPresent",
		command: [
			"sh",
			"-c",
			"mkdir -p /run/compass/bootstrap /run/compass/xdg-state && cp /var/run/compass/admin-token/token /run/compass/bootstrap/admin-token && chmod 0600 /run/compass/bootstrap/admin-token",
		],
		securityContext: {
			runAsNonRoot: true,
			runAsUser: RUN_AS_UID,
			runAsGroup: RUN_AS_UID,
			allowPrivilegeEscalation: false,
			readOnlyRootFilesystem: true,
			capabilities: { drop: ["ALL"] },
			seccompProfile: { type: "RuntimeDefault" },
		},
		volumeMounts: [
			{ name: COMPASS_RUN_VOLUME, mountPath: RUN_ROOT },
			{
				name: "admin-token",
				mountPath: "/var/run/compass/admin-token",
				readOnly: true,
			},
		],
	};
	const server: JsonObject = {
		name: SERVER_NAME,
		image: values.serverImage,
		imagePullPolicy: "IfNotPresent",
		args,
		securityContext: {
			allowPrivilegeEscalation: false,
			readOnlyRootFilesystem: true,
			capabilities: { drop: ["ALL"] },
		},
		ports: [{ name: "https", containerPort: 50052, protocol: "TCP" }],
		startupProbe: {
			httpGet: { path: "/healthz", port: 50052, scheme: "HTTPS" },
			periodSeconds: 5,
			failureThreshold: 30,
		},
		readinessProbe: {
			httpGet: { path: "/healthz", port: 50052, scheme: "HTTPS" },
			periodSeconds: 5,
		},
		volumeMounts: [
			{ name: COMPASS_RUN_VOLUME, mountPath: RUN_ROOT },
			{ name: "tls", mountPath: "/etc/compass/tls", readOnly: true },
			{
				name: "server-secrets",
				mountPath: "/etc/compass/secrets",
				readOnly: true,
			},
			{
				name: "runner-clusters",
				mountPath: "/etc/compass/runner-clusters",
				readOnly: true,
			},
		],
		env: serverEnv,
	};
	if (values.s3SecretName !== undefined)
		server.envFrom = [{ secretRef: { name: values.s3SecretName } }];
	return {
		apiVersion: "apps/v1",
		kind: "Deployment",
		metadata: {
			name: SERVER_NAME,
			namespace: values.namespace,
			labels: RESOURCE_LABELS,
		},
		spec: {
			replicas: 1,
			strategy: { type: "Recreate" },
			selector: { matchLabels: SERVER_LABEL },
			template: {
				metadata: {
					labels: SERVER_LABEL,
					annotations: {
						"compass.rigel.build/runner-clusters-checksum":
							runnerClustersConfigMapChecksum(runnerClusters),
					},
				},
				spec: {
					securityContext: {
						runAsNonRoot: true,
						runAsUser: RUN_AS_UID,
						runAsGroup: RUN_AS_UID,
						fsGroup: RUN_AS_UID,
						seccompProfile: { type: "RuntimeDefault" },
					},
					serviceAccountName: SERVER_NAME,
					initContainers: [init],
					containers: [server],
					volumes,
				},
			},
		},
	};
}

function serverServices(values: ServerDeployValues): JsonObject[] {
	const services: JsonObject[] = [
		{
			apiVersion: "v1",
			kind: "Service",
			metadata: {
				name: SERVER_NAME,
				namespace: values.namespace,
				labels: RESOURCE_LABELS,
			},
			spec: {
				type: "ClusterIP",
				selector: SERVER_LABEL,
				ports: [
					{ name: "https", port: 443, targetPort: 50052, protocol: "TCP" },
				],
			},
		},
	];
	if (values.external !== undefined) {
		const spec: JsonObject = {
			type: "LoadBalancer",
			selector: SERVER_LABEL,
			ports: [
				{
					name: "https",
					port: values.external.port,
					targetPort: 50052,
					protocol: "TCP",
				},
			],
		};
		if (values.external.loadBalancerClass !== undefined)
			spec.loadBalancerClass = values.external.loadBalancerClass;
		services.push({
			apiVersion: "v1",
			kind: "Service",
			metadata: {
				name: `${SERVER_NAME}-external`,
				namespace: values.namespace,
				labels: RESOURCE_LABELS,
			},
			spec,
		});
	}
	return services;
}

function natsResources(values: ServerDeployValues): JsonObject[] {
	const natsConfig = `host: "0.0.0.0"\nport: 4222\n\nhttp: "0.0.0.0:8222"\n\njetstream {\n  store_dir: "${NATS_STORE_DIR}"\n  sync_interval: "100ms"\n}\n\nauthorization {\n  user: compass\n  password: $NATS_PASSWORD\n}\n`;
	return [
		{
			apiVersion: "v1",
			kind: "ConfigMap",
			metadata: {
				name: `${NATS_NAME}-config`,
				namespace: values.namespace,
				labels: RESOURCE_LABELS,
			},
			data: { "nats.conf": natsConfig },
		},
		{
			apiVersion: "apps/v1",
			kind: "StatefulSet",
			metadata: {
				name: NATS_NAME,
				namespace: values.namespace,
				labels: RESOURCE_LABELS,
			},
			spec: {
				serviceName: NATS_NAME,
				replicas: 1,
				selector: { matchLabels: NATS_LABEL },
				template: {
					metadata: { labels: NATS_LABEL },
					spec: {
						securityContext: {
							runAsNonRoot: true,
							runAsUser: RUN_AS_UID,
							runAsGroup: RUN_AS_UID,
							fsGroup: RUN_AS_UID,
							seccompProfile: { type: "RuntimeDefault" },
						},
						containers: [
							{
								name: NATS_NAME,
								image: values.natsImage,
								args: ["--config", "/etc/nats/nats.conf"],
								ports: [
									{ name: "client", containerPort: 4222, protocol: "TCP" },
								],
								securityContext: {
									allowPrivilegeEscalation: false,
									readOnlyRootFilesystem: true,
									capabilities: { drop: ["ALL"] },
								},
								env: [
									secretEnv("NATS_PASSWORD", values.natsSecretName, "password"),
								],
								volumeMounts: [
									{
										name: "nats-config",
										mountPath: "/etc/nats",
										readOnly: true,
									},
									{ name: "nats-data", mountPath: NATS_STORE_DIR },
								],
							},
						],
						volumes: [
							{
								name: "nats-config",
								configMap: { name: `${NATS_NAME}-config` },
							},
						],
					},
				},
				volumeClaimTemplates: [
					{
						metadata: { name: "nats-data", labels: RESOURCE_LABELS },
						spec: {
							accessModes: ["ReadWriteOnce"],
							resources: { requests: { storage: values.natsStorage } },
						},
					},
				],
			},
		},
		{
			apiVersion: "v1",
			kind: "Service",
			metadata: {
				name: NATS_NAME,
				namespace: values.namespace,
				labels: RESOURCE_LABELS,
			},
			spec: {
				clusterIP: "None",
				selector: NATS_LABEL,
				ports: [
					{ name: "client", port: 4222, targetPort: 4222, protocol: "TCP" },
				],
			},
		},
	];
}

function postgresResources(values: ServerDeployValues): JsonObject[] {
	const postgres = values.postgres;
	if (postgres === undefined) return [];
	return [
		{
			apiVersion: "apps/v1",
			kind: "StatefulSet",
			metadata: {
				name: POSTGRES_NAME,
				namespace: values.namespace,
				labels: RESOURCE_LABELS,
			},
			spec: {
				serviceName: POSTGRES_NAME,
				replicas: 1,
				selector: { matchLabels: POSTGRES_LABEL },
				template: {
					metadata: { labels: POSTGRES_LABEL },
					spec: {
						securityContext: {
							runAsNonRoot: true,
							runAsUser: RUN_AS_UID,
							runAsGroup: RUN_AS_UID,
							fsGroup: RUN_AS_UID,
							seccompProfile: { type: "RuntimeDefault" },
						},
						containers: [
							{
								name: POSTGRES_NAME,
								image: postgres.image,
								ports: [
									{ name: "postgres", containerPort: 5432, protocol: "TCP" },
								],
								securityContext: {
									allowPrivilegeEscalation: false,
									readOnlyRootFilesystem: true,
									capabilities: { drop: ["ALL"] },
								},
								env: [
									{ name: "POSTGRES_USER", value: "compass" },
									secretEnv(
										"POSTGRES_PASSWORD",
										postgres.secretName,
										"password",
									),
									{
										name: "POSTGRES_INITDB_ARGS",
										value: "--auth=scram-sha-256",
									},
									{ name: "POSTGRES_HOST_AUTH_METHOD", value: "scram-sha-256" },
								],
								// Postgres 18 images keep PGDATA in a versioned child of
								// /var/lib/postgresql; the read-only root needs the socket
								// and temp dirs writable.
								volumeMounts: [
									{
										name: "postgres-data",
										mountPath: "/var/lib/postgresql",
									},
									{ name: "postgres-run", mountPath: "/var/run/postgresql" },
									{ name: "postgres-tmp", mountPath: "/tmp" },
								],
							},
						],
						volumes: [
							{ name: "postgres-run", emptyDir: {} },
							{ name: "postgres-tmp", emptyDir: {} },
						],
					},
				},
				volumeClaimTemplates: [
					{
						metadata: { name: "postgres-data", labels: RESOURCE_LABELS },
						spec: {
							accessModes: ["ReadWriteOnce"],
							resources: { requests: { storage: postgres.storage } },
						},
					},
				],
			},
		},
		{
			apiVersion: "v1",
			kind: "Service",
			metadata: {
				name: POSTGRES_NAME,
				namespace: values.namespace,
				labels: RESOURCE_LABELS,
			},
			spec: {
				clusterIP: "None",
				selector: POSTGRES_LABEL,
				ports: [
					{ name: "postgres", port: 5432, targetPort: 5432, protocol: "TCP" },
				],
			},
		},
	];
}

export function renderServerManifests(
	v: ServerDeployValues,
): ReturnType<typeof renderRunnerManifests> {
	const validated = parseServerDeployValues(v);
	const runnerClusters = runnerClustersConfigMapData(validated.runnerClusters);
	const networkPolicies = [
		natsNetworkPolicy(validated.namespace),
		...(validated.postgres === undefined
			? []
			: [postgresNetworkPolicy(validated.namespace)]),
	];
	const manifests: ReturnType<typeof renderRunnerManifests> = [
		{
			apiVersion: "v1",
			kind: "ServiceAccount",
			metadata: {
				name: SERVER_NAME,
				namespace: validated.namespace,
				labels: RESOURCE_LABELS,
			},
			automountServiceAccountToken: false,
		},
		{
			apiVersion: "v1",
			kind: "ConfigMap",
			metadata: {
				name: `${SERVER_NAME}-runner-clusters`,
				namespace: validated.namespace,
				labels: RESOURCE_LABELS,
			},
			data: runnerClusters,
		},
		serverDeployment(validated, runnerClusters),
		...serverServices(validated),
		...natsResources(validated),
		...postgresResources(validated),
		...networkPolicies,
	];
	return manifests;
}
