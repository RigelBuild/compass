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
const values: ServerDeployValues = {
	namespace: "compass",
	serverImage: digest("compass-server"),
	natsImage: digest("nats"),
	initImage: digest("busybox"),
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
			jwks: JSON.stringify({ keys: [{ kty: "RSA", kid: "test" }] }),
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

const serverValuesWithoutPostgres: ServerDeployValues = structuredClone(values);

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

	test("renders a singleton Recreate deployment and hardened Server pod", () => {
		const manifests = renderServerManifests(values);
		const deployment = serverDeployment(manifests);
		const podSpec = object(nested(deployment, "spec", "template", "spec"));
		const server = container(deployment, "compass-server");
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
		expect(podSpec).toHaveProperty("initContainers");
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

	test("copies the init-only admin token into the shared state volume", () => {
		const deployment = serverDeployment(renderServerManifests(values));
		const pod = object(nested(deployment, "spec", "template", "spec"));
		const initContainers = pod.initContainers;
		if (!Array.isArray(initContainers))
			throw new Error("init container missing");
		const init = object(initContainers[0]);
		expect(init.image).toBe(values.initImage);
		expect(init.command).toEqual([
			"sh",
			"-c",
			"mkdir -p /run/compass/bootstrap /run/compass/xdg-state && cp /var/run/compass/admin-token/token /run/compass/bootstrap/admin-token && chmod 0600 /run/compass/bootstrap/admin-token",
		]);
		expect(init.securityContext).toEqual({
			runAsNonRoot: true,
			runAsUser: 65532,
			runAsGroup: 65532,
			allowPrivilegeEscalation: false,
			readOnlyRootFilesystem: true,
			capabilities: { drop: ["ALL"] },
			seccompProfile: { type: "RuntimeDefault" },
		});
		const volumes = pod.volumes;
		if (!Array.isArray(volumes)) throw new Error("pod volumes missing");
		const token = volumes
			.map(object)
			.find((volume) => volume.name === "admin-token");
		expect(token).toEqual({
			name: "admin-token",
			secret: { secretName: values.adminTokenSecretName, defaultMode: 0o440 },
		});
		expect(init.volumeMounts).toEqual([
			{ name: "compass-run", mountPath: "/run/compass" },
			{
				name: "admin-token",
				mountPath: "/var/run/compass/admin-token",
				readOnly: true,
			},
		]);
		expect(
			container(deployment, "compass-server").volumeMounts,
		).not.toContainEqual(expect.objectContaining({ name: "admin-token" }));
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
			"/run/compass/bootstrap/admin-token",
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

	test("omits Postgres without values and renders hardened dev Postgres when set", () => {
		const absent = renderServerManifests(serverValuesWithoutPostgres);
		expect(
			absent.some(
				(item) =>
					["StatefulSet", "Service"].includes(String(object(item).kind)) &&
					String(nested(item, "metadata", "name")).startsWith(
						"compass-postgres",
					),
			),
		).toBe(false);
		const manifests = renderServerManifests({
			...values,
			postgres: {
				image: digest("postgres"),
				storage: "20Gi",
				secretName: "compass-postgres",
			},
		});
		const postgres = manifests
			.map(object)
			.find(
				(item) =>
					item.kind === "StatefulSet" &&
					object(item.metadata).name === "compass-postgres",
			);
		expect(postgres).toBeDefined();
		const pgContainer = object(
			(
				object(nested(postgres, "spec", "template", "spec"))
					.containers as unknown[]
			)[0],
		);
		expect(pgContainer.env).toEqual(
			expect.arrayContaining([
				{ name: "POSTGRES_USER", value: "compass" },
				{
					name: "POSTGRES_PASSWORD",
					valueFrom: {
						secretKeyRef: { name: "compass-postgres", key: "password" },
					},
				},
				{ name: "POSTGRES_INITDB_ARGS", value: "--auth=scram-sha-256" },
				{ name: "POSTGRES_HOST_AUTH_METHOD", value: "scram-sha-256" },
			]),
		);
		const policies = manifests.filter(
			(item) => object(item).kind === "NetworkPolicy",
		);
		expect(policies).toHaveLength(2);
		expect(
			policies.some(
				(item) =>
					object(nested(item, "spec", "podSelector", "matchLabels"))[
						"app.kubernetes.io/name"
					] === "compass-postgres",
			),
		).toBe(true);
		const postgresPolicy = policies
			.map(object)
			.find(
				(item) =>
					object(nested(item, "spec", "podSelector", "matchLabels"))[
						"app.kubernetes.io/name"
					] === "compass-postgres",
			);
		expect(nested(postgresPolicy, "spec", "ingress")).toEqual([
			{
				from: [
					{
						podSelector: {
							matchLabels: { "app.kubernetes.io/name": "compass-server" },
						},
					},
				],
				ports: [{ protocol: "TCP", port: 5432 }],
			},
		]);
	});

	test("renders only the required NATS ingress policy when Postgres is absent", () => {
		const policies = renderServerManifests(values).filter(
			(item) => object(item).kind === "NetworkPolicy",
		);
		expect(policies).toHaveLength(1);
		expect(object(policies[0]).metadata).toMatchObject({
			name: "compass-nats-ingress",
		});
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

	test("rejects mutable image tags for Server, NATS, init, and Postgres images", () => {
		for (const field of ["serverImage", "natsImage", "initImage"] as const) {
			expect(() =>
				parseServerDeployValues({
					...values,
					[field]: "example.test/image:latest",
				}),
			).toThrow(field);
		}
		expect(() =>
			assertServerImageDigest("registry.example.test/server:latest"),
		).toThrow();
		expect(() =>
			parseServerDeployValues({
				...values,
				postgres: {
					image: "example.test/postgres:latest",
					storage: "10Gi",
					secretName: "pg-secret",
				},
			}),
		).toThrow("postgres.image");
	});

	test("rejects invalid storage quantities and accepts Kubernetes binary quantities", () => {
		for (const quantity of ["", "banana", "0", "-1Gi", "1KB", "1.2.3Gi"]) {
			expect(() =>
				parseServerDeployValues({ ...values, natsStorage: quantity }),
			).toThrow("natsStorage");
		}
		expect(() =>
			parseServerDeployValues({
				...values,
				natsStorage: "10Gi",
				postgres: {
					image: digest("postgres"),
					storage: "bad-size",
					secretName: "pg-secret",
				},
			}),
		).toThrow("postgres.storage");
		expect(() =>
			parseServerDeployValues({ ...values, natsStorage: "1.5Gi" }),
		).not.toThrow();
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
			if (metadata.namespace !== undefined)
				expect(metadata.namespace).toBe("compass");
		}
	});
});
