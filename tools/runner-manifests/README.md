# Runner manifests

Render Kubernetes objects from an operator values file:

```sh
bun run render.ts values.example.json
```

The image must be a `repo@sha256:<digest>` reference. Configure node selectors,
taints, host paths, the Server address (an absolute `https://` URL), token
lifetimes, and admission identities before applying.

Three optional values:

- `egressAllow`: the hosts agents may reach, such as `["github.com", "api.github.com"]`.
  Each is a DNS name or a bare IP literal. It renders as one `--egress-allow`
  argument. Without it, agents run default-deny and cannot reach a forge.
- `serverCA`: `{ "configMapName": "<name>", "key": "<file>" }` or
  `{ "secretName": "<name>", "key": "<file>" }`. Set it when the Server's
  certificate is not under a public root. The key is mounted read-only under
  `/etc/compass/server-ca/`, and `COMPASS_RUNNER_CA` points at it.
- `serverName`: the certificate name the Runner checks instead of `serverAddr`'s
  host. Set it when `serverAddr` is the in-cluster Service and the certificate
  names only a public host. It is passed as `COMPASS_SERVER_NAME`.

The DaemonSet uses a projected `compass-runner` ServiceAccount token at
`/var/run/secrets/compass/runner/token`. Set `tokenExpirationSeconds` between
600 and 4294967296, and no higher than the matching Server cluster's
`maxTokenLifetime`. The namespace, ServiceAccount, and audience (`compass-runner`)
must match that Server cluster entry. The admission policy uses
`admission.controllers` and `admission.deployers`; set them to the controller
identities that create Runner pods and the identity that applies the DaemonSet.
It denies exec, attach, and ephemeral containers on Runner pods except to
`admission.breakGlassGroups` (default `system:masters`).
The Server assigns each Runner ID during enrollment. The ServiceAccount has no
RBAC binding. Rollout beyond one node waits for the multi-Runner hub.
Runner nodes must be amd64.

## KVM device delivery

- Install an operator-selected, digest-pinned device plugin that advertises the
  configured `kvmResourceName` for `/dev/kvm`. The plugin injects the device and
  its cgroup device allowance; no `/dev` hostPath is used.
- Set node `/dev/kvm` mode to `0666`. User-namespaced pods see the device as
  `65534:65534`, so a host `kvm` group grant does not work.

## Other node prerequisites

- Enable Kubernetes user-namespace support.
- Set `kernel.apparmor_restrict_unprivileged_userns=0` where the node image
  enables that restriction.
- Stage `seccomp/compass-runner.json` at the kubelet seccomp root under the
  configured `seccompProfilePath` before starting the DaemonSet. It is the
  profile measured to boot, not yet minimized: `clone`, `clone3`, `unshare`
  and `mount` are allowed without argument filters. Narrow it with a
  remove-one test on a real node before production use.
- Provide the `hostPaths.sessionVolumeRoot` directory on a filesystem
  configured for project quotas. Leave that directory itself unprojected; put
  the project quota on `<sessionVolumeRoot>/volumes`. Own both by uid and gid
  65532. Provide the separate runtime host tree for stale-session reaping,
  also owned by uid and gid 65532. User-namespaced pods mount both trees
  idmapped. Not yet verified on a real node.

Metrics: set `OTEL_EXPORTER_OTLP_ENDPOINT` to push them. The Runner serves no
scrape endpoint.
