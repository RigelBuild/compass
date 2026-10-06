-- Authorization-probe queries (sqlc adoption T6, RIG-3034). These replace the
-- inline SQL literals in internal/store/authz.go; the hand-written helpers keep
-- their signatures and the not-found/forbidden merge, wrapping these EXISTS
-- probes. requireChannelMember / isChannelMember wrap ChannelParticipant
-- (channels.sql); the topic-keyed and creation probes live here.

-- name: TopicChannelMemberExists :one
-- Stored-row membership on the topic's channel; IsTopicChannelMember uses TopicChannelParticipant.
SELECT EXISTS (SELECT 1 FROM topics t JOIN channel_members cm ON cm.channel_id = t.channel_id WHERE t.id = $1 AND cm.account_id = $2);

-- name: TopicChannelParticipant :one
-- Feeds IsTopicChannelMember: ChannelParticipant on the channel that owns the
-- topic. UNION stops the agent-parent walk on a cycle, as there.
WITH RECURSIVE tc AS (
    SELECT t.channel_id FROM topics t WHERE t.id = $1
), chain AS (
    SELECT aa.account_id, aa.parent_agent_id
    FROM agent_accounts aa
    WHERE aa.account_id = $2
      AND EXISTS (SELECT 1 FROM channels c JOIN tc ON c.id = tc.channel_id WHERE c.membership_mode = 1)
    UNION
    SELECT a.account_id, a.parent_agent_id
    FROM agent_accounts a
    JOIN chain ch ON a.account_id = ch.parent_agent_id
)
SELECT EXISTS (
    SELECT 1 FROM tc JOIN channel_members cm ON cm.channel_id = tc.channel_id
    WHERE cm.account_id = $2
) OR EXISTS (
    SELECT 1 FROM channels c JOIN tc ON c.id = tc.channel_id
    WHERE c.membership_mode = 1 AND (
        c.parent_agent_id IN (SELECT ch.account_id FROM chain ch)
        OR $2 = (SELECT aa.owner_user_id FROM agent_accounts aa
                 WHERE aa.account_id = c.parent_agent_id)
    )
);

-- name: GroupCreateAuthorized :one
-- Feeds requireGroupCreateAuthz: owner, agent-owner, same namespace, or SHARED-visibility group.
SELECT EXISTS (
        SELECT 1 FROM channel_groups g
        WHERE g.id = $1 AND (
              g.owner_user_id = $2
           -- Gates on BARE g.visibility = SHARED, not effective
           -- (MIN-over-ancestry) visibility. Sound only because groups are
           -- immutable post-create: the sole channel_groups mutation is the
           -- CreateChannelGroup INSERT (no UpdateChannelGroup / re-parent
           -- RPC), and CreateChannelGroup enforces child <= parent ceiling,
           -- so bare-SHARED implies effective-SHARED. If a re-parent or
           -- visibility-update RPC ever lands, switch this to
           -- effectiveVisibilityCTE or it becomes a create-leak (a
           -- bare-SHARED group nested under an OWNER parent would authorize
           -- creates it should not).
           OR g.visibility = $3
           OR g.owner_user_id = (SELECT owner_user_id FROM agent_accounts WHERE account_id = $2)
           -- A group an agent created lives in its owner's namespace.
           OR g.namespace_owner_id = COALESCE((SELECT owner_user_id FROM agent_accounts WHERE account_id = $2), $2)));

-- name: AgentAttachAuthorized :one
-- Feeds requireAgentAttachAuthz: the agent's owner, or an agent of that owner.
SELECT EXISTS (
    SELECT 1 FROM agent_accounts target
    WHERE target.account_id = $1 AND (
        target.owner_user_id = $2
        OR target.owner_user_id = (
            SELECT owner_user_id FROM agent_accounts WHERE account_id = $2
        )
    )
);

-- name: AgentWorkspaceVisible :one
-- Feeds isAgentWorkspaceVisible: membership on the agent's home channel.
SELECT EXISTS (
        SELECT 1 FROM agent_accounts ag
        JOIN channel_members cm ON cm.channel_id = ag.home_channel_id AND cm.account_id = $1
        WHERE ag.account_id = $2);
