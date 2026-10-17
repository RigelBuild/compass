# Server manifests

Render Kubernetes objects from an operator values file:

```sh
bun run render.ts values.example.json
```

Images must use untagged `repo@sha256:<digest>` references. `natsStorage` and
the optional dev-only `postgres.storage` are Kubernetes storage quantities.
`external.port` is the optional LoadBalancer Service port. The renderer emits no
Secret objects or secret values; create each referenced Secret with the keys
below:

| Value | Key | Use |
| --- | --- | --- |
| `dsnSecretName` | `dsn` | Server `COMPASS_DATABASE_DSN` |
| `natsSecretName` | `url` | Server `COMPASS_NATS_URL` |
| `natsSecretName` | `password` | NATS `NATS_PASSWORD` |
| `serverSecretsSecretName` | `server.env` | Read-only Server secrets file |
| `adminTokenSecretName` | `token` | Read-only init-container token input |
| `s3SecretName` | all keys | Optional Server environment |
| `tlsSecretName` | `tls.crt`, `tls.key` | Read-only Server TLS files |
| `postgres.secretName` | `password` | Postgres `POSTGRES_PASSWORD` |

`runnerClusters` is written to the `clusters.yaml` ConfigMap entry. To use a
local Kubernetes issuer without anonymous discovery, fetch its public keys at
render time and pass the JSON text as `jwks`:

```sh
kubectl get --raw /openid/v1/jwks
```

The renderer stores it as `<name>.jwks.json` and points that cluster's `jwksFile`
at the mounted path. `jwksURI` can be used instead; it must be an HTTPS URL and
cannot be set together with `jwks`. Cluster names are unique, lowercase DNS
labels of at most 40 characters. `maxTokenLifetime` must be a Go duration of at
least `600s`.

If the Server fails at boot with `resolve master key: ... exit status 1`, check
the `server.env` Secret key and the `/etc/compass/secrets/server.env` mount
first. The Server discards secretspec's stderr because it can quote secret
values, so a malformed file shows no detail. Newer Server builds name a missing
or unreadable file in the error.

NATS uses the R1 file JetStream settings with a PVC-backed store and requires
the `password` key to match the NATS config authorization user. The optional
Postgres StatefulSet is for development; operators must back up its volume and
the Server master key. It uses `compass` and SCRAM-SHA-256 authentication, and
needs a Postgres 18+ image: the PVC mounts at `/var/lib/postgresql`, where those
images keep a versioned data directory.
