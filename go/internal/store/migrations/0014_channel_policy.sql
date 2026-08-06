-- ── Channel policy ───────────────────────────────────────────────────────────
-- The manager-comms substrate's channel-policy fields (SEA-1722 T4, design
-- record T4): who may post, who owns/operates the channel, and whether
-- membership implies a non-togglable subscription. All three default to the
-- pre-substrate behavior, so an existing channel keeps OPEN posting, no owner,
-- and per-member opt-in subscription until SetChannelPolicy — the ONLY mutation
-- path for these fields after creation — sets them.
--
-- post_policy mirrors the ChannelPostPolicy enum (comms.proto): 0 = OPEN (any
-- member may post, the default), 1 = OWNER_ONLY (only owner_account_id may
-- post). owner_account_id is the owner/operator account for policy operations;
-- NULL/empty leaves the channel unowned (the only legal state when OPEN).
-- mandatory_subscription, when true, makes every member a delivery target
-- regardless of its per-member channel_members.subscribed flag (the D1 read-side
-- disjunct); SetChannelPolicy transactionally seeds each newly-mandatory
-- member's delivery cursor so a flip never mints an un-seeded delivery target
-- (the fail-DANGEROUS D2 hazard).
ALTER TABLE channels
    ADD COLUMN post_policy            SMALLINT NOT NULL DEFAULT 0 CHECK (post_policy IN (0, 1)),
    ADD COLUMN owner_account_id       TEXT REFERENCES accounts (id) ON DELETE RESTRICT,
    ADD COLUMN mandatory_subscription BOOLEAN NOT NULL DEFAULT FALSE;
