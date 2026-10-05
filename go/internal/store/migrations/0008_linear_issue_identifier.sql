-- Store the Linear forge coordinate; the issue UUID cannot drive ownership lookup.
ALTER TABLE linear_agent_sessions ADD COLUMN linear_issue_identifier TEXT;
