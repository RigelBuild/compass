---
name: config-import-review
description: "Review an operator's existing agent config (skills, rules, MCP servers, settings) against Compass's shipped corpus and the live fleet bundle before import: classify each member redundant, conflicting, or complementary, explain what import will do, record the operator's decisions, and assemble the approved bundle for `compass agent-config push --dir`."
---

# Config-import review

Run this when the operator wants to bring their existing agent config into
Compass, for example from `compass-setup` Step 3. You read their corpus and
Compass's built-ins, and report what overlaps. **You propose; the operator
decides.** You never edit their source files, and you never push without an
explicit go-ahead.

The review works best on the host tier: you run on the operator's own machine,
so you can read their config where it lives, before anything is pushed. On a
container tier you can only read paths the operator mounts or copies in. Say so
and ask for the path rather than guessing.

## Step 1 — Ingest

1. Ask where the corpus lives. Common places: `~/.agents`, `~/.claude`,
   `~/.config/<tool>/`, a repo's `.agents/`, or an existing bundle dir.
2. Read the built-ins. There are two sources, and you need both:
   - **Compass's shipped corpus**: the `config/` tree of the Compass source,
     from the operator's checkout or the public repository. This is what
     Compass provides by default.
   - **The live fleet bundle**: under `$COMPASS_AGENT_CONFIG_MOUNT_PATH/current/`
     (default `/run/compass/agent-config/current/`). `compass agent-config
     show` prints its version and member names. It can differ from the shipped
     corpus, and it is what a push replaces. If there is no `current/`, confirm
     with `compass agent-config show`: an unconfigured fleet is a valid empty
     bundle, not a missing source.

   If you cannot read either source, stop and tell the operator. A candidate
   checked against only one baseline can look complementary when it is not.
3. Map each source file to the bundle member it would become. Only these are
   importable:

   | Member | Shape |
   | --- | --- |
   | `skills/<name>/…` | a directory; `SKILL.md` plus any files it uses |
   | `rules/<name>.md` or `.mdc` | flat, one file per rule |
   | `agents/<name>.md` | flat, one file per subagent |
   | `mcp/<name>.json` | one JSON file per server |
   | `extensions/<name>/…` | a directory |
   | `prompts/<role>/SYSTEM.md` | one prompt per role |
   | `profiles/<name>/profile.yml` | a YAML mapping |
   | `settings/config.yml` | the only settings file; YAML mapping |
   | `AGENTS.md`, `models.yml` | the only top-level files |

   `<name>` must match `^[A-Za-z0-9_-]+$`. Symlinks are refused, so copy the
   target. Anything that maps to no row (hooks, shell aliases, other tools'
   state) is **not importable**: list it and say why.

## Step 2 — Compare

Read each candidate in full and compare it with the built-ins. Do not stop at
filenames: a skill can do what a built-in does under a different name, and two
files can share a name but do different things. Put each candidate in one class:

- **Redundant**: a built-in already covers it. Name the built-in.
- **Conflicting**: it contradicts a built-in rule or skill, or Compass's
  semantics. Examples: a rule that says "merge when green" against the
  built-in `never-merge`, or a `git push` workflow against the built-in `jj`
  skill. Name both sides.
- **Complementary**: nothing built-in covers it. Safe to import as-is.

Give each finding one line of reasoning. If you are unsure, say so in the
finding. Do not pick a class to look decisive.

## Step 3 — Explain what import will do

Tell the operator what happens on import, before they decide:

- **The bundle is one fleet-wide singleton.** A push replaces the current
  bundle for every agent; it does not add to it. The pushed directory must
  therefore hold the built-ins they keep as well as the new members. Pushing
  needs an admin.
- **Rules and `AGENTS.md` add up.** Fleet rules load first, then the project's
  own; both apply and nothing is de-duplicated. The fleet `AGENTS.md` loads
  ahead of the checkout's `AGENTS.md` chain. A redundant rule is not harmful,
  but it is loaded twice.
- **Settings replace as a whole file.** `settings/config.yml` is delivered
  whole, with no key-level merge with another fleet file. Its keys override the
  project's settings. If both sides have one, the operator picks one or you
  merge the keys by hand for review.
- **Subagents and models are user-level.** A project's own `agents/` file of
  the same name still wins over the fleet one. `models.yml` has no project
  layer.
- **Credentials bounce at the door.** The store rejects credential keys in
  `settings/config.yml` and in the `settings` mapping of each
  `profiles/<name>/profile.yml`. In `models.yml` it rejects any non-null
  `providers.<name>.apiKey`, even an env-var reference, and literal header
  values (a header may name an env var or use a `!command`). List each member
  that will bounce and how to fix it, for example by moving the secret into
  the operator's secret store.
- **Same-name collisions overwrite.** If a candidate and a built-in map to the
  same member path, only one can ship.

## Step 4 — Decide and assemble

1. Write the report (format below) to a file outside the source corpus, for
   example `~/compass-import/report.md`, and show it.
2. Ask the operator to mark each finding **keep**, **drop**, or **rewrite**.
   Record each decision in the report. Defaults are not decisions: a finding
   with no mark is not imported.
3. Build the bundle in a new directory, for example `~/compass-import/bundle/`:
   - Start from the live fleet bundle in `current/`, since a push replaces it.
     If there is none, start from the shipped `config/` corpus. Copy only the
     members the operator keeps, and only bundle members: not the `version`
     file.
   - Copy each kept candidate to its member path. Write each rewrite as a new
     file there, never over the source file.
4. Show the final member list. Then, only after an explicit yes, run:

   ```sh
   compass agent-config push --dir ~/compass-import/bundle
   ```

   The command prints the new bundle version. If the door rejects the bundle,
   report the error, fix the member, and ask again before the next push.

## Report format

```markdown
# Config-import review — <source path>, <date>

Built-ins: shipped corpus <commit or path>; fleet bundle <version> (or "none")

| Member | Source | Class | Overlaps | Why | Decision |
| --- | --- | --- | --- | --- | --- |
| rules/merge-policy.md | ~/.agents/rules/merge-policy.md | conflicting | rules/never-merge.md | says merge when CI is green; built-in says the human merges | |

## Will bounce at the door
- models.yml: `providers.openai.apiKey` is set; remove it and supply the key outside the bundle

## Not importable
- ~/.claude/hooks/pre-commit.sh: hooks are not a bundle member
```
