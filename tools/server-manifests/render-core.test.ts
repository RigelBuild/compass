import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import {
	assertServerImageDigest,
	parseServerDeployValues,
	renderServerManifests,
	type ServerDeployValues,
} from "./render-core.ts";

const digest = (name: string) =>
	`ghcr.io/example/${name}@sha256:${"a".repeat(64)}`;
// A real RSA public key: the Server refuses JWKS entries without key material.
const TEST_JWK = {
	kty: "RSA",
	kid: "test",
	n: "v_DRFFXPuN5E_0evmokNkJOd9vOnAX-oudOpOcjMRWnCTEkZv4ZCedUGJCTtxaLB0Cag3GbYTu0JIk1Qcp6wTiFd3OFZ_2TiKivNYndAi-2qbEwIxwBZFOr-1LQZDJUK40Dt2CV_3-y_NpXG1bsdj1daUaBD1EhSlGespnszq67klxOKPC-pk3EF9ZL6NIOzUbVsorWxuUuWrC0FCZwCA47DkrEEXraDIehrLfsfrtyZLvb4StZPX2q9hXKwh_5BbWUfBe7Y6QRy13VT5KuTGfZavLKP8hcx_bsB_SU_u-94JekLO6Mhj4_iO7ZMAfnSJuU-v64gNIhFZ0fRX5Yw3w",
	e: "AQAB",
};

const PADDED_RSA_JWK = {
	...TEST_JWK,
	n: Buffer.concat([
		Buffer.from([0]),
		Buffer.from(TEST_JWK.n, "base64url"),
	]).toString("base64url"),
};

const SHORT_P256_JWK = {
	kty: "EC",
	crv: "P-256",
	x: "xlVlwWVNqy-VAUogc1Lh-cL1iv0N3qxKGNZLAcUGgg",
	y: "66ly01W5151UW-U6UDqrPG3NJFkiDbkUgtgYOZQNYk4",
};

const values: ServerDeployValues = {
	namespace: "compass",
	serverImage: digest("compass-server"),
	natsImage: digest("nats"),
	natsStorage: "8Gi",
	tlsSecretName: "compass-server-tls",
	dsnSecretName: "compass-dsn",
	natsSecretName: "compass-nats",
	serverSecretsSecretName: "compass-server-secrets",
	adminTokenSecretName: "compass-admin-token",
	runnerClusters: [
		{
			name: "local",
			issuer: "https://kubernetes.default.svc.cluster.local",
			namespace: "compass-runners",
			serviceAccount: "compass-runner",
			maxTokenLifetime: "10m",
			jwks: JSON.stringify({ keys: [TEST_JWK] }),
		},
	],
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

function manifestOf(manifests: readonly unknown[], kind: string): JsonObject {
	const manifest = manifests.find((item) => object(item).kind === kind);
	if (manifest === undefined) throw new Error(`${kind} manifest is missing`);
	return object(manifest);
}

function serverDeployment(manifests: readonly unknown[]): JsonObject {
	const deployment = manifests.find(
		(item) =>
			object(item).kind === "Deployment" &&
			object(nested(item, "metadata")).name === "compass-server",
	);
	if (deployment === undefined)
		throw new Error("compass-server Deployment is missing");
	return object(deployment);
}

function container(deployment: JsonObject, name: string): JsonObject {
	const containers = nested(
		deployment,
		"spec",
		"template",
		"spec",
		"containers",
	);
	if (!Array.isArray(containers)) throw new Error("containers are missing");
	const result = containers.find((item) => object(item).name === name);
	if (result === undefined) throw new Error(`${name} container is missing`);
	return object(result);
}

function serializedManifests(manifests: readonly unknown[]): string {
	return `${manifests
		.map((manifest) =>
			Bun.YAML.stringify(JSON.parse(JSON.stringify(manifest)), null, 2),
		)
		.join("\n---\n")}\n`;
}

describe("renderServerManifests", () => {
	test("matches the example values golden output", () => {
		const example: unknown = JSON.parse(
			readFileSync(join(import.meta.dir, "values.example.json"), "utf8"),
		);
		const expected = readFileSync(join(import.meta.dir, "golden.yaml"), "utf8");
		expect(
			serializedManifests(
				renderServerManifests(parseServerDeployValues(example)),
			),
		).toBe(expected);
	});

	test("renders a singleton Recreate deployment without init containers", () => {
		const manifests = renderServerManifests(values);
		const deployment = serverDeployment(manifests);
		const podSpec = object(nested(deployment, "spec", "template", "spec"));
		const server = container(deployment, "compass-server");
		expect(
			object(nested(deployment, "spec", "template", "metadata", "annotations"))[
				"compass.rigel.build/runner-clusters-checksum"
			],
		).toMatch(/^[a-f0-9]{64}$/);
		expect(nested(deployment, "spec", "replicas")).toBe(1);
		expect(nested(deployment, "spec", "strategy", "type")).toBe("Recreate");
		expect(podSpec.securityContext).toEqual({
			runAsNonRoot: true,
			runAsUser: 65532,
			runAsGroup: 65532,
			fsGroup: 65532,
			seccompProfile: { type: "RuntimeDefault" },
		});
		expect(server.securityContext).toEqual({
			allowPrivilegeEscalation: false,
			readOnlyRootFilesystem: true,
			capabilities: { drop: ["ALL"] },
		});
		expect(podSpec).not.toHaveProperty("initContainers");
		expect(server).not.toHaveProperty("livenessProbe");
		expect(nested(server, "startupProbe", "httpGet")).toEqual({
			path: "/healthz",
			port: 50052,
			scheme: "HTTPS",
		});
		expect(nested(server, "readinessProbe", "httpGet")).toEqual({
			path: "/healthz",
			port: 50052,
			scheme: "HTTPS",
		});
	});
	test("rejects duplicate RSA keys with a leading zero in the modulus", () => {
		const cluster = values.runnerClusters[0];
		if (cluster === undefined) throw new Error("cluster test value missing");
		const jwks = (key: object) => JSON.stringify({ keys: [key] });
		expect(() =>
			parseServerDeployValues({
				...values,
				runnerClusters: [
					{ ...cluster, jwks: jwks(TEST_JWK) },
					{
						...cluster,
						name: "other",
						issuer: "https://other.example.test",
						jwks: jwks(PADDED_RSA_JWK),
					},
				],
			}),
		).toThrow("same public key");
	});
	test("rejects unknown root and nested runner-cluster keys with full paths", () => {
		const cluster = values.runnerClusters[0];
		if (cluster === undefined) throw new Error("cluster test value missing");
		expect(() =>
			parseServerDeployValues({
				...values,
				runerClusters: values.runnerClusters,
			}),
		).toThrow("runerClusters: unknown key");
		expect(() =>
			parseServerDeployValues({
				...values,
				runnerClusters: [
					{ ...cluster, jwksUri: "https://issuer.example.test/keys" },
				],
			}),
		).toThrow("runnerClusters[0].jwksUri: unknown key");
		expect(() =>
			parseServerDeployValues({
				...values,
				external: { port: 7443, typo: true },
			}),
		).toThrow("external.typo: unknown key");
	});

	test("rejects HTTPS URL forms not accepted by Go", () => {
		const cluster = values.runnerClusters[0];
		if (cluster === undefined) throw new Error("cluster test value missing");
		for (const issuer of [
			" https://issuer.example.test",
			"https://issuer.example.test ",
			"https:issuer.example.test",
			"https://issuer\\example.test",
			"https:///path-only",
		]) {
			expect(() =>
				parseServerDeployValues({
					...values,
					runnerClusters: [{ ...cluster, issuer }],
				}),
			).toThrow("runnerClusters[0].issuer");
		}
	});

	test("requires runner-cluster names to be DNS labels of at most 40 characters", () => {
		const cluster = values.runnerClusters[0];
		if (cluster === undefined) throw new Error("cluster test value missing");
		for (const name of ["-leading", "trailing-", "A-upper", "a".repeat(41)]) {
			expect(() =>
				parseServerDeployValues({
					...values,
					runnerClusters: [{ ...cluster, name }],
				}),
			).toThrow("runnerClusters[0].name");
		}
		expect(() =>
			parseServerDeployValues({
				...values,
				runnerClusters: [{ ...cluster, name: "a".repeat(40) }],
			}),
		).not.toThrow();
	});

	test("changes the Server pod checksum when runner-cluster ConfigMap data changes", () => {
		const checksum = (input: ServerDeployValues) => {
			const deployment = serverDeployment(renderServerManifests(input));
			const annotations = nested(
				deployment,
				"spec",
				"template",
				"metadata",
				"annotations",
			);
			return object(annotations)[
				"compass.rigel.build/runner-clusters-checksum"
			];
		};
		const first = checksum(values);
		const unchanged = checksum(values);
		const changedValues = structuredClone(values);
		const cluster = changedValues.runnerClusters[0];
		if (cluster === undefined || cluster.jwks === undefined)
			throw new Error("inline JWKS test value missing");
		const jwks = JSON.parse(cluster.jwks) as { keys: { kid?: string }[] };
		const key = jwks.keys[0];
		if (key === undefined) throw new Error("inline JWK missing");
		key.kid = "changed";
		cluster.jwks = JSON.stringify(jwks);
		expect(first).toEqual(expect.any(String));
		expect(unchanged).toBe(first);
		expect(checksum(changedValues)).not.toBe(first);
	});

	test("mounts the admin token directly for the Server", () => {
		const deployment = serverDeployment(renderServerManifests(values));
		const pod = object(nested(deployment, "spec", "template", "spec"));
		const volumes = pod.volumes;
		if (!Array.isArray(volumes)) throw new Error("pod volumes missing");
		const token = volumes
			.map(object)
			.find((volume) => volume.name === "admin-token");
		expect(token).toEqual({
			name: "admin-token",
			secret: { secretName: values.adminTokenSecretName, defaultMode: 0o440 },
		});
		const server = container(deployment, "compass-server");
		expect(server.volumeMounts).toContainEqual({
			name: "admin-token",
			mountPath: "/var/run/compass/admin-token",
			readOnly: true,
		});
		expect(server.args).toContain("/var/run/compass/admin-token/token");
	});

	test("keeps Server state, socket, bootstrap, and XDG state on one child-mounted emptyDir", () => {
		const deployment = serverDeployment(renderServerManifests(values));
		const pod = object(nested(deployment, "spec", "template", "spec"));
		const server = container(deployment, "compass-server");
		const mounts = server.volumeMounts;
		if (!Array.isArray(mounts)) throw new Error("server mounts are missing");
		const stateMount = mounts
			.map(object)
			.find((mount) => mount.name === "compass-run");
		expect(stateMount).toEqual({
			name: "compass-run",
			mountPath: "/run/compass",
		});
		expect(nested(server, "env")).toContainEqual({
			name: "XDG_STATE_HOME",
			value: "/run/compass/xdg-state",
		});
		expect(server.args).toEqual([
			"--listen",
			"0.0.0.0:50052",
			"--tls-cert",
			"/etc/compass/tls/tls.crt",
			"--tls-key",
			"/etc/compass/tls/tls.key",
			"--state-dir",
			"/run/compass/state",
			"--socket",
			"/run/compass/state/server.sock",
			"--admin-token-file",
			"/var/run/compass/admin-token/token",
			"--runner-clusters",
			"/etc/compass/runner-clusters/clusters.yaml",
			"--secret-provider",
			"dotenv:///etc/compass/secrets/server.env",
		]);
		const volumes = pod.volumes;
		if (!Array.isArray(volumes)) throw new Error("pod volumes missing");
		expect(
			volumes.map(object).filter((volume) => volume.emptyDir !== undefined),
		).toEqual([{ name: "compass-run", emptyDir: {} }]);
		const args = server.args as string[];
		const stateDir = args[args.indexOf("--state-dir") + 1];
		expect(stateDir?.startsWith(`${stateMount?.mountPath}/`)).toBe(true);
	});

	test("wires secrets by key without rendering Secret objects or values", () => {
		const manifests = renderServerManifests(values);
		const server = container(serverDeployment(manifests), "compass-server");
		expect(server.env).toEqual(
			expect.arrayContaining([
				{
					name: "COMPASS_DATABASE_DSN",
					valueFrom: {
						secretKeyRef: { name: values.dsnSecretName, key: "dsn" },
					},
				},
				{
					name: "COMPASS_NATS_URL",
					valueFrom: {
						secretKeyRef: { name: values.natsSecretName, key: "url" },
					},
				},
			]),
		);
		expect(
			manifests.some((manifest) => object(manifest).kind === "Secret"),
		).toBe(false);
		expect(serializedManifests(manifests)).not.toContain("MASTER_KEY");
		expect(server.envFrom).toBeUndefined();
	});

	test("adds optional S3 envFrom, CORS argument, and OTLP endpoint", () => {
		const withOptions = {
			...values,
			s3SecretName: "compass-s3",
			corsAllowedOrigin: "https://console.example.test",
			otlpEndpoint: "http://otel.example.test:4318",
		};
		const server = container(
			serverDeployment(renderServerManifests(withOptions)),
			"compass-server",
		);
		expect(server.envFrom).toEqual([{ secretRef: { name: "compass-s3" } }]);
		expect(server.args).toContain("--cors-allowed-origin");
		expect(server.args).toContain("https://console.example.test");
		expect(server.env).toContainEqual({
			name: "OTEL_EXPORTER_OTLP_ENDPOINT",
			value: "http://otel.example.test:4318",
		});
		expect(
			container(
				serverDeployment(renderServerManifests(values)),
				"compass-server",
			).args,
		).not.toContain("--cors-allowed-origin");
	});

	test("mounts TLS and server.env read-only and configures the Server service", () => {
		const manifests = renderServerManifests(values);
		const server = container(serverDeployment(manifests), "compass-server");
		expect(server.volumeMounts).toEqual(
			expect.arrayContaining([
				{ name: "tls", mountPath: "/etc/compass/tls", readOnly: true },
				{
					name: "server-secrets",
					mountPath: "/etc/compass/secrets",
					readOnly: true,
				},
			]),
		);
		const service = manifests
			.map(object)
			.find(
				(item) =>
					item.kind === "Service" &&
					object(item.metadata).name === "compass-server",
			);
		expect(service).toMatchObject({
			spec: { type: "ClusterIP", ports: [{ port: 443, targetPort: 50052 }] },
		});
	});

	test("renders the optional external LoadBalancer service", () => {
		const manifests = renderServerManifests({
			...values,
			external: { port: 7443, loadBalancerClass: "example.test/external" },
		});
		const service = manifests
			.map(object)
			.find(
				(item) =>
					item.kind === "Service" &&
					object(item.metadata).name === "compass-server-external",
			);
		expect(service).toMatchObject({
			spec: {
				type: "LoadBalancer",
				loadBalancerClass: "example.test/external",
				ports: [{ port: 7443, targetPort: 50052 }],
			},
		});
	});

	test("renders NATS JetStream config, credentials, persistent storage, and ingress restriction", () => {
		const manifests = renderServerManifests(values);
		const natsStatefulSet = manifests
			.map(object)
			.find(
				(item) =>
					item.kind === "StatefulSet" &&
					object(item.metadata).name === "compass-nats",
			);
		expect(natsStatefulSet).toBeDefined();
		const natsContainer = object(
			(
				object(nested(natsStatefulSet, "spec", "template", "spec"))
					.containers as unknown[]
			)[0],
		);
		expect(natsContainer.securityContext).toEqual({
			allowPrivilegeEscalation: false,
			readOnlyRootFilesystem: true,
			capabilities: { drop: ["ALL"] },
		});
		expect(natsContainer.env).toContainEqual({
			name: "NATS_PASSWORD",
			valueFrom: {
				secretKeyRef: { name: values.natsSecretName, key: "password" },
			},
		});
		const config = manifests
			.map(object)
			.find(
				(item) =>
					item.kind === "ConfigMap" &&
					object(item.metadata).name === "compass-nats-config",
			);
		expect(nested(config, "data", "nats.conf")).toContain(
			'store_dir: "/var/lib/nats"',
		);
		expect(nested(config, "data", "nats.conf")).toContain(
			'sync_interval: "100ms"',
		);
		expect(nested(config, "data", "nats.conf")).toContain(
			'http: "0.0.0.0:8222"',
		);
		expect(nested(config, "data", "nats.conf")).toContain(
			"password: $NATS_PASSWORD",
		);
		const natsPolicy = manifests
			.map(object)
			.find(
				(item) =>
					item.kind === "NetworkPolicy" &&
					object(nested(item, "spec", "podSelector", "matchLabels"))[
						"app.kubernetes.io/name"
					] === "compass-nats",
			);
		expect(natsPolicy).toBeDefined();
		expect(nested(natsPolicy, "spec", "ingress")).toEqual([
			{
				from: [
					{
						podSelector: {
							matchLabels: { "app.kubernetes.io/name": "compass-server" },
						},
					},
				],
				ports: [{ protocol: "TCP", port: 4222 }],
			},
		]);
	});

	test("rejects removed Postgres and initImage values", () => {
		const manifests = renderServerManifests(values);
		expect(
			manifests.some((item) =>
				String(nested(item, "metadata", "name")).startsWith("compass-postgres"),
			),
		).toBe(false);
		expect(() =>
			parseServerDeployValues({
				...values,
				postgres: {
					image: digest("postgres"),
					storage: "20Gi",
					secretName: "compass-postgres",
				},
			}),
		).toThrow("postgres");
		expect(() =>
			parseServerDeployValues({ ...values, initImage: digest("busybox") }),
		).toThrow("initImage");
	});

	test("renders only the required NATS ingress policy", () => {
		const policies = renderServerManifests(values).filter(
			(item) => object(item).kind === "NetworkPolicy",
		);
		expect(policies).toHaveLength(1);
		expect(object(policies[0]).metadata).toMatchObject({
			name: "compass-nats-ingress",
		});
	});

	test("rejects JWKS configured with a URI", () => {
		const cluster = values.runnerClusters[0];
		if (cluster === undefined || cluster.jwks === undefined)
			throw new Error("inline JWKS test value missing");
		expect(() =>
			parseServerDeployValues({
				...values,
				runnerClusters: [
					{ ...cluster, jwksURI: "https://issuer.example.test/keys" },
				],
			}),
		).toThrow("jwksURI and jwks");
	});
	test("renders JWKS data and maps jwks to the Server's matching jwksFile", () => {
		const configMap = manifestOf(renderServerManifests(values), "ConfigMap");
		const data = object(configMap.data);
		expect(data["local.jwks.json"]).toBe(values.runnerClusters[0]?.jwks);
		const clusterFile = Bun.YAML.parse(String(data["clusters.yaml"]));
		expect(clusterFile).toEqual({
			clusters: [
				{
					name: "local",
					issuer: "https://kubernetes.default.svc.cluster.local",
					namespace: "compass-runners",
					serviceAccount: "compass-runner",
					maxTokenLifetime: "10m",
					jwksFile: "/etc/compass/runner-clusters/local.jwks.json",
				},
			],
		});
	});

	test("preserves optional Runner cluster fields and supports JWKS URI discovery", () => {
		const manifests = renderServerManifests({
			...values,
			runnerClusters: [
				{
					name: "remote",
					issuer: "https://issuer.example.test/",
					namespace: "compass-runners",
					serviceAccount: "compass-runner",
					jwksURI: "https://issuer.example.test/keys",
					audience: "compass-runner",
					caFile: "/etc/ssl/certs/ca.pem",
				},
			],
		});
		const data = object(manifestOf(manifests, "ConfigMap").data);
		expect(Bun.YAML.parse(String(data["clusters.yaml"]))).toEqual({
			clusters: [
				{
					name: "remote",
					issuer: "https://issuer.example.test/",
					audience: "compass-runner",
					namespace: "compass-runners",
					serviceAccount: "compass-runner",
					jwksURI: "https://issuer.example.test/keys",
					caFile: "/etc/ssl/certs/ca.pem",
				},
			],
		});
	});

	test("rejects malformed RSA and EC public key material", () => {
		const cluster = values.runnerClusters[0];
		if (cluster === undefined) throw new Error("cluster test value missing");
		for (const jwk of [
			{ ...TEST_JWK, n: "!!" },
			{ kty: "EC", crv: "P-256", x: "!!", y: "!!" },
		]) {
			expect(() =>
				parseServerDeployValues({
					...values,
					runnerClusters: [
						{ ...cluster, jwks: JSON.stringify({ keys: [jwk] }) },
					],
				}),
			).toThrow("usable public key");
		}
	});
	test("rejects EC coordinates with widths below the named curve size", () => {
		const cluster = values.runnerClusters[0];
		if (cluster === undefined) throw new Error("cluster test value missing");
		expect(() =>
			parseServerDeployValues({
				...values,
				runnerClusters: [
					{ ...cluster, jwks: JSON.stringify({ keys: [SHORT_P256_JWK] }) },
				],
			}),
		).toThrow("usable public key");
	});

	test("rejects weak Ed25519 public keys", () => {
		const cluster = values.runnerClusters[0];
		if (cluster === undefined) throw new Error("cluster test value missing");
		const weakKeys = [
			Buffer.alloc(32).toString("base64url"),
			Buffer.concat([Buffer.alloc(31), Buffer.from([0x80])]).toString(
				"base64url",
			),
		];
		for (const x of weakKeys) {
			expect(() =>
				parseServerDeployValues({
					...values,
					runnerClusters: [
						{
							...cluster,
							jwks: JSON.stringify({
								keys: [{ kty: "OKP", crv: "Ed25519", x }],
							}),
						},
					],
				}),
			).toThrow("usable public key");
		}
	});

	test("rejects Ed25519 public keys with incorrect widths", () => {
		const cluster = values.runnerClusters[0];
		if (cluster === undefined) throw new Error("cluster test value missing");
		for (const x of [
			Buffer.alloc(31).toString("base64url"),
			Buffer.alloc(33).toString("base64url"),
		]) {
			expect(() =>
				parseServerDeployValues({
					...values,
					runnerClusters: [
						{
							...cluster,
							jwks: JSON.stringify({
								keys: [{ kty: "OKP", crv: "Ed25519", x }],
							}),
						},
					],
				}),
			).toThrow("usable public key");
		}
	});

	test("rejects mutable and tagged digest images", () => {
		for (const field of ["serverImage", "natsImage"] as const) {
			for (const ref of [
				"example.test/image:latest",
				`example.test/image:latest@sha256:${"a".repeat(64)}`,
			]) {
				expect(() =>
					parseServerDeployValues({ ...values, [field]: ref }),
				).toThrow(field);
			}
		}
		expect(() =>
			assertServerImageDigest("registry.example.test/server:latest"),
		).toThrow();
	});

	test("renders optional forge args and validates dotenv secret names and ids", () => {
		const configured = renderServerManifests({
			...values,
			forge: {
				host: "github.example.test",
				repos: ["rigelbuild/compass"],
				appId: 123,
				installationId: 456,
				appKeySecret: "FORGE_APP_PRIVATE_KEY",
				appWebhookSecret: "FORGE_APP_WEBHOOK_SECRET",
				reviewer: {
					appId: 789,
					installationId: 987,
					appKeySecret: "FORGE_REVIEWER_APP_PRIVATE_KEY",
				},
			},
		});
		const server = container(serverDeployment(configured), "compass-server");
		expect(server.args).toEqual(
			expect.arrayContaining([
				"--forge-host",
				"github.example.test",
				"--forge-app-id",
				"123",
				"--forge-installation-id",
				"456",
				"--forge-app-key-secret",
				"FORGE_APP_PRIVATE_KEY",
				"--forge-app-webhook-secret",
				"FORGE_APP_WEBHOOK_SECRET",
				"--forge-repos",
				"rigelbuild/compass",
				"--forge-reviewer-app-id",
				"789",
				"--forge-reviewer-app-installation-id",
				"987",
				"--forge-reviewer-app-key-secret",
				"FORGE_REVIEWER_APP_PRIVATE_KEY",
			]),
		);
		const minimalForge = container(
			serverDeployment(
				renderServerManifests({
					...values,
					forge: {
						appId: 123,
						installationId: 456,
						appKeySecret: "FORGE_APP_PRIVATE_KEY",
						appWebhookSecret: "FORGE_APP_WEBHOOK_SECRET",
					},
				}),
			),
			"compass-server",
		);
		const minimalArgs = object(minimalForge).args;
		if (!Array.isArray(minimalArgs)) throw new Error("server args are missing");
		expect(minimalArgs).not.toContain("--forge-host");
		expect(minimalArgs).not.toContain("--forge-repos");
		expect(
			minimalArgs.some((arg) => String(arg).startsWith("--forge-reviewer-")),
		).toBe(false);
		for (const [field, id] of [
			["appId", 0],
			["installationId", -1],
			["appId", 1.5],
			["installationId", 1.5],
		] as const) {
			expect(() =>
				parseServerDeployValues({
					...values,
					forge: {
						appId: 123,
						installationId: 456,
						appKeySecret: "FORGE_APP_PRIVATE_KEY",
						appWebhookSecret: "FORGE_APP_WEBHOOK_SECRET",
						[field]: id,
					},
				}),
			).toThrow(`forge.${field}`);
		}
		for (const [field, id] of [
			["appId", 0],
			["installationId", -1],
			["appId", 1.5],
			["installationId", 1.5],
		] as const) {
			expect(() =>
				parseServerDeployValues({
					...values,
					forge: {
						appId: 123,
						installationId: 456,
						appKeySecret: "FORGE_APP_PRIVATE_KEY",
						appWebhookSecret: "FORGE_APP_WEBHOOK_SECRET",
						reviewer: {
							appId: 789,
							installationId: 987,
							appKeySecret: "FORGE_REVIEWER_APP_PRIVATE_KEY",
							[field]: id,
						},
					},
				}),
			).toThrow(`forge.reviewer.${field}`);
		}
		expect(() =>
			parseServerDeployValues({
				...values,
				forge: {
					appId: 123,
					installationId: 456,
					appKeySecret: "forge-app-key",
					appWebhookSecret: "FORGE_APP_WEBHOOK_SECRET",
				},
			}),
		).toThrow("forge.appKeySecret");
	});

	test("rejects empty optional scheduling values", () => {
		expect(() =>
			parseServerDeployValues({
				...values,
				nodeSelector: { key: "", value: "" },
			}),
		).toThrow("nodeSelector.key");
		expect(() =>
			parseServerDeployValues({
				...values,
				toleration: { key: "", value: "", effect: "NoSchedule" },
			}),
		).toThrow("toleration.key");
	});

	test("renders optional scheduling values like the Runner", () => {
		const scheduled = renderServerManifests({
			...values,
			nodeSelector: { key: "node-role", value: "server" },
			toleration: { key: "dedicated", value: "server", effect: "NoSchedule" },
		});
		const podSpec = nested(
			serverDeployment(scheduled),
			"spec",
			"template",
			"spec",
		);
		expect(podSpec).toMatchObject({
			nodeSelector: { "node-role": "server" },
			tolerations: [
				{
					key: "dedicated",
					operator: "Equal",
					value: "server",
					effect: "NoSchedule",
				},
			],
		});
		const unscheduled = nested(
			serverDeployment(renderServerManifests(values)),
			"spec",
			"template",
			"spec",
		);
		expect(object(unscheduled)).not.toHaveProperty("nodeSelector");
		expect(object(unscheduled)).not.toHaveProperty("tolerations");
	});

	test("validates external ports and DNS-1123 names", () => {
		for (const port of [0, 65536, 1.5, Number.NaN]) {
			expect(() =>
				parseServerDeployValues({ ...values, external: { port } }),
			).toThrow("external.port");
		}
		for (const name of ["Bad_Name", "-bad", "bad-", "a".repeat(64)]) {
			expect(() =>
				parseServerDeployValues({ ...values, dsnSecretName: name }),
			).toThrow("dsnSecretName");
		}
		expect(() =>
			parseServerDeployValues({ ...values, namespace: "Bad_Namespace" }),
		).toThrow("namespace");
	});

	test("rejects conflicting or malformed runner-cluster JWKS, names, issuers, and lifetime", () => {
		const cluster = values.runnerClusters[0];
		if (cluster === undefined) throw new Error("cluster test value missing");
		const rejectCluster = (update: object) =>
			parseServerDeployValues({
				...values,
				runnerClusters: [{ ...cluster, ...update }],
			});
		expect(() =>
			rejectCluster({ jwksURI: "https://issuer.example.test/keys" }),
		).toThrow("jwksURI and jwks");
		expect(() => rejectCluster({ jwks: "not json" })).toThrow(
			"runnerClusters[0].jwks",
		);
		expect(() => rejectCluster({ jwks: JSON.stringify({ keys: {} }) })).toThrow(
			"runnerClusters[0].jwks",
		);
		expect(() => rejectCluster({ jwks: JSON.stringify({ keys: [] }) })).toThrow(
			"usable public key",
		);
		expect(() =>
			rejectCluster({
				jwks: JSON.stringify({ keys: [{ kty: "RSA", kid: "no-material" }] }),
			}),
		).toThrow("usable public key");
		expect(() =>
			rejectCluster({
				jwks: JSON.stringify({ keys: [{ kty: "oct", k: "c2VjcmV0" }] }),
			}),
		).toThrow("usable public key");
		expect(() =>
			rejectCluster({
				jwks: JSON.stringify({
					keys: [{ kty: "RSA", n: "AQAB", e: "AQAB", d: "AQAB" }],
				}),
			}),
		).toThrow("usable public key");
		expect(() => rejectCluster({ name: "Invalid_Name" })).toThrow(
			"runnerClusters[0].name",
		);
		expect(() => rejectCluster({ maxTokenLifetime: "599s" })).toThrow(
			"maxTokenLifetime",
		);
		expect(() => rejectCluster({ maxTokenLifetime: "3000000h" })).toThrow(
			"maximum duration",
		);
		expect(() =>
			rejectCluster({ maxTokenLifetime: "9223372036854775808ns" }),
		).toThrow("maximum duration");
		expect(() =>
			rejectCluster({ maxTokenLifetime: "2562047h47m16.854775808s" }),
		).toThrow("maximum duration");
		for (const max of ["9223372036854775807ns", "2562047h47m16.854775807s"]) {
			expect(() => rejectCluster({ maxTokenLifetime: max })).not.toThrow();
		}
		expect(() => rejectCluster({ maxTokenLifetime: "not a duration" })).toThrow(
			"maxTokenLifetime",
		);
		expect(() =>
			parseServerDeployValues({
				...values,
				runnerClusters: [
					cluster,
					{
						...cluster,
						name: cluster.name,
						issuer: "https://other.example.test",
					},
				],
			}),
		).toThrow("duplicate name");
		expect(() =>
			parseServerDeployValues({
				...values,
				runnerClusters: [cluster, { ...cluster, name: "other" }],
			}),
		).toThrow("issuer");
	});

	test("applies common labels and namespace to every namespaced object", () => {
		const manifests = renderServerManifests(values);
		for (const manifest of manifests) {
			const metadata = object(object(manifest).metadata);
			expect(metadata.labels).toEqual({
				"app.kubernetes.io/part-of": "compass",
			});
			const kind = object(manifest).kind;
			const clusterScopedKinds = ["Namespace", "CustomResourceDefinition"];
			if (clusterScopedKinds.includes(String(kind))) {
				continue;
			}
			if (metadata.namespace === undefined)
				throw new Error(`${String(kind)} is missing its namespace`);
			expect(metadata.namespace).toBe("compass");
		}
	});
});
