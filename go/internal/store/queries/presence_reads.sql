-- Presence-component read queries (sqlc adoption T4, RIG-3034). These replace the
-- const-hoisted SQL in internal/store/presence_reads.go (it was never in the
-- inline-sql-gate allowlist — the gate is literal-at-callsite scoped and this
-- file passed its SQL as a const identifier). The hand-written Store methods keep
-- their signatures and error mapping.

-- name: AgentHasOpenAsk :one
SELECT EXISTS (
	SELECT 1 FROM messages
	WHERE author_account_id = $1
	  AND blocks @? '$[*] ? (@.kind == "ask" && (!exists(@.ask.answered) || @.ask.answered == false))'
);

-- name: SharesVisibleChannel :one
SELECT EXISTS (
	SELECT 1
	FROM channel_members cm1
	JOIN channel_members cm2 ON cm2.channel_id = cm1.channel_id
	WHERE cm1.account_id = $1
	  AND cm2.account_id = $2
);
