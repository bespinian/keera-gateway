-- What v0.13.0 changes in the schema.

-- The app that issued a key and holds its secret: 'agents' for a key Keera
-- Agents issued for an agent, empty for a key a person issued. People may
-- revoke such a key but not rotate it, since the new secret would reach
-- nobody. See docs/agents.md.
ALTER TABLE api_keys ADD COLUMN issued_by text NOT NULL DEFAULT '';

-- Sandboxes are gone from the gateway; Keera Agents took their place. The
-- keys of sandboxes still running are revoked, so none outlives the feature.
-- Machines still running in the cluster are the operator's to remove.
UPDATE api_keys SET revoked_at = now()
    WHERE revoked_at IS NULL AND id IN (SELECT key_id FROM sandboxes);
DROP TABLE sandboxes;
DROP TABLE sandbox_classes;
DELETE FROM job_runs WHERE job = 'sandbox-sweep';
ALTER TABLE guardrails
    DROP COLUMN max_sandboxes,
    DROP COLUMN max_sandbox_ttl_seconds,
    DROP COLUMN sandbox_classes,
    DROP COLUMN max_sandbox_cpu_millis,
    DROP COLUMN max_sandbox_memory_mib,
    DROP COLUMN allowed_repos;
