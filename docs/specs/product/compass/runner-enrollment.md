# Runner enrollment CLI

Two operator binaries provision Runner enrollment credentials; both log to
stderr only and print their version with `--version`. `compass-runner --ca`
and `--server-name` set the Runner's TLS trust and checked name.

## Requirement: `compass-mint-runner-token` mints a Runner-subject token

`compass-mint-runner-token --runner-id <id>` SHALL mint a random 32-byte
bearer token (base64url, unpadded) whose subject is the Runner `<id>`. It
SHALL store only the token's SHA-256 hash, in the store named by `--database`
(default `$COMPASS_DATABASE_DSN`; it fails when neither is set). It is a
dedicated Runner-subject mint path, not the account door's `IssueToken`.
`--runner-id` is required and SHALL NOT contain `/`. Without `--token-out`,
every run mints a new token and writes the plaintext to stdout exactly once,
with a trailing newline. Nothing else goes to stdout, so a redirect captures
the credential cleanly.

### Scenario: A stdout mint emits the token once

- **Given** a reachable store
- **When** an operator runs `compass-mint-runner-token --runner-id r1`
- **Then** stdout holds exactly one token line, and the store resolves that
  token to the Runner subject `r1`.

## Requirement: `--token-out` writes the token file idempotently against the store

With `--token-out <file>`, the binary SHALL write the raw token (no trailing
newline) atomically at mode `0600`: write a temp file, sync it, then rename it
over the target. The parent directory must already exist. It SHALL write the
file before committing the new hash, and SHALL remove the file if that commit
fails, so a failed mint never leaves a new hash with no file. A failed rotation
of an existing file removes the file but does not revoke the earlier hash. When
the file already exists and `--force` is not given, the binary SHALL decide by
the existing token's state in the store, and SHALL leave the file's mode as it
found it, logging a warning when the mode grants any group or other permission:

| Existing token in the store | Action |
| -- | -- |
| Registered to this `--runner-id` | No-op |
| Revoked | No-op; the file is left in place |
| Unknown (the store was replaced) | Re-register that same token; the file is unchanged |
| Registered to another subject | Rotate: mint a new token and overwrite the file; log the prior subject |

`--force` SHALL always mint and overwrite. It has no effect without
`--token-out`.

### Scenario: A replaced store heals without rotating the Runner's credential

- **Given** a token file from an earlier `--token-out` run, and a store that no
  longer holds its hash
- **When** the operator runs the same command again
- **Then** the file's bytes are unchanged, and the store again resolves that
  token to the Runner subject.

## Requirement: `compass-gen-cert` writes a self-signed network-door keypair

`compass-gen-cert --cert-out <file> --key-out <file>` SHALL generate an
ECDSA P-256 certificate that is self-signed and marked as a CA, so the same PEM
serves as the Server's `--tls-cert` and the Runner's `--ca`. Its SANs come from
`--hosts` (comma-separated; default `127.0.0.1,::1,localhost`; IP literals
become IP SANs, other values DNS SANs; at least one entry is required). It is
valid for `--validity` (default one year; a non-positive value uses the
default), backdated one hour for clock skew. Both output flags are required,
and their parent directories must already exist. The binary SHALL reject two
flags that name the same file, including through a symlink, so the certificate
never overwrites the key. The key SHALL be written at
`0600` and the certificate at `0644`, as it is a public trust anchor. Each file
is written atomically, and the key first, so a write failure never leaves a new
certificate beside a missing key. When both files already exist the binary
SHALL skip generation unless `--force` is given, and SHALL leave both files'
modes as it found them, logging a warning when the key's mode grants any group
or other permission. When only one exists, it SHALL regenerate both.

### Scenario: A re-run keeps a live keypair

- **Given** a certificate and key written by an earlier run
- **When** `compass-gen-cert` runs again without `--force`
- **Then** both files are unchanged, so a running Server and Runner need not
  re-enroll.

## Requirement: `compass-runner --ca` trusts a named certificate instead of the system roots

`compass-runner --ca <pem>` (default `$COMPASS_RUNNER_CA`) SHALL trust only
that PEM's certificates, at TLS 1.3 minimum; unset, the system roots. After
the backend preflight, an unreadable file or one with no PEM certificate SHALL
stop startup.

### Scenario: A Runner trusting another certificate is refused

- **Given** a door serving a `compass-gen-cert` certificate
- **When** the Runner's `--ca` names another
- **Then** `Enroll` fails the TLS handshake

## Requirement: `compass-runner --server-name` sets the name checked on the Server's certificate

`--server-name` (default `$COMPASS_SERVER_NAME`) SHALL replace the `--server`
host as the name checked, with the same roots; unset, the `--server` host.

### Scenario: A Runner checks the public name

- **Given** an in-cluster address and a trusted cert naming only
  `compass.example.test`
- **When** the Runner sets `--server-name compass.example.test`
- **Then** it enrolls
