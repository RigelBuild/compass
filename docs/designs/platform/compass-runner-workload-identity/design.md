# Runner authentication from Kubernetes workload identity

Ledger-impact: mints DL-440

## Problem / Intent

A Runner proves its identity with a per-Runner bearer token. An operator mints
the token with `compass-mint-runner-token` and delivers it out of band. The
DaemonSet in
[Containerizing the Compass Runner](../compass-runner-containerization/design.md)
puts a Runner on every eligible node. Minting one secret per node does not
scale, and it leaves a long-lived secret at rest on every host. This record
lets a Kubernetes-hosted Runner authenticate with the identity its pod already
has. The minted-token path stays for Runners that are not on Kubernetes.

## Approach

A Kubernetes-hosted Runner presents a **projected ServiceAccount token** with
audience `compass-runner` and a 600 s expiry, which the kubelet rotates. The
Server verifies the token offline against the cluster's OIDC issuer keys
(JWKS). It maps the identity to a Runner ID derived from (registered cluster,
node name) and resolves it to the existing `store.SubjectRunner`. The door's
callers do not change.

Today the Runner sends `os.Getenv("COMPASS_RUNNER_TOKEN")` as a fixed bearer
string (`go/internal/runner/runner.go` `bearerToken`).
`bearerAuth.authenticate` (`go/internal/runnerhub/auth.go`) resolves it
through the `TokenResolver` closure in `buildNetworkServer`
(`go/server/network_door.go`): `return auth.ResolveToken(ctx, st, presented,
want)`, a SHA-256 lookup in `tokens`. No code in the module verifies an
inbound signed token.

### Cluster registration: a Server config file

The trust root is a YAML file named by `--runner-clusters` or
`$COMPASS_RUNNER_CLUSTERS`, in the same flag-then-env pattern as `--nats-url`.

```yaml
clusters:
  - name: prod-eks                 # [a-z0-9-]{1,40}; first segment of the Runner ID
    issuer: https://oidc.eks.us-east-1.amazonaws.com/id/EXAMPLE
    audience: compass-runner       # default compass-runner
    namespace: compass-runner
    serviceAccount: compass-runner
    maxTokenLifetime: 600s         # default and minimum 600s; upper bound on exp - iat
    jwksURI: ""                    # optional; overrides OIDC discovery
    jwksFile: ""                   # optional static JWKS; no fetch
    caFile: ""                     # optional PEM CA for a private issuer
```

The Server refuses to start unless `name` and `issuer` are each unique
(issuers compared without a trailing `/`), `issuer` is `https://`,
`jwksURI` and `jwksFile` are not both set, and `maxTokenLifetime` is at least
600 s. That is the Kubernetes TokenRequest minimum (`MinTokenAgeSec` in
`pkg/apis/authentication/validation/validation.go`), so a shorter bound would
reject every projected token. With no flag, no cluster is trusted and the door
works as it does today.

**A unique issuer is load-bearing.** The Server selects the cluster by `iss`,
so two clusters sharing an issuer could forge each other's node names. EKS
issuers are unique. A self-managed cluster (k3s, kubeadm) MUST set a unique
`--service-account-issuer`. Distinct issuer strings are not enough if two
clusters share a signing key, as happens when kubeadm templates copy
`sa.key`. So after each JWKS load the Server recomputes RFC 7638 thumbprints
across every cluster's current key set. While a key is in more than one set,
every cluster holding it fails closed, whichever set loaded first, and the
Server logs their names. Failing only the later cluster would leave the
earlier one accepting tokens the other cluster signs. The cost is that a
registered cluster can take another offline by publishing its public key.

A file, not a table plus admin RPC: the trust root is rare and
security-critical, so it belongs in reviewed infrastructure-as-code. The mint
CLI is "deliberately not an automated RPC" for the same reason. Adding or
removing a cluster costs a Server rollout, which is also how a cluster is
revoked.

### Tenancy

A cluster binds to **no tenant**. "A Runner is shared across tenants and its
door carries none" (`go/internal/runnerhub/runner_tenant_pgtest_test.go`). The
verifier stamps the bootstrap tenant. A minted Runner token gets the same,
because `PutTokenHash` stamps `s.resolveTenant(ctx)`. No Runner-door code
reads `Subject.Tenant`.

### Verification

`auth.RunnerVerifier.Verify` runs these steps in order:

1. `jwt.ParseSigned(tok, []jose.SignatureAlgorithm{jose.RS256, jose.ES256})`.
   This rejects `none` and HMAC.
2. Read `iss` unverified and match it exactly. An unknown issuer fails with no
   network fetch.
3. Require a non-empty header `kid`, which Kubernetes always sets. Select keys
   by it and verify the signature.
4. `Claims.ValidateWithLeeway(jwt.Expected{Issuer: c.Issuer,
   AnyAudience: jwt.Audience{c.Audience}, Time: now}, jwt.DefaultLeeway)`, with
   1 minute of leeway.
5. Require `exp` and `iat`, because go-jose skips an absent time claim. Require
   `exp - iat ≤ maxTokenLifetime`.
6. Require `sub == "system:serviceaccount:<namespace>:<serviceAccount>"`, and
   matching `kubernetes.io.namespace` and `kubernetes.io.serviceaccount.name`.
7. Require non-empty `kubernetes.io.pod.name` and `kubernetes.io.node.name`.

Every failure becomes `errUnauthenticated`, with one exception. If a known
cluster has no usable key set, `Verify` returns `auth.ErrKeysUnavailable` and
the door answers `CodeUnavailable`. That happens when the keys were never
fetched, were discarded as stale, or share a key with another cluster.
`retryableDialError` (`go/internal/runner/run.go`) retries Unavailable, so an
issuer outage does not crash-loop the fleet. The exception reveals only that a
cluster's keys are unusable. The Server logs causes with the cluster name and
never logs the token.

The claim names follow the Kubernetes bound-token format. The token carries
`aud`, `exp`, `iat`, `iss`, `jti` and `sub`, plus a `"kubernetes.io"` object
holding `namespace`, `node{name,uid}`, `pod{name,uid}` and
`serviceaccount{name,uid}`
(<https://kubernetes.io/docs/reference/access-authn-authz/service-accounts-admin/>).
The node claims are on by default from 1.32.

The verifier uses go-jose v4 directly. go-oidc's remote key set re-fetches on
every unknown `kid` with no rate limit.

### JWKS fetch, cache and rotation

The Server keeps one in-memory key set per cluster.

- **Source.** A `jwksFile` is read at load. Otherwise the Server fetches
  `<issuer>/.well-known/openid-configuration`, then its `jwks_uri` (or the
  configured `jwksURI`). Fetches are https-only, trust `caFile`, cap the body
  at 1 MiB, and time out after 3 s. Discovery plus JWKS therefore fits inside
  the Runner's 10 s `enrollAttemptTimeout`.
- **Schedule.** Serve start begins one background fetch per cluster and does
  not wait for it. A refresh then runs hourly under the serve loop's context.
  An unknown `kid` triggers at most one re-fetch per cluster per 5 minutes,
  deduplicated with `singleflight`.
- **Cooldown pinning.** Issuers are public, so forged-`kid` tokens can keep the
  cooldown used up. The worst case delays a new key to the hourly refresh.
  That is acceptable because operators publish a key at least an hour before
  signing with it (W4 docs).
- **Failure.** The last good key set is kept for 24 h after a fetch error,
  then discarded (`CodeUnavailable` until a fetch succeeds).

### Runner ID derivation

The ID is `<cluster>/<node>`, with each `.` in the node name replaced by `_`.
For example, `prod-eks/ip-10-0-1-5_ec2_internal`.

- `fabric.ValidSubjectToken` rejects `.` in the NATS subject
  `compass.runner.<runner_id>.cmd`. RFC 1123 node names never contain `_` or
  `/`, so the mapping is injective and splits unambiguously.
- `/` becomes reserved. `StoreRunnerTokenHash` and `MintRunnerToken` reject an
  ID containing it. Rows minted before this change are not covered, so with
  clusters configured the Server refuses to start if any non-revoked Runner
  token's subject ID contains `/`.
- The ID is per node, not per pod. Two Runner pods on one node would sweep
  each other's containers and both receive every command, hence `maxSurge: 0`.
- A node rebuilt with the same name keeps its ID, as a reused minted token
  does.

### Subject and enrollment

The verifier returns `store.Subject{Kind: store.SubjectRunner, ID: <runner id>,
Tenant: <bootstrap>}`. No new `SubjectKind` is added (`go/internal/store/types.go`:
"Sealed to exactly these three").

The pod cannot compute its own ID, because it does not know the cluster's
registered name. A new `EnrollResponse.runner_id = 2` returns `subj.ID`. The
Runner uses it for its ownership label and stale-container sweep.
`Handler.Enroll` already accepts an empty `EnrollRequest.runner_id` and checks
a non-empty one, so that is unchanged. Only the `EnrollRequest` proto comment
changes.

### Door ordering

The door tells the two token kinds apart by shape, inside the existing
`TokenResolver` closure:

- A token with exactly two dots (a compact JWS) goes to `Verify` only, never
  to the hash lookup. The branch returns `auth.ErrWrongKind` unless
  `want == store.SubjectRunner`.
- Any other token goes to `auth.ResolveToken` unchanged.

A minted token is `base64.RawURLEncoding` and has no `.`. `StoreRunnerTokenHash`
now rejects a token containing `.`, which would otherwise be stored but never
resolve. `bearerAuth.authenticate` gains one branch: `auth.ErrKeysUnavailable`
maps to `CodeUnavailable`. Only the RunnerService door has the verifier, so a
projected token on another door misses the hash lookup and gets
`Unauthenticated`.

### Runner side

- `--token-file` / `$COMPASS_RUNNER_TOKEN_FILE` names the projected token. It
  is mutually exclusive with `$COMPASS_RUNNER_TOKEN`, and makes `--runner-id`
  optional.
- `bearerToken` asks a `TokenSource` on every call. `FileToken` re-reads the
  file, which the kubelet replaces atomically at about 80% of the token's
  lifetime. No watcher is needed.
- On a read error, `FileToken` logs and returns its last good token while that
  token is unexpired (`exp` read unverified). One transient error therefore
  cannot end the Runner and its sessions.
- `compass-runner` refuses a `--mount` whose host path is the token directory,
  inside it, or an ancestor of it.

### DaemonSet change

This extends containerization task R3:

```yaml
metadata:
  name: compass-runner                      # the policy's owner check names it
  namespace: compass-runner
spec:
  updateStrategy:
    type: RollingUpdate
    rollingUpdate:
      maxSurge: 0                           # the Runner ID is per node
  template:
    spec:
      serviceAccountName: compass-runner
      automountServiceAccountToken: false   # the Runner never calls the API server
      containers:
        - name: runner
          env:
            - name: COMPASS_RUNNER_TOKEN_FILE
              value: /var/run/secrets/compass/runner/token
          volumeMounts:
            - name: compass-runner-token
              mountPath: /var/run/secrets/compass/runner
              readOnly: true
      volumes:
        - name: compass-runner-token
          projected:
            sources:
              - serviceAccountToken:
                  audience: compass-runner
                  expirationSeconds: 600    # the Kubernetes minimum; ≤ maxTokenLifetime
                  path: token
```

The ServiceAccount has **no** RBAC binding. An admission policy limits who can
obtain the identity:

- A pod using the ServiceAccount must be created by a `controllers` identity
  and have the DaemonSet `compass-runner` as its controller owner. One
  controller creates every DaemonSet's pods, and with shared controller-manager
  credentials every controller is `system:kube-controller-manager`, so the
  username alone cannot single out the Runner's pods.
- Only a `deployers` identity may create that DaemonSet or change its `spec`.
  A metadata-only update, such as the garbage collector removing a finalizer,
  passes.
- No other built-in workload template may name the ServiceAccount, so a bad
  workload is rejected when applied. Pods from any other controller fail the
  pod rule.
- Tokens for the ServiceAccount go only to kubelets.

`controllers` and `deployers` are R3 render parameters written into
`spec.variables`, not a `paramKind` object. A param object would be one more
writable object the identity depends on; the policy is cluster-scoped and
admin-only.

```yaml
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicy
metadata:
  name: compass-runner-identity
spec:
  failurePolicy: Fail
  matchConstraints:
    namespaceSelector:
      matchLabels: {kubernetes.io/metadata.name: compass-runner}
    resourceRules:
      - apiGroups: [""]
        apiVersions: [v1]
        operations: [CREATE]
        resources: [pods, serviceaccounts/token]
      - apiGroups: [""]
        apiVersions: [v1]
        operations: [CREATE, UPDATE]
        resources: [replicationcontrollers]
      - apiGroups: [apps]
        apiVersions: [v1]
        operations: [CREATE, UPDATE]
        resources: [daemonsets, deployments, replicasets, statefulsets]
      - apiGroups: [batch]
        apiVersions: [v1]
        operations: [CREATE, UPDATE]
        resources: [jobs, cronjobs]
  variables:
    - name: controllers                     # render parameter
      expression: >-
        ['system:serviceaccount:kube-system:daemon-set-controller',
         'system:kube-controller-manager']
    - name: deployers                       # render parameter
      expression: "['system:serviceaccount:flux-system:kustomize-controller']"
    - name: res
      expression: request.resource.resource
    - name: podSpec
      expression: >-
        variables.res == 'pods' ? object.spec
        : variables.res == 'cronjobs' ? object.spec.jobTemplate.spec.template.spec
        : object.spec.template.spec
    - name: usesRunnerSA
      expression: >-
        has(variables.podSpec.serviceAccountName) &&
        variables.podSpec.serviceAccountName == 'compass-runner'
    - name: isRunnerDS
      expression: variables.res == 'daemonsets' && object.metadata.name == 'compass-runner'
  validations:
    - expression: >-
        variables.res != 'serviceaccounts' || request.name != 'compass-runner' ||
        request.userInfo.username.startsWith('system:node:')
      message: only kubelets may request a compass-runner token
    - expression: >-
        variables.res != 'pods' || !variables.usesRunnerSA ||
        (request.userInfo.username in variables.controllers &&
         has(object.metadata.ownerReferences) &&
         object.metadata.ownerReferences.exists(r,
           has(r.controller) && r.controller && r.apiVersion == 'apps/v1' &&
           r.kind == 'DaemonSet' && r.name == 'compass-runner'))
      message: compass-runner pods must come from the compass-runner DaemonSet
    - expression: >-
        variables.res in ['pods', 'serviceaccounts'] || variables.isRunnerDS ||
        !variables.usesRunnerSA
      message: only the compass-runner DaemonSet may use the compass-runner ServiceAccount
    - expression: >-
        !variables.isRunnerDS || request.userInfo.username in variables.deployers ||
        (request.operation == 'UPDATE' && object.spec == oldObject.spec)
      message: only a deployer may create or change the compass-runner DaemonSet
---
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicyBinding
metadata:
  name: compass-runner-identity
spec:
  policyName: compass-runner-identity
  validationActions: [Deny]
```

### NATS-plane credentials

The Runner has no NATS client today. One forward constraint applies: the
DL-316 auth-callout MUST accept a projected token through
`RunnerVerifier.Verify`, with the same shape dispatch as the door. The
callout's own record decides its user-JWT lifetime policy.

### Revocation and node removal

- **Cluster:** remove its entry and roll the Servers.
- **Node:** delete the Runner pod or the Node. The last token expires within
  `maxTokenLifetime`. Offline verification does not see pod deletion.
- **Live streams:** the Connect door authenticates when a stream opens, so a
  stream outlives its token. A later-revoked minted token behaves the same way
  today. This holds until the Sessions plane moves to NATS (DL-316).

### Threat model

| Threat | Bound |
| --- | --- |
| Replay of a stolen projected token | Verifies until `exp`: at most `maxTokenLifetime` (default 600 s) after issue, plus 60 s leeway. A Connect stream opened in that window outlives the token (§Revocation). TLS-only door; never logged. |
| Node A claims node B's ID | `node.name` is the API server's record of the pod's node. NodeRestriction and the Node authorizer let a kubelet get tokens only for pods bound to it. |
| A user with `edit` or `admin` in the Runner namespace | Both built-in roles grant `create` on `serviceaccounts/token` and `pods`. Such a user can mint a token for any Runner pod, or create a pod with `spec.nodeName` on any node, and so enroll as any node. Bound: no such binding (§Global Constraints) and the admission policy. |
| Another workload uses the Runner ServiceAccount, directly or through a controller (an attacker-authored DaemonSet) | The controller manager creates a controller's pods, so a username check alone would admit them. Bound: the policy admits a Runner-ServiceAccount pod only when its controller owner is the DaemonSet `compass-runner`; only `deployers` may create that DaemonSet or change its `spec`; no other workload template may name the ServiceAccount. |
| An agent reads the Runner token | It could enroll as the node. Bound: the `--mount` guard, and a microVM guest has no view of the pod filesystem. |
| Token for another audience, or a legacy Secret token | `aud` mismatch; or a different `iss` and no `exp`. |
| One cluster forges another's nodes | Exact `iss` selection, unique issuers, and the cross-cluster key-thumbprint check, which fails every cluster sharing a key. |
| `kid` flood | Unknown issuer: no fetch. Known issuer: one re-fetch per 5 minutes; a pinned cooldown delays a new key to the hourly refresh. |
| Issuer outage | `CodeUnavailable`, which the Runner retries; last good keys kept 24 h. |

## Alternatives considered

- **Per-node token file on the host — rejected.** Manual per node, and a
  long-lived secret at rest.
- **One fleet-wide token — rejected.** One leak compromises every node, and
  `RunnerTokenStatus` returns `TokenOtherSubject` for a mismatched Runner ID,
  so one token names one Runner.
- **Per-node Kubernetes Secret — rejected.** A secret at rest in etcd, RBAC
  around it, and manual creation per node.
- **TokenReview as the primary check — rejected.** The Server would need to
  reach every cluster's API server, often a private endpoint, and hold a
  credential per cluster. Per-RPC cost is not the reason, since a review can be
  cached until `exp`. Deferred: offline verification plus an optional
  Enroll-time TokenReview, which would catch deleted pods (OQ-1).
- **Database table plus admin RPC for clusters — rejected.** It adds a runtime
  write path to the trust root.

## Global Constraints

- **Kubernetes ≥ 1.32**, with NodeRestriction and the Node authorizer.
- **Multi-node use needs a multi-Runner hub.** `Hub` holds one attached Runner
  ("single-Runner MVP, OQ6", `go/internal/runnerhub/hub.go`). A second enroll
  replaces the first and clears every in-memory binding. Until the hub
  supports subject-addressable Runners (DL-312), a cluster runs one Runner pod.
  W4's rollout beyond one node is gated on that work.
- **`maxSurge: 0`.** Two Runner pods on one node share an ID. The R3 render
  test asserts it.
- **No identity-granting role in the Runner namespace.** No RoleBinding or
  ClusterRoleBinding there may grant `edit`, `admin`, or `create` on `pods` or
  `serviceaccounts/token`. The controller manager, kubelets and the `deployers`
  identities are the only exceptions. Cluster administrators and the deployers
  are inside the trust boundary.
- **Policy render parameters.** `controllers` defaults to both controller
  usernames: `system:serviceaccount:kube-system:daemon-set-controller`
  (per-controller credentials) and `system:kube-controller-manager` (shared
  credentials). `deployers` defaults to
  `system:serviceaccount:flux-system:kustomize-controller`.
- **`github.com/go-jose/go-jose/v4` v4.1.5.** Update every Go `vendorHash`
  (`flake.nix`, `guest-image/default.nix`).
- **Context flows from the caller.** New functions take `ctx` first. No
  `context.Background()` outside `main` and tests.
- **One failure shape, one exception.** Verifier failures become
  `errUnauthenticated`, except keys unavailable for a known cluster, which
  becomes `CodeUnavailable`. Tokens are never logged.
- **The minted path is unchanged**, except that its IDs may not contain `/` and
  its tokens may not contain `.`.
- **No new `SubjectKind`.**
- **Public-repo prose:** no private repository, internal hostname or tracker ID.

## Plan

### W1 — Verifier and cluster config

New files `go/internal/auth/runner_clusters.go` and
`go/internal/auth/workload_identity.go`, with tests. Adds go-jose.

```go
type RunnerCluster struct {
	Name, Issuer, Audience, Namespace, ServiceAccount string
	JWKSURI, JWKSFile, CAFile                          string
	MaxTokenLifetime                                   time.Duration // default 600s; < 600s fails to parse
}
var ErrKeysUnavailable = errors.New("auth: runner cluster keys unavailable")
func ParseRunnerClusters(data []byte) ([]RunnerCluster, error)
func NewRunnerVerifier(clusters []RunnerCluster, tenant store.TenantID, client *http.Client, now func() time.Time) (*RunnerVerifier, error)
func (v *RunnerVerifier) Start(ctx context.Context) // background first fetch, then hourly, until ctx ends
func (v *RunnerVerifier) Verify(ctx context.Context, token string) (store.Subject, error)
func LooksLikeJWT(token string) bool                 // strings.Count(token, ".") == 2
func WorkloadRunnerID(cluster, node string) string   // cluster + "/" + strings.ReplaceAll(node, ".", "_")
```

### W2 — Door wiring, enrollment ID, mint guards

- `ServeConfig.RunnerClustersPath string` and `--runner-clusters` /
  `$COMPASS_RUNNER_CLUSTERS` in `go/cmd/compass-server/main.go`.
- At serve start, build the verifier with `st.BootstrapTenant(ctx)` and call
  `Start(ctx)`. With clusters configured, refuse to start if
  `st.CountRunnerTokenIDsWithSlash(ctx)` is non-zero.
- `runnerResolve` JWS branch: `auth.ErrWrongKind` unless
  `want == store.SubjectRunner`; `auth.ErrTokenNotFound` with no clusters;
  otherwise `Verify`.
- `bearerAuth.authenticate`: `auth.ErrKeysUnavailable` maps to
  `CodeUnavailable`.
- `runner.proto`: `string runner_id = 2;` on `EnrollResponse`, and an updated
  `EnrollRequest` comment. `Handler.Enroll` returns `subj.ID`.
- `StoreRunnerTokenHash` and `MintRunnerToken` reject an ID with `/`, and
  `StoreRunnerTokenHash` rejects a token with `.`. Both wrap
  `store.ErrInvalidArgument`.
- New sqlc query in `go/internal/store/queries/tokens.sql`:

```go
// SELECT count(*) FROM tokens
// WHERE subject_kind = 1 AND revoked_at IS NULL AND strpos(subject_id, '/') > 0
func (s *Store) CountRunnerTokenIDsWithSlash(ctx context.Context) (int64, error)
```

### W3 — Runner token source and Server-assigned ID

New file `go/internal/runner/token_source.go`.

```go
type TokenSource interface{ Token() (string, error) }
type StaticToken string // $COMPASS_RUNNER_TOKEN
type FileToken struct{ /* path, logger, clock; mutex-guarded last good token and its exp */ }
func NewFileToken(path string, log *slog.Logger, now func() time.Time) *FileToken
func (l *ServerLink) RunnerID() string // from EnrollResponse.runner_id
```

- `RunnerConfig.Token` becomes a `TokenSource`; remove the unread
  `ServerLink.token`.
- `bearerToken`: a unary call returns the `Token()` error; a streaming call
  sends no header.
- `Run` uses `link.RunnerID()`.
- `compass-runner` adds `--token-file` / `$COMPASS_RUNNER_TOKEN_FILE` and the
  `--mount` guard.

### W4 — Manifests, admission policy and operator docs

- R3 manifests: §DaemonSet change in full, including `maxSurge: 0`, the
  binding-free ServiceAccount, and the policy and its binding. Rollout beyond
  one node waits for the multi-Runner hub.
- `docs/self-host.md` §Runner enrollment documents:
  - the cluster file and the unique-issuer rule;
  - key reachability. Discovery and JWKS are readable only by service accounts
    by default (`system:service-account-issuer-discovery` is bound to
    `system:serviceaccounts`). An out-of-cluster Server needs one of three
    options:
    - that role bound to `system:unauthenticated`, with anonymous auth on;
    - the JWKS published at a reachable `--service-account-jwks-uri`;
    - a `jwksFile`, which makes every key rotation a Server rollout.
  - keeping the old value as a second `--service-account-issuer` while a live
    cluster changes its issuer;
  - publishing a new signing key at least an hour before signing with it;
  - setting `deployers` to whoever applies the Runner manifests (for Flux, the
    kustomize-controller, or the ServiceAccount a Kustomization impersonates),
    and optionally narrowing `controllers` to the one username the controller
    manager uses.

### W5 — Ledger row

DL-440 in `docs/designs/DECISIONS.md` §Transport, in this PR.

### Test plan

- **W1:**
  - Each algorithm verifies against an `httptest` TLS issuer.
  - Rejected: wrong `aud`; unknown `iss`; empty `kid`; expired; future `nbf`;
    absent `exp`; `exp - iat > maxTokenLifetime`; wrong `sub`, namespace or
    ServiceAccount; missing pod or node claim; `alg: none`; HS256.
  - A `kid` miss re-fetches once, then not again within 5 minutes.
  - A rotated key verifies after the re-fetch.
  - A fetch error keeps the last good keys.
  - A JWKS 503 with no keys gives `ErrKeysUnavailable`; a bad signature does
    not.
  - Duplicate names, issuers differing only by a trailing `/`, and
    `maxTokenLifetime: 599s` fail to parse; `600s` parses.
  - Clusters A and B publish one shared key. With A's set loaded first and with
    B's first, tokens for both get `ErrKeysUnavailable`. Once B's set drops the
    key, A's tokens verify again.
- **W2:**
  - A JWS-shaped token never reaches the hash lookup (resolver spy), and gets
    `ErrWrongKind` with `want = SubjectAccount`.
  - Minted tokens still enroll.
  - A projected token on the account door gets `Unauthenticated`.
  - The door maps keys-unavailable to `CodeUnavailable` and a bad signature to
    `CodeUnauthenticated`.
  - `EnrollResponse.runner_id == subj.ID`.
  - Minting `a/b` fails; storing a token with `.` fails.
  - Startup is refused when a Runner row has `/`.
- **W3:**
  - Rewriting the file between two RPCs changes the token on the second.
  - A read error returns the cached token while unexpired, and fails after
    expiry.
  - Setting both token sources fails at start.
  - A `--mount` over the token directory is refused.
- **W4 render:**
  - `maxSurge: 0`;
  - `expirationSeconds` ≤ `maxTokenLifetime`;
  - `automountServiceAccountToken: false`;
  - no RoleBinding in the namespace;
  - the policy's default `controllers` (both usernames) and `deployers`.
- **Live smoke (W4):** on one node, the DaemonSet applied as a `deployers`
  identity is admitted, its Runner enrolls as `<cluster>/<node>`, and the
  decoded claim names match §Verification.
- **Threat proof (W4):**
  - Without the policy, an `edit` user runs `kubectl create token
    compass-runner --audience compass-runner --bound-object-kind Pod
    --bound-object-name <runner-pod>`, and the token enrolls.
  - With the policy, the same command is denied.
  - A user whose Role grants `create` and `update` on `daemonsets` is denied
    when creating a DaemonSet `evil` whose template names the Runner
    ServiceAccount, changing the `compass-runner` DaemonSet's `spec`, or
    deleting and recreating it. The same user is admitted for a metadata-only
    update of `compass-runner`, such as adding a label.
  - A server-side dry-run create of a Runner-ServiceAccount pod, impersonating
    each `controllers` username (`--as-group system:masters` so authorization
    passes), is admitted with a controller owner of DaemonSet `compass-runner`
    and denied with DaemonSet `evil` or a ReplicaSet as owner.

## Tasks

- [ ] W1 — verifier, key sets, cluster config, go-jose
- [ ] W2 — door dispatch, `--runner-clusters`, `EnrollResponse.runner_id`, mint guards
- [ ] W3 — Runner `TokenSource`, `--token-file`, Server-assigned ID, mount guard
- [ ] W4 — DaemonSet projected volume, `maxSurge: 0`, admission policy (R3), self-host docs
- [ ] W5 — DL-440 ledger row

## Open Questions

### OQ-1 [non-load-bearing] — Server-side per-node deny

The only Server-side lever is removing a whole cluster. Cutting one node
without cluster access would need a small addition: a per-node deny list in
the cluster file, or the deferred Enroll-time TokenReview.
