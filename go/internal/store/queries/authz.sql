-- Authorization-probe queries (sqlc adoption T6, RIG-3034). These replace the
-- inline SQL literals in internal/store/authz.go; the hand-written helpers keep
-- their signatures and the not-found/forbidden merge, wrapping these EXISTS
-- probes (each returns a bare bool). requireChannelMember / isChannelMember reuse
-- ChannelMemberExists (channels.sql) — the statement is textually identical — so
-- only the three probes without an existing query live here.

-- name: TopicChannelMemberExists :one
-- Feeds IsTopicChannelMember: membership on the channel that owns the topic.
SELECT EXISTS (SELECT 1 FROM topics t JOIN channel_members cm ON cm.channel_id = t.channel_id WHERE t.id = $1 AND cm.account_id = $2);

-- name: GroupCreateAuthorized :one
-- Feeds requireGroupCreateAuthz: owner, agent-owner, or SHARED-visibility group.
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
           OR g.owner_user_id = (SELECT owner_user_id FROM agent_accounts WHERE account_id = $2)));

-- name: AgentWorkspaceVisible :one
-- Feeds isAgentWorkspaceVisible: membership on the agent's home channel.
SELECT EXISTS (
        SELECT 1 FROM agent_accounts ag
        JOIN channel_members cm ON cm.channel_id = ag.home_channel_id AND cm.account_id = $1
        WHERE ag.account_id = $2);
