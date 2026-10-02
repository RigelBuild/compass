-- Repairs session rows that system-role wakes stamped with tenant_id '' before
-- WakeAgent ran under the agent's tenant. RLS hides '' rows from every tenant,
-- and a '' binding next to a real one makes SessionBindingTenant ambiguous.
-- Runs as compass_system: under FORCE RLS a non-superuser owner sees no rows.
SET LOCAL ROLE compass_system;

UPDATE agent_sessions s
   SET tenant_id = a.tenant_id
  FROM agent_accounts a
 WHERE s.tenant_id = '' AND a.account_id = s.agent_account_id;

UPDATE agent_placements p
   SET tenant_id = a.tenant_id
  FROM agent_accounts a
 WHERE p.tenant_id = '' AND a.account_id = p.agent_account_id;

UPDATE agent_session_transcript_entries e
   SET tenant_id = s.tenant_id
  FROM agent_sessions s
 WHERE e.tenant_id = '' AND s.session_id = e.session_id;

UPDATE agent_session_archive_segments g
   SET tenant_id = s.tenant_id
  FROM agent_sessions s
 WHERE g.tenant_id = '' AND s.session_id = g.session_id;

-- A binding is live routing state. Re-home a '' binding only when its tenant
-- holds no binding for that account or session; drop it otherwise.
UPDATE session_bindings b
   SET tenant_id = a.tenant_id
  FROM agent_accounts a
 WHERE b.tenant_id = '' AND a.account_id = b.agent_account_id
   AND NOT EXISTS (
       SELECT 1 FROM session_bindings o
        WHERE o.tenant_id = a.tenant_id
          AND (o.agent_account_id = b.agent_account_id OR o.session_id = b.session_id));

DELETE FROM session_bindings WHERE tenant_id = '';

RESET ROLE;
