# Runner manifests

Render Kubernetes objects from an operator values file:

```sh
bun run render.ts values.example.json
```

The image must be a `repo@sha256:<digest>` reference. Replace the example node
selector, taint, host paths, Server address, and token Secret before applying.

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
- Provide the session-volume host tree on a filesystem configured for project
  quotas. Provide the separate runtime host tree for stale-session reaping.
  Own both by uid and gid 65532: user-namespaced pods mount them idmapped. Not
  yet verified on a real node.
- Create the `runnerTokenSecret` with the key named in the values file.

**Single-node only for now.** A Runner token is minted for one runner ID, and
each pod enrolls as its node name. One shared Secret therefore enrolls only
the node whose name matches the token. Per-node token delivery is not
designed yet.

Metrics: set `OTEL_EXPORTER_OTLP_ENDPOINT` to push them. The Runner serves no
scrape endpoint.

The Runner does not call the Kubernetes API. Its ServiceAccount disables
credential automount, and no Role is rendered.
