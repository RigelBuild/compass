ALTER TABLE forge_write_intents
    ADD COLUMN base_ref TEXT,
    ADD COLUMN forge_url TEXT NOT NULL DEFAULT '',
    ADD COLUMN issue_link_forge_provider SMALLINT,
    ADD COLUMN issue_link_forge_host TEXT,
    ADD COLUMN issue_link_repo TEXT,
    ADD COLUMN issue_link_number BIGINT;

ALTER TABLE forge_write_intents
    ADD CONSTRAINT forge_write_intents_issue_link_complete_check
    CHECK (
        (issue_link_forge_provider IS NULL AND issue_link_forge_host IS NULL AND issue_link_repo IS NULL AND issue_link_number IS NULL)
        OR
        (issue_link_forge_provider IS NOT NULL AND issue_link_forge_host IS NOT NULL AND issue_link_repo IS NOT NULL AND issue_link_number IS NOT NULL)
    ) NOT VALID;
