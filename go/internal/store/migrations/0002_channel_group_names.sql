-- Agent tools read '/' as a path separator and address a group by sibling name,
-- so a name holds no '/' and is unique among one namespace's siblings. An
-- agent's groups live in its owner's namespace. Owner ids are global, so the key
-- is namespace/parent/name; tenant_id is not part of it.

-- Fail fast rather than queue every channel read behind a long transaction.
SET LOCAL lock_timeout = '5s';

-- The default exists only for the backfill and is dropped below; the trigger
-- fills the column for writers that omit it. ADD COLUMN holds ACCESS EXCLUSIVE
-- until commit, so no create can slip a duplicate past the repair.
ALTER TABLE channel_groups ADD COLUMN namespace_owner_id TEXT NOT NULL DEFAULT '';

-- Under FORCE RLS a non-superuser owner sees no rows, so the repair runs as
-- compass_system.
SET LOCAL ROLE compass_system;
-- Backfill namespaces, rewrite separators, then suffix duplicates. A valid
-- original keeps its name; each free suffix is found first so no existing name
-- is overwritten.
DO $$
DECLARE
    rewritten_ids TEXT[];
    duplicate RECORD;
    candidate TEXT;
    suffix INTEGER;
BEGIN
    UPDATE channel_groups AS g
       SET namespace_owner_id = coalesce(
           (SELECT a.owner_user_id FROM agent_accounts AS a WHERE a.account_id = g.owner_user_id),
           g.owner_user_id);

    WITH rewritten AS (
        UPDATE channel_groups SET name = replace(name, '/', '-')
         WHERE name LIKE '%/%'
        RETURNING id
    )
    SELECT coalesce(array_agg(id), '{}') INTO rewritten_ids FROM rewritten;

    FOR duplicate IN
        SELECT id, namespace_owner_id, parent_group_id, name, duplicate_number
        FROM (
            SELECT id, namespace_owner_id, parent_group_id, name,
                   row_number() OVER (
                       PARTITION BY namespace_owner_id, coalesce(parent_group_id, ''), name
                       ORDER BY id = ANY (rewritten_ids), id
                   ) AS duplicate_number
            FROM channel_groups
        ) AS ranked
        WHERE duplicate_number > 1
        ORDER BY namespace_owner_id, coalesce(parent_group_id, ''), name, duplicate_number
    LOOP
        IF duplicate.parent_group_id IS NULL
           AND duplicate.name IN ('__dm__', '__linear__', '__coordination__') THEN
            CONTINUE;
        END IF;
        suffix := duplicate.duplicate_number;
        LOOP
            candidate := duplicate.name || '-' || suffix::text;
            EXIT WHEN NOT EXISTS (
                SELECT 1
                FROM channel_groups AS sibling
                WHERE sibling.namespace_owner_id = duplicate.namespace_owner_id
                  AND coalesce(sibling.parent_group_id, '') = coalesce(duplicate.parent_group_id, '')
                  AND sibling.name = candidate
                  AND sibling.id <> duplicate.id
            );
            suffix := suffix + 1;
        END LOOP;
        UPDATE channel_groups SET name = candidate WHERE id = duplicate.id;
    END LOOP;
END $$;

-- DDL runs as the table owner, not the system role.
RESET ROLE;

ALTER TABLE channel_groups ALTER COLUMN namespace_owner_id DROP DEFAULT;

-- Top-level reserved names stay exempt: a planted wider look-alike must not
-- block the system's own owner-visible group.
CREATE UNIQUE INDEX channel_groups_owner_parent_name_key
    ON channel_groups (namespace_owner_id, coalesce(parent_group_id, ''), name)
    WHERE NOT (parent_group_id IS NULL AND name IN ('__dm__', '__linear__', '__coordination__'));

-- The repair above leaves no '/' behind. This takes ACCESS EXCLUSIVE until the
-- migration commits; brief on a small table.
-- squawk-ignore constraint-missing-not-valid
ALTER TABLE channel_groups ADD CONSTRAINT channel_groups_name_no_slash CHECK (position('/' IN name) = 0);

-- Binaries from before this migration insert without the column during a
-- rolling deploy, so derive it from the creator the way the backfill does.
CREATE FUNCTION channel_groups_fill_namespace() RETURNS TRIGGER
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.namespace_owner_id IS NULL THEN
        NEW.namespace_owner_id := coalesce(
            (SELECT a.owner_user_id FROM agent_accounts AS a WHERE a.account_id = NEW.owner_user_id),
            NEW.owner_user_id);
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER channel_groups_fill_namespace BEFORE INSERT ON channel_groups
    FOR EACH ROW EXECUTE FUNCTION channel_groups_fill_namespace();
