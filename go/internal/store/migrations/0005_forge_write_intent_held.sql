-- A held intent reserves a request key for one target and sends nothing. It is
-- outside the PR-head index and the sweep, and becomes pending before a create.
ALTER TABLE forge_write_intents DROP CONSTRAINT forge_write_intents_state_check;
ALTER TABLE forge_write_intents ADD CONSTRAINT forge_write_intents_state_check
    CHECK (state IN ('held', 'pending', 'done', 'failed', 'lost')) NOT VALID;
