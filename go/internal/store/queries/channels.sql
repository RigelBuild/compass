-- Channel-domain queries (sqlc adoption T3, RIG-3034). These replace the inline
-- SQL literals that lived in internal/store/channels.go; the hand-written Store
-- methods keep their exact signatures and wrap these generated calls, mapping
-- the generated row structs back to the domain Channel / ChannelGroup.
--
-- The channel-visibility CTE (effective(id, eff_vis)) and the channel/group
-- visibility predicates are repeated per read because sqlc has no query-fragment
-- composition — they replace the former channels.go const fragments
-- (effectiveVisibilityCTE / viewerCTE / channelVisiblePredicate /
-- groupVisiblePredicate). The copies MUST stay textually identical so the stream
-- edge's single-id visibility check cannot drift from the list read (the
-- anti-drift guarantee the frozen record requires).

-- name: InsertChannelGroup :exec
INSERT INTO channel_groups (id, name, parent_group_id, owner_user_id, visibility)
VALUES ($1, $2, NULLIF($3, ''), $4, $5);

-- name: GetChannelGroupVisibility :one
SELECT visibility FROM channel_groups WHERE id = $1;

-- name: InsertChannel :exec
INSERT INTO channels (id, name, group_id, kind, post_policy, owner_account_id, mandatory_subscription)
VALUES ($1, $2, NULLIF($3, ''), $4, $5, NULLIF($6, ''), $7);

-- name: UpsertChannelMember :exec
INSERT INTO channel_members (channel_id, account_id, subscribed)
VALUES ($1, $2, $3)
ON CONFLICT (channel_id, account_id) DO UPDATE SET subscribed = EXCLUDED.subscribed;

-- name: DeleteChannelMember :execrows
DELETE FROM channel_members WHERE channel_id = $1 AND account_id = $2;

-- name: AgentOwnersByIDs :many
SELECT owner_user_id FROM agent_accounts WHERE account_id = ANY($1::text[]);

-- name: ChannelMemberExists :one
SELECT EXISTS (SELECT 1 FROM channel_members WHERE channel_id = $1 AND account_id = $2);

-- name: LockChannelMandatoryKind :one
SELECT mandatory_subscription, kind FROM channels WHERE id = $1 FOR UPDATE;

-- name: ConvertDMChannel :exec
UPDATE channels SET kind = $1, name = $2, group_id = NULL, mandatory_subscription = FALSE WHERE id = $3;

-- name: SubscribeConvertedDMParties :exec
UPDATE channel_members cm SET subscribed = TRUE
FROM agent_accounts aa WHERE aa.account_id = cm.account_id AND cm.channel_id = $1;

-- name: CountAgentMembers :one
SELECT COUNT(*) FROM channel_members cm
JOIN agent_accounts aa ON aa.account_id = cm.account_id
WHERE cm.channel_id = $1;

-- name: OwnerHasPresentAgent :one
SELECT EXISTS (
    SELECT 1 FROM agent_accounts aa
    JOIN channel_members cm ON cm.account_id = aa.account_id
    WHERE aa.owner_user_id = $1 AND cm.channel_id = $2 AND aa.account_id <> $1
);

-- name: LockChannelPolicy :one
SELECT mandatory_subscription, COALESCE(owner_account_id, '') AS owner_account_id
FROM channels WHERE id = $1 FOR UPDATE;

-- name: UpdateChannelPolicy :exec
UPDATE channels SET post_policy = $2, owner_account_id = NULLIF($3, ''), mandatory_subscription = $4 WHERE id = $1;

-- name: GetChannel :one
SELECT id, name, COALESCE(group_id, '') AS group_id, kind, post_policy,
       COALESCE(owner_account_id, '') AS owner_account_id, mandatory_subscription
FROM channels WHERE id = $1;

-- name: ChannelMembersByChannelIDs :many
SELECT channel_id, account_id, subscribed
FROM channel_members
WHERE channel_id = ANY($1::text[])
ORDER BY account_id;

-- name: ListChannelGroups :many
WITH RECURSIVE ancestry AS (
	SELECT id, parent_group_id, visibility AS min_vis
	FROM channel_groups
	UNION ALL
	SELECT a.id, g.parent_group_id, LEAST(a.min_vis, g.visibility)
	FROM ancestry a
	JOIN channel_groups g ON g.id = a.parent_group_id
),
effective AS (
	SELECT id, MIN(min_vis) AS eff_vis
	FROM ancestry
	GROUP BY id
),
viewer AS (
	SELECT owner_user_id AS uid FROM agent_accounts WHERE account_id = $1
	UNION ALL
	SELECT $1 AS uid
)
SELECT g.id, g.name, COALESCE(g.parent_group_id, '') AS parent_group_id, g.owner_user_id, g.visibility
FROM channel_groups g
JOIN effective e ON e.id = g.id
WHERE (e.eff_vis = 1 OR g.owner_user_id IN (SELECT uid FROM viewer))
ORDER BY g.name;

-- name: ChannelGroupVisibleTo :one
WITH RECURSIVE ancestry AS (
	SELECT id, parent_group_id, visibility AS min_vis
	FROM channel_groups
	UNION ALL
	SELECT a.id, g.parent_group_id, LEAST(a.min_vis, g.visibility)
	FROM ancestry a
	JOIN channel_groups g ON g.id = a.parent_group_id
),
effective AS (
	SELECT id, MIN(min_vis) AS eff_vis
	FROM ancestry
	GROUP BY id
),
viewer AS (
	SELECT owner_user_id AS uid FROM agent_accounts WHERE account_id = $1
	UNION ALL
	SELECT $1 AS uid
)
SELECT EXISTS (
	SELECT 1 FROM channel_groups g
	JOIN effective e ON e.id = g.id
	WHERE g.id = $2 AND (e.eff_vis = 1 OR g.owner_user_id IN (SELECT uid FROM viewer))
);

-- name: ListChannels :many
WITH RECURSIVE ancestry AS (
	SELECT id, parent_group_id, visibility AS min_vis
	FROM channel_groups
	UNION ALL
	SELECT a.id, g.parent_group_id, LEAST(a.min_vis, g.visibility)
	FROM ancestry a
	JOIN channel_groups g ON g.id = a.parent_group_id
),
effective AS (
	SELECT id, MIN(min_vis) AS eff_vis
	FROM ancestry
	GROUP BY id
)
SELECT c.id, c.name, COALESCE(c.group_id, '') AS group_id, c.kind, c.post_policy,
       COALESCE(c.owner_account_id, '') AS owner_account_id, c.mandatory_subscription
FROM channels c
WHERE (
		EXISTS (
		    SELECT 1 FROM channel_members cm
		    WHERE cm.channel_id = c.id AND cm.account_id = $1
		)
		OR (
		    c.kind = 0 AND c.group_id IS NOT NULL AND EXISTS (
		        SELECT 1 FROM effective e WHERE e.id = c.group_id AND e.eff_vis = 1
		    )
		)
	)
ORDER BY c.name;

-- name: ChannelVisibleTo :one
WITH RECURSIVE ancestry AS (
	SELECT id, parent_group_id, visibility AS min_vis
	FROM channel_groups
	UNION ALL
	SELECT a.id, g.parent_group_id, LEAST(a.min_vis, g.visibility)
	FROM ancestry a
	JOIN channel_groups g ON g.id = a.parent_group_id
),
effective AS (
	SELECT id, MIN(min_vis) AS eff_vis
	FROM ancestry
	GROUP BY id
)
SELECT EXISTS (
	SELECT 1 FROM channels c
	WHERE c.id = $2 AND (
		EXISTS (
		    SELECT 1 FROM channel_members cm
		    WHERE cm.channel_id = c.id AND cm.account_id = $1
		)
		OR (
		    c.kind = 0 AND c.group_id IS NOT NULL AND EXISTS (
		        SELECT 1 FROM effective e WHERE e.id = c.group_id AND e.eff_vis = 1
		    )
		)
	)
);

-- name: ChannelsByNameForViewer :many
WITH RECURSIVE ancestry AS (
	SELECT id, parent_group_id, visibility AS min_vis
	FROM channel_groups
	UNION ALL
	SELECT a.id, g.parent_group_id, LEAST(a.min_vis, g.visibility)
	FROM ancestry a
	JOIN channel_groups g ON g.id = a.parent_group_id
),
effective AS (
	SELECT id, MIN(min_vis) AS eff_vis
	FROM ancestry
	GROUP BY id
)
SELECT c.id, c.name, COALESCE(c.group_id, '') AS group_id, c.kind, c.post_policy,
       COALESCE(c.owner_account_id, '') AS owner_account_id, c.mandatory_subscription
FROM channels c
WHERE c.name = $2 AND (
		EXISTS (
		    SELECT 1 FROM channel_members cm
		    WHERE cm.channel_id = c.id AND cm.account_id = $1
		)
		OR (
		    c.kind = 0 AND c.group_id IS NOT NULL AND EXISTS (
		        SELECT 1 FROM effective e WHERE e.id = c.group_id AND e.eff_vis = 1
		    )
		)
	)
ORDER BY c.id;

-- name: InsertAgentWorkspaceIgnore :exec
INSERT INTO agent_workspaces (id, agent_account_id)
VALUES ($1, $2)
ON CONFLICT (agent_account_id) DO NOTHING;

-- name: GetAgentWorkspaceID :one
SELECT id FROM agent_workspaces WHERE agent_account_id = $1;
