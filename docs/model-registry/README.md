# Model registry: recommended models and day-1 defaults

A profile names a model by its **stable name**, such as `claude-opus-4-8`.
The Server's model registry maps each stable name to an ordered list of
`(provider, model id)` candidates. The gateway uses the first candidate whose
provider you hold a credential for. A selector that contains a `/`, such as
`anthropic/claude-opus-4-8`, skips the registry and names one exact model.
Until the gateway resolver ships, the Server stores the registry but requests
do not yet route through it.

The design is in
[stable-name routing](../designs/platform/compass-stable-name-routing/design.md).

## Day-1 defaults

[`day-1.json`](./day-1.json) is the day-1 registry. It is a complete
`PutModelRegistry` request body. Every model id in it exists in the
`@oh-my-pi/pi-catalog` 18.0.11 model catalog that Compass pins.

| Stable name | Candidates, in order |
| --- | --- |
| `claude-opus-4-8` | `anthropic` → `openrouter` → `amazon-bedrock` |
| `gpt-5-5` | `openai-codex` → `openai` → `openrouter` |
| `gemini-3-1-pro` | `google` → `openrouter` |

The first candidate of each entry is a day-1 gateway provider: `anthropic`
(OAuth or API key), `openai-codex` (ChatGPT OAuth), `openai` (API key), or
`google` (API key). OpenRouter and Bedrock are alternates. They let one
OpenRouter key or one AWS account serve a stable name when you hold no
first-party credential for it.

## Recommended model per role

These are recommendations, not the shipped `default` profile, which picks its
own models. Pick the row for the credentials you hold. The value goes in a
profile's `models.manager` or `models.agents.<name>` selector. Add `:high` or
another reasoning level after the name when you want one.

| Provider you hold | Manager roles (supervisor, owner, manager) | Implementer subagent |
| --- | --- | --- |
| OpenAI (ChatGPT OAuth or API key) | `gpt-5-5` | `gpt-5-5` |
| Anthropic (OAuth or API key) | `claude-opus-4-8` | `claude-opus-4-8` |
| Google (API key) | `gemini-3-1-pro` | `gemini-3-1-pro` |
| OpenRouter only | `gpt-5-5` | `gpt-5-5` |
| Bedrock only | `claude-opus-4-8` | `claude-opus-4-8` |
| OpenAI and Anthropic | `gpt-5-5` | `gpt-5-5` |

These values follow the fleet's current practice: the OpenAI Codex tier first,
then the Anthropic Opus tier. Bedrock carries no `gpt-5-5` candidate, so a
Bedrock-only fleet uses the Opus tier. The goal these values serve is
long-running coding agents, where a low hallucination rate counts as much as
raw capability.

Per-role model evaluations will replace these values with measured numbers.
When they land, the change is a registry write (below), not a new release.

## Applying and updating the defaults

Registry defaults are data in the Server store. You change them with an
operator `PutModelRegistry` RPC write, not with a Compass release.

The Unix socket is the admin credential. Only the user that runs the Server
can open it, so the write runs as that user on the Server host, for example
`sudo -u compass` under the [systemd unit](../self-host.md#running-under-systemd).

Set `SOCK` to the Server's socket. That is its `--socket` value, or by default
`$XDG_RUNTIME_DIR/compass/server.sock` in the Server's environment, or
`~/.compass/server.sock` in its home when `XDG_RUNTIME_DIR` is unset.

Set `REF` to the source of the Server you run: its release tag, or for a Nix
flake install the commit that `nix profile list` shows. If the seed is absent
at that ref (it is not in `v0.3.0` or earlier, or in older flake pins), use
`main`. Fetch the seed as yourself and pipe it straight into the write, which
runs as the Server user:

```console
curl -fsSL "https://raw.githubusercontent.com/RigelBuild/compass/$REF/docs/model-registry/day-1.json" |
    sudo -u compass curl --fail-with-body --unix-socket "$SOCK" \
        -H 'Content-Type: application/json' \
        --data-binary @- \
        http://localhost/compass.v1.CompassService/PutModelRegistry
```

A failed fetch sends an empty body, which the Server rejects. From a
repository checkout, `docs/model-registry/day-1.json` is the same file.

`expectedVersion` is a compare-and-set guard. `0` writes the first registry
only. To update a registry that already exists, read its current version with
`GetModelRegistry` (body `{}`), set `expectedVersion` to that value, and send
the edited body. A stale version fails with `aborted`: read again and retry.
The Server rejects a write that removes a stable name a published profile
still uses.
