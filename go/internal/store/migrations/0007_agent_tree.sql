-- 0007_agent_tree: the agent tree's organizing spine (Record C, T2/T3). Give
-- agent_accounts a parent edge so the fleet's reporting structure — who spawned
-- or supervises whom — becomes a model fact every surface (sidebar, board)
-- re-derives from, replacing the hand-curated folder tree.
--
-- Nullable, NULL = a root agent, mirroring channel_groups.parent_group_id
-- (0001_init.sql:52,57): a self-referential FK on the same table, ON DELETE
-- RESTRICT so a parent cannot be orphaned out from under its children, plus an
-- index on the edge for the "children of this parent" read direction.
--
-- The edge is set at creation (to the spawning agent's account id, or a
-- user-supplied parent on CreateAgent) and editable via ReparentAgent; same-
-- owner and no-cycle are validated server-side on every write, not by the
-- schema — the FK only guarantees the referent exists and is an agent account.
ALTER TABLE agent_accounts
    ADD COLUMN parent_agent_id TEXT REFERENCES agent_accounts (account_id) ON DELETE RESTRICT;

CREATE INDEX agent_accounts_parent_idx ON agent_accounts (parent_agent_id);
