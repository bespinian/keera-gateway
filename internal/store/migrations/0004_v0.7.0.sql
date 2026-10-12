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

-- A request's id, and the client's own when it sent one, so a request someone
-- reports by either id can be found. Rows written before this have neither.
-- Each is indexed on its own, as a search by id ignores the time window.
ALTER TABLE usage_events ADD COLUMN request_id text;
ALTER TABLE usage_events ADD COLUMN client_request_id text;
CREATE INDEX usage_events_request_id_idx ON usage_events (request_id)
    WHERE request_id IS NOT NULL;
CREATE INDEX usage_events_client_request_id_idx ON usage_events (client_request_id)
    WHERE client_request_id IS NOT NULL;
