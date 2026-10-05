-- Store the Linear forge coordinate; the issue UUID cannot drive ownership lookup.

-- A nullable add with no default is metadata-only, but its ACCESS EXCLUSIVE lock
-- queues behind any long transaction on a live table; fail fast instead of stalling.
SET LOCAL lock_timeout = '5s';

ALTER TABLE linear_agent_sessions ADD COLUMN linear_issue_identifier TEXT;
