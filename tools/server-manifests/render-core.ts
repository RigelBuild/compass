import { createHash, createPublicKey } from "node:crypto";

import { assertRunnerImageDigest } from "../runner-manifests/render-core.ts";

const SERVER_NAME = "compass-server";
const NATS_NAME = "compass-nats";
const RUN_AS_UID = 65532;
const COMPASS_RUN_VOLUME = "compass-run";
const RUN_ROOT = "/run/compass";
const NATS_STORE_DIR = "/var/lib/nats";
const ADMIN_TOKEN_PATH = "/var/run/compass/admin-token/token";
const SERVER_LABEL = { "app.kubernetes.io/name": SERVER_NAME };
const NATS_LABEL = { "app.kubernetes.io/name": NATS_NAME };
const RESOURCE_LABELS = { "app.kubernetes.io/part-of": "compass" };

type JsonObject = Record<string, unknown>;

export type ServerManifest = Record<string, unknown>;

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

export type ForgeValues = {
	host?: string;
	repos?: string[];
	appId: number;
	installationId: number;
	appKeySecret: string;
	appWebhookSecret: string;
	reviewer?: {
		appId: number;
		installationId: number;
		appKeySecret: string;
	};
};

export type ServerDeployValues = {
	namespace: string;
	serverImage: string;
	natsImage: string;
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
	nodeSelector?: { key: string; value: string };
	toleration?: {
		key: string;
		value: string;
		effect: "NoSchedule" | "PreferNoSchedule" | "NoExecute";
	};
	forge?: ForgeValues;
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

function assertPositiveInteger(value: number, field: string): void {
	if (!Number.isSafeInteger(value) || value < 1)
		throw new Error(`${field} must be a positive integer`);
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

const EC_CURVE_COORDINATE_BYTES: Record<string, number> = {
	"P-256": 32,
	"P-384": 48,
	"P-521": 66,
};

const WEAK_ED25519_PUBLIC_KEYS: Record<string, true> = {
	"0000000000000000000000000000000000000000000000000000000000000000": true,
	"0000000000000000000000000000000000000000000000000000000000000080": true,
	"0100000000000000000000000000000000000000000000000000000000000000": true,
	"0100000000000000000000000000000000000000000000000000000000000080": true,
	"26e8958fc2b227b045c3f489f2ef98f0d5dfac05d3c63339b13802886d53fc05": true,
	"26e8958fc2b227b045c3f489f2ef98f0d5dfac05d3c63339b13802886d53fc85": true,
	c7176a703d4dd84fba3c0b760d10670f2a2053fa2c39ccc64ec7fd7792ac037a: true,
	c7176a703d4dd84fba3c0b760d10670f2a2053fa2c39ccc64ec7fd7792ac03fa: true,
	ecffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f: true,
	ecffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff: true,
	edffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f: true,
	edffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff: true,
	eeffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f: true,
	eeffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff: true,
};

type PublicJWK =
	| { kty: "RSA"; n: string; e: string }
	| { kty: "EC"; crv: string; x: string; y: string }
	| { kty: "OKP"; crv: "Ed25519"; x: string };

function parseRSAPublicJWK(
	key: Record<string, unknown>,
): PublicJWK | undefined {
	const { n, e } = key;
	if (typeof n !== "string" || n === "" || typeof e !== "string" || e === "") {
		return undefined;
	}
	return { kty: "RSA", n, e };
}

function parseECPublicJWK(key: Record<string, unknown>): PublicJWK | undefined {
	const { crv, x, y } = key;
	if (
		typeof crv !== "string" ||
		!Object.hasOwn(EC_CURVE_COORDINATE_BYTES, crv) ||
		typeof x !== "string" ||
		x === "" ||
		typeof y !== "string" ||
		y === ""
	)
		return undefined;
	const coordinateBytes = EC_CURVE_COORDINATE_BYTES[crv];
	if (
		coordinateBytes === undefined ||
		Buffer.from(x, "base64url").length !== coordinateBytes ||
		Buffer.from(y, "base64url").length !== coordinateBytes
	)
		return undefined;
	return { kty: "EC", crv, x, y };
}

function parseEd25519PublicJWK(
	key: Record<string, unknown>,
): PublicJWK | undefined {
	const { crv, x } = key;
	if (crv !== "Ed25519" || typeof x !== "string" || x === "") return undefined;
	const publicKeyBytes = Buffer.from(x, "base64url");
	if (
		publicKeyBytes.length !== 32 ||
		Object.hasOwn(WEAK_ED25519_PUBLIC_KEYS, publicKeyBytes.toString("hex"))
	)
		return undefined;
	return { kty: "OKP", crv, x };
}

function parsePublicJWK(key: unknown): PublicJWK | undefined {
	if (!isPlainObject(key) || key.d !== undefined) return undefined;
	switch (key.kty) {
		case "RSA":
			return parseRSAPublicJWK(key);
		case "EC":
			return parseECPublicJWK(key);
		case "OKP":
			return parseEd25519PublicJWK(key);
		default:
			return undefined;
	}
}

// Mirrors the Server's JWKS loader, which drops private, symmetric and
// incomplete keys and refuses to start when none remain.
function isUsablePublicJWK(key: unknown): boolean {
	const publicJWK = parsePublicJWK(key);
	if (publicJWK === undefined) return false;
	try {
		const publicKey = createPublicKey({ key: publicJWK, format: "jwk" });
		return !(
			publicKey.asymmetricKeyType === "rsa" &&
			publicKey.asymmetricKeyDetails?.modulusLength === 0
		);
	} catch {
		return false;
	}
}

function minimalBase64urlUint(value: unknown): string {
	if (typeof value !== "string") throw new Error("invalid RSA JWK integer");
	const bytes = Buffer.from(value, "base64url");
	let firstNonZero = 0;
	while (firstNonZero < bytes.length && bytes[firstNonZero] === 0) {
		firstNonZero++;
	}
	return bytes.subarray(firstNonZero).toString("base64url");
}

function publicJWKThumbprint(key: Record<string, unknown>): string {
	let canonical: string;
	switch (key.kty) {
		case "RSA":
			canonical = JSON.stringify({
				e: minimalBase64urlUint(key.e),
				kty: key.kty,
				n: minimalBase64urlUint(key.n),
			});
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
	assertDNS1123(cluster.name, `${field}.name`, 40);
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

function parseRunnerClusters(raw: unknown): RunnerClusterValues[] {
	if (!Array.isArray(raw)) throw new Error("runnerClusters must be an array");
	return raw.map((value, index) => {
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
			const field = `runnerClusters[${index}].${key}`;
			const value = optionalStringValue(item, key, field);
			if (value !== undefined) cluster[key] = value;
		}
		return cluster;
	});
}

function numberValue(
	record: Record<string, unknown>,
	key: string,
	field: string,
): number {
	const value = record[key];
	if (typeof value !== "number") throw new Error(`${field} must be a number`);
	return value;
}

const SERVER_SECRET_NAME_PATTERN = /^[A-Za-z_][A-Za-z0-9_]*$/;

function serverSecretNameValue(
	record: Record<string, unknown>,
	key: string,
	field: string,
): string {
	const value = stringValue(record, key, field);
	const match = SERVER_SECRET_NAME_PATTERN.exec(value);
	if (match?.[0] !== value)
		throw new Error(`${field} must match ${SERVER_SECRET_NAME_PATTERN}`);
	return value;
}

function parseForgeValues(input: unknown): ForgeValues {
	const record = objectValue(input, "forge");
	assertOnlyKeys(
		record,
		[
			"host",
			"repos",
			"appId",
			"installationId",
			"appKeySecret",
			"appWebhookSecret",
			"reviewer",
		],
		"forge",
	);
	const values: ForgeValues = {
		appId: numberValue(record, "appId", "forge.appId"),
		installationId: numberValue(
			record,
			"installationId",
			"forge.installationId",
		),
		appKeySecret: serverSecretNameValue(
			record,
			"appKeySecret",
			"forge.appKeySecret",
		),
		appWebhookSecret: serverSecretNameValue(
			record,
			"appWebhookSecret",
			"forge.appWebhookSecret",
		),
	};
	const host = optionalStringValue(record, "host", "forge.host");
	if (host !== undefined) values.host = host;
	if (record.repos !== undefined) {
		if (
			!Array.isArray(record.repos) ||
			!record.repos.every((repo) => typeof repo === "string")
		)
			throw new Error("forge.repos must be an array of strings");
		values.repos = record.repos;
	}
	if (record.reviewer !== undefined) {
		const reviewer = objectValue(record.reviewer, "forge.reviewer");
		assertOnlyKeys(
			reviewer,
			["appId", "installationId", "appKeySecret"],
			"forge.reviewer",
		);
		values.reviewer = {
			appId: numberValue(reviewer, "appId", "forge.reviewer.appId"),
			installationId: numberValue(
				reviewer,
				"installationId",
				"forge.reviewer.installationId",
			),
			appKeySecret: serverSecretNameValue(
				reviewer,
				"appKeySecret",
				"forge.reviewer.appKeySecret",
			),
		};
	}
	return values;
}

function parseServerDeployValuesWithoutValidation(
	raw: unknown,
): ServerDeployValues {
	const root = objectValue(raw, "values");
	if (Object.hasOwn(root, "postgres"))
		throw new Error(
			"postgres: removed value; Postgres must be operator-provided",
		);
	if (Object.hasOwn(root, "initImage"))
		throw new Error("initImage: removed value");
	assertOnlyKeys(
		root,
		[
			"namespace",
			"serverImage",
			"natsImage",
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
			"nodeSelector",
			"toleration",
			"forge",
			"runnerClusters",
		],
		"",
	);
	return {
		...parseRequiredServerValues(root),
		...parseOptionalServerValues(root),
	};
}

type RequiredServerDeployValues = Pick<
	ServerDeployValues,
	| "namespace"
	| "serverImage"
	| "natsImage"
	| "natsStorage"
	| "tlsSecretName"
	| "dsnSecretName"
	| "natsSecretName"
	| "serverSecretsSecretName"
	| "adminTokenSecretName"
	| "runnerClusters"
>;

function parseRequiredServerValues(
	root: Record<string, unknown>,
): RequiredServerDeployValues {
	return {
		namespace: stringValue(root, "namespace", "namespace"),
		serverImage: stringValue(root, "serverImage", "serverImage"),
		natsImage: stringValue(root, "natsImage", "natsImage"),
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
		runnerClusters: parseRunnerClusters(root.runnerClusters),
	};
}

type OptionalServerDeployValues = Pick<
	ServerDeployValues,
	| "s3SecretName"
	| "corsAllowedOrigin"
	| "otlpEndpoint"
	| "external"
	| "nodeSelector"
	| "toleration"
	| "forge"
>;

function parseOptionalServerValues(
	root: Record<string, unknown>,
): OptionalServerDeployValues {
	const values: OptionalServerDeployValues = {};
	for (const key of [
		"s3SecretName",
		"corsAllowedOrigin",
		"otlpEndpoint",
	] as const) {
		const value = optionalStringValue(root, key, key);
		if (value !== undefined) values[key] = value;
	}
	const external = parseExternal(root.external);
	const nodeSelector = parseNodeSelector(root.nodeSelector);
	const toleration = parseToleration(root.toleration);
	if (external !== undefined) values.external = external;
	if (nodeSelector !== undefined) values.nodeSelector = nodeSelector;
	if (toleration !== undefined) values.toleration = toleration;
	if (root.forge !== undefined) values.forge = parseForgeValues(root.forge);
	return values;
}

function parseExternal(input: unknown): ServerDeployValues["external"] {
	if (input === undefined) return undefined;
	const record = objectValue(input, "external");
	assertOnlyKeys(record, ["port", "loadBalancerClass"], "external");
	const port = record.port;
	if (
		typeof port !== "number" ||
		!Number.isSafeInteger(port) ||
		port < 1 ||
		port > 65535
	)
		throw new Error("external.port must be an integer from 1 through 65535");
	const external: NonNullable<ServerDeployValues["external"]> = { port };
	const loadBalancerClass = optionalStringValue(
		record,
		"loadBalancerClass",
		"external.loadBalancerClass",
	);
	if (loadBalancerClass !== undefined)
		external.loadBalancerClass = loadBalancerClass;
	return external;
}

function parseNodeSelector(input: unknown): ServerDeployValues["nodeSelector"] {
	if (input === undefined) return undefined;
	const record = objectValue(input, "nodeSelector");
	assertOnlyKeys(record, ["key", "value"], "nodeSelector");
	return {
		key: stringValue(record, "key", "nodeSelector.key"),
		value: stringValue(record, "value", "nodeSelector.value"),
	};
}

function parseToleration(input: unknown): ServerDeployValues["toleration"] {
	if (input === undefined) return undefined;
	const record = objectValue(input, "toleration");
	assertOnlyKeys(record, ["key", "value", "effect"], "toleration");
	const effect = stringValue(record, "effect", "toleration.effect");
	if (
		effect !== "NoSchedule" &&
		effect !== "PreferNoSchedule" &&
		effect !== "NoExecute"
	)
		throw new Error(
			"toleration.effect must be NoSchedule, PreferNoSchedule, or NoExecute",
		);
	return {
		key: stringValue(record, "key", "toleration.key"),
		value: stringValue(record, "value", "toleration.value"),
		effect,
	};
}
function validateImages(values: ServerDeployValues): void {
	assertServerImageDigest(values.serverImage);
	try {
		assertRunnerImageDigest(values.natsImage);
	} catch {
		throw new Error(
			"natsImage must be an untagged repository@sha256 digest reference",
		);
	}
}

function validateScheduling(values: ServerDeployValues): void {
	if (values.nodeSelector !== undefined) {
		if (values.nodeSelector.key.trim() === "")
			throw new Error("nodeSelector.key must not be empty");
		if (values.nodeSelector.value.trim() === "")
			throw new Error("nodeSelector.value must not be empty");
	}
	if (values.toleration !== undefined) {
		if (values.toleration.key.trim() === "")
			throw new Error("toleration.key must not be empty");
		if (values.toleration.value.trim() === "")
			throw new Error("toleration.value must not be empty");
	}
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
	] as const)
		assertDNS1123(name, field);
	validateImages(values);
	assertKubernetesQuantity(values.natsStorage, "natsStorage");
	validateScheduling(values);
	if (values.forge !== undefined) validateForgeValues(values.forge);
	validateRunnerClusters(values.runnerClusters);
}

function validateForgeValues(forge: ForgeValues): void {
	assertPositiveInteger(forge.appId, "forge.appId");
	assertPositiveInteger(forge.installationId, "forge.installationId");
	if (forge.reviewer === undefined) return;
	assertPositiveInteger(forge.reviewer.appId, "forge.reviewer.appId");
	assertPositiveInteger(
		forge.reviewer.installationId,
		"forge.reviewer.installationId",
	);
}

function secretEnv(name: string, secretName: string, key: string): JsonObject {
	return { name, valueFrom: { secretKeyRef: { name: secretName, key } } };
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
		ADMIN_TOKEN_PATH,
		"--runner-clusters",
		"/etc/compass/runner-clusters/clusters.yaml",
		"--secret-provider",
		"dotenv:///etc/compass/secrets/server.env",
	];
	if (values.corsAllowedOrigin !== undefined)
		args.push("--cors-allowed-origin", values.corsAllowedOrigin);
	if (values.forge !== undefined) {
		const forge = values.forge;
		if (forge.host !== undefined) args.push("--forge-host", forge.host);
		args.push("--forge-app-id", String(forge.appId));
		args.push("--forge-installation-id", String(forge.installationId));
		args.push("--forge-app-key-secret", forge.appKeySecret);
		args.push("--forge-app-webhook-secret", forge.appWebhookSecret);
		if (forge.repos !== undefined)
			args.push("--forge-repos", forge.repos.join(","));
		if (forge.reviewer !== undefined) {
			args.push("--forge-reviewer-app-id", String(forge.reviewer.appId));
			args.push(
				"--forge-reviewer-app-installation-id",
				String(forge.reviewer.installationId),
			);
			args.push("--forge-reviewer-app-key-secret", forge.reviewer.appKeySecret);
		}
	}
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
				name: "admin-token",
				mountPath: "/var/run/compass/admin-token",
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
					...(values.nodeSelector === undefined
						? {}
						: {
								nodeSelector: {
									[values.nodeSelector.key]: values.nodeSelector.value,
								},
							}),
					...(values.toleration === undefined
						? {}
						: {
								tolerations: [
									{
										key: values.toleration.key,
										operator: "Equal",
										value: values.toleration.value,
										effect: values.toleration.effect,
									},
								],
							}),
					serviceAccountName: SERVER_NAME,
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

export function renderServerManifests(v: ServerDeployValues): ServerManifest[] {
	const validated = parseServerDeployValues(v);
	const runnerClusters = runnerClustersConfigMapData(validated.runnerClusters);
	const networkPolicies = [natsNetworkPolicy(validated.namespace)];
	const manifests: ServerManifest[] = [
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
		...networkPolicies,
	];
	return manifests;
}
