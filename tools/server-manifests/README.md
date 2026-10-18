# Server manifests

Render Kubernetes objects from an operator values file:

```sh
bun run render.ts values.example.json
```

Images must use `repo@sha256:<64 hex>` references; tagged refs are rejected, and
only the digest field should be updated by image automation. `natsStorage` is
a Kubernetes storage quantity. `external.port` is the optional LoadBalancer
Service port. The renderer emits no Secret objects or secret values; create
each referenced Kubernetes Secret with the keys below. Forge secret names are
dotenv names, not Kubernetes Secret names; each needs a `SERVER_<name>` line
in the `server.env` key of `serverSecretsSecretName`.

| Value | Key | Use |
| --- | --- | --- |
| `dsnSecretName` | `dsn` | Server `COMPASS_DATABASE_DSN` |
| `natsSecretName` | `url` | Server `COMPASS_NATS_URL` |
| `natsSecretName` | `password` | NATS `NATS_PASSWORD` |
| `serverSecretsSecretName` | `server.env` | Read-only Server secrets file |
| `adminTokenSecretName` | `token` | Read-only Server admin token file |
| `s3SecretName` | all keys | Optional Server environment |
| `tlsSecretName` | `tls.crt`, `tls.key` | Read-only Server TLS files |
| `forge.appKeySecret` | `SERVER_<name>` in `server.env` | Forge App private key |
| `forge.appWebhookSecret` | `SERVER_<name>` in `server.env` | Forge webhook signing key |
| `forge.reviewer.appKeySecret` | `SERVER_<name>` in `server.env` | Optional reviewer App private key |

The admin token file is mounted directly at `/var/run/compass/admin-token/token`.
Postgres is operator-provided, not rendered. Its DSN must use a direct,
session-mode endpoint; the login role must own the target database and be able
to create objects in `public`. PostgreSQL must be version 16 or newer when the
login role holds CREATEROLE and BYPASSRLS; a superuser may use any version.
The database must answer at Server boot. One replica fits the default pool of
10.

`runnerClusters` is written to the `clusters.yaml` ConfigMap entry. For a local
Kubernetes issuer without anonymous discovery, fetch its public keys at render
time and pass the JSON text as `jwks`:

```sh
kubectl get --raw /openid/v1/jwks
```

The renderer stores it as `<name>.jwks.json` and points that cluster's `jwksFile`
at the mounted path. `jwksURI` can be used instead; it must be an HTTPS URL and
cannot be set together with `jwks`. Cluster names are unique, lowercase DNS
labels of at most 40 characters. `maxTokenLifetime` must be a Go duration of at
least `600s`.

The `serverSecretsSecretName` Secret key `server.env` holds `COMPASS_MASTER_KEY`
and all `SERVER_<name>` values read by the dotenv provider. `COMPASS_MASTER_KEY`
must remain unchanged after initialization; changing it makes the Server fail
to start. Never put forge secret values in `forge` or agent environment.

NATS uses the R1 file JetStream settings with a PVC-backed store and requires
the `password` key to match the NATS config authorization user.

If boot fails with `resolve master key: ... exit status 1`, check the `server.env`
Secret key and `/etc/compass/secrets/server.env` mount first. The resolver names
that dotenv path on mount, path, and read errors, but malformed dotenv content
and missing keys provide less detail.
