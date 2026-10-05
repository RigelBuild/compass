-- Agent tools read '/' as a path separator and address a group by sibling name,
-- so a name holds no '/' and is unique among one account's siblings. Owner ids
-- are global, so the key is owner/parent/name; tenant_id is not part of it.

-- Fail fast rather than queue every channel read behind a long transaction.
SET LOCAL lock_timeout = '5s';

-- Under FORCE RLS a non-superuser owner sees no rows, so the repair runs as
-- compass_system.
SET LOCAL ROLE compass_system;
-- Freeze writers from the repair through the index build, or a concurrent
-- create could commit a duplicate the repair never saw.
LOCK TABLE channel_groups IN SHARE ROW EXCLUSIVE MODE;

-- Rewrite separators first, then suffix duplicates. A valid original keeps its
-- name; each free suffix is found first so no existing name is overwritten.
DO $$
DECLARE
    rewritten_ids TEXT[];
    duplicate RECORD;
    candidate TEXT;
    suffix INTEGER;
BEGIN
    WITH rewritten AS (
        UPDATE channel_groups SET name = REPLACE(name, '/', '-')
         WHERE name LIKE '%/%'
        RETURNING id
    )
    SELECT COALESCE(array_agg(id), '{}') INTO rewritten_ids FROM rewritten;

    FOR duplicate IN
        SELECT id, owner_user_id, parent_group_id, name, duplicate_number
        FROM (
            SELECT id, owner_user_id, parent_group_id, name,
                   row_number() OVER (
                       PARTITION BY owner_user_id, COALESCE(parent_group_id, ''), name
                       ORDER BY id = ANY (rewritten_ids), id
                   ) AS duplicate_number
            FROM channel_groups
        ) AS ranked
        WHERE duplicate_number > 1
        ORDER BY owner_user_id, COALESCE(parent_group_id, ''), name, duplicate_number
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
                WHERE sibling.owner_user_id = duplicate.owner_user_id
                  AND COALESCE(sibling.parent_group_id, '') = COALESCE(duplicate.parent_group_id, '')
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

-- Top-level reserved names stay exempt: a planted wider look-alike must not
-- block the system's own owner-visible group.
CREATE UNIQUE INDEX channel_groups_owner_parent_name_key
    ON channel_groups (owner_user_id, COALESCE(parent_group_id, ''), name)
    WHERE NOT (parent_group_id IS NULL AND name IN ('__dm__', '__linear__', '__coordination__'));

-- The repair above leaves no '/' behind. This takes ACCESS EXCLUSIVE until the
-- migration commits; brief on a small table.
-- squawk-ignore constraint-missing-not-valid
ALTER TABLE channel_groups ADD CONSTRAINT channel_groups_name_no_slash CHECK (POSITION('/' IN name) = 0);
