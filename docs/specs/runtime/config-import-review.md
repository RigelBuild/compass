# Host-tier first run: the config-import review

The [host tier](runner-tiers.md#host)'s first useful job is to review the
operator's existing agent config before it is imported. The built-in
`config-import-review` skill (`config/skills/config-import-review/SKILL.md`)
runs it:

1. **Ingest.** The agent reads the operator's corpus where it lives (for
   example `~/.agents` or `~/.claude`) and maps each file to the bundle member
   it would become. Files with no member (hooks, other tools' state) are
   listed as not importable.
2. **Compare.** It reads each candidate against both baselines: Compass's
   shipped `config/` corpus and the live fleet bundle, which can be empty or
   differ from it. It classes each candidate **redundant**, **conflicting**, or
   **complementary**, naming the built-in it overlaps. Overlap is judged on
   content, not filenames. With no readable baseline it stops instead of
   guessing.
3. **Explain.** It states what import will do: the bundle is one fleet-wide
   singleton that a push replaces; rules and `AGENTS.md` add to the project's
   own; `settings/config.yml` replaces as a whole file; credential-bearing
   members bounce at the store door.
4. **Decide.** The operator marks each finding keep, drop, or rewrite. The
   agent writes the report, assembles the approved members in a new bundle
   dir, and runs `compass agent-config push --dir <dir>` only after an
   explicit yes.

The agent proposes; the operator disposes. The review never edits the source
corpus. On a container tier the same skill works only on paths the operator
mounts or copies in, which is why the host tier is where it fits.
