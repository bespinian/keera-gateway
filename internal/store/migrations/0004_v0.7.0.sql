-- What v0.7.0 changes in the schema.

-- Organisations are billed at list price and the gateway no longer tracks
-- what the provider charges, so a bill line no longer records it.
ALTER TABLE billing_lines DROP COLUMN provider_micros;

-- Background jobs, such as the sandbox sweep and retention, run on one
-- replica per interval rather than on each. A job's row says when one last
-- took its turn.
CREATE TABLE job_runs (
    job    text PRIMARY KEY,
    ran_at timestamptz NOT NULL
);
