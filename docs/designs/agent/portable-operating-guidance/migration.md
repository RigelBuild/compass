# Portable operating guidance migration

This bundle carries provider-neutral workflow guidance into Compass's existing
skills and rules. The management-tree model, async channel/topic communication,
and operator merge gate remain authoritative.

The migration reviewed the source guidance and deliberately omitted flat-peer
coordination and its transport, provider-specific credentials and commands, and
provider-specific infrastructure and merge policy. The exclusions frozen by
DL-143 remain unchanged; adding an exclusion requires a separate decision.

Extension points are the configured issue and review tools, the CI-engine hooks
in a later skill bundle, and user configuration for infrastructure providers.
The portable skills define behavior and evidence requirements without selecting
an adapter.

Issue lifecycle guidance remains in `own-your-issue.md`; the dedicated skill is
deferred under BC-3 until its issue and PR tools are available. The ownership
rule stays behavioral and names no unavailable tool.
