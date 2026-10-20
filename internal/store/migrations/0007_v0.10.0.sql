-- What v0.10.0 changes in the schema.

-- A key an administrator leaves for its holder to activate has no secret
-- until they do, so it has no hash either. NULL never matches a presented
-- key, and UNIQUE allows any number of them.
ALTER TABLE api_keys ALTER COLUMN key_hash DROP NOT NULL;

-- What went wrong on the way to a request's answer, when it was answered all
-- the same: a router that could not decide and used its fallback, or a
-- destination that failed before another one answered. It is not an error,
-- so a request that carries only a note still counts as served.
ALTER TABLE usage_events ADD COLUMN note text;

-- The person's name as their identity provider gives it, refreshed at every
-- sign-in. Empty for someone who has not signed in through one yet.
ALTER TABLE users ADD COLUMN name text NOT NULL DEFAULT '';
ALTER TABLE signups ADD COLUMN name text NOT NULL DEFAULT '';

-- Daily totals of usage_events, one row per organisation, key, model, client
-- and UTC day. Reports over more than two days read these, so the request
-- log can be kept briefly and the charts still reach back months. A missing
-- project, person or client is '', not NULL, so every column is part of the
-- key. requests counts every request; refused (4xx) and failed (5xx) are
-- part of it.
CREATE TABLE usage_daily (
    day              date NOT NULL,
    org_id           text NOT NULL,
    key_id           text NOT NULL,
    alias            text NOT NULL,
    client           text NOT NULL,
    project_id       text NOT NULL,
    user_id          text NOT NULL,
    requests         bigint NOT NULL DEFAULT 0,
    refused          bigint NOT NULL DEFAULT 0,
    failed           bigint NOT NULL DEFAULT 0,
    input_tokens     bigint NOT NULL DEFAULT 0,
    output_tokens    bigint NOT NULL DEFAULT 0,
    cost_micros      bigint NOT NULL DEFAULT 0,
    list_cost_micros bigint NOT NULL DEFAULT 0,
    last_at          timestamptz NOT NULL,
    PRIMARY KEY (org_id, day, key_id, alias, client, project_id, user_id)
) WITH (fillfactor = 80); -- room for the upserts to update rows in place

-- Fill the totals from the request log that is still there. It runs before
-- the changes below, so their locks are held only briefly.
INSERT INTO usage_daily (day, org_id, key_id, alias, client, project_id, user_id,
    requests, refused, failed, input_tokens, output_tokens, cost_micros,
    list_cost_micros, last_at)
SELECT (ts AT TIME ZONE 'UTC')::date, org_id, COALESCE(key_id, ''), alias,
       COALESCE(client, ''), COALESCE(project_id, ''), COALESCE(user_id, ''),
       count(*), count(*) FILTER (WHERE status BETWEEN 400 AND 499),
       count(*) FILTER (WHERE status >= 500),
       sum(input_tokens), sum(output_tokens), sum(cost_micros),
       sum(list_cost_micros), max(ts)
FROM usage_events
GROUP BY 1, 2, 3, 4, 5, 6, 7;

-- Spans are now written as compact arrays in plain json, about a third of
-- the size of jsonb objects. Rows written before keep their jsonb, which is
-- read until retention removes them.
ALTER TABLE usage_events RENAME COLUMN spans TO spans_jsonb;
ALTER TABLE usage_events ADD COLUMN spans json;

-- Indexes the reports no longer need. Long windows read usage_daily, and
-- short ones are narrowed to one organisation by usage_events_org_ts_idx.
DROP INDEX usage_events_project_ts_idx;
DROP INDEX usage_events_alias_ts_idx;
-- The filters list now names its filters, so it uses the index that leads
-- with them.
DROP INDEX filter_runs_org_ts_idx;
-- One session's tool calls are a few minutes of tool_calls_org_ts_idx.
DROP INDEX tool_calls_key_ts_idx;

-- A session key is a hash that already includes the key, so it alone finds
-- one conversation, and each value is stored once.
DROP INDEX usage_events_session_idx;
CREATE INDEX usage_events_session_idx ON usage_events (session_key)
    WHERE session_key IS NOT NULL;

-- The request log's and the audit log's indexes, ascending. Descending, each
-- new row went to the front of its range, and every page split there left a
-- page half empty for good. The logs read them backwards for newest first.
DROP INDEX usage_events_org_id_idx;
CREATE INDEX usage_events_org_id_idx ON usage_events (org_id, id);
DROP INDEX usage_events_failed_idx;
CREATE INDEX usage_events_failed_idx ON usage_events (org_id, id)
    WHERE status >= 400 OR error IS NOT NULL;
DROP INDEX audit_log_org_id_idx;
CREATE INDEX audit_log_org_id_idx ON audit_log (org_id, id);
DROP INDEX audit_log_actor_idx;
CREATE INDEX audit_log_actor_idx ON audit_log (actor, id);
DROP INDEX audit_log_action_idx;
CREATE INDEX audit_log_action_idx ON audit_log (action, id);
-- A router's screen reads a window, which either order serves.
DROP INDEX usage_events_router_idx;
CREATE INDEX usage_events_router_idx ON usage_events (org_id, router, ts)
    WHERE router IS NOT NULL;

-- Request ids are only ever looked up whole, which a hash index does in
-- less space.
DROP INDEX usage_events_request_id_idx;
CREATE INDEX usage_events_request_id_idx ON usage_events USING hash (request_id)
    WHERE request_id IS NOT NULL;
DROP INDEX usage_events_client_request_id_idx;
CREATE INDEX usage_events_client_request_id_idx ON usage_events USING hash (client_request_id)
    WHERE client_request_id IS NOT NULL;

-- Billing rows are never deleted, so they stay in time order and a BRIN
-- index finds a month in a fraction of the space.
DROP INDEX billing_lines_ts_idx;
CREATE INDEX billing_lines_ts_idx ON billing_lines USING brin (ts);

-- Retention deletes a little every hour. Vacuum then runs at 2% dead rows
-- rather than 20%, so the space is reused before the tables grow.
ALTER TABLE usage_events SET (autovacuum_vacuum_scale_factor = 0.02);
ALTER TABLE filter_runs SET (autovacuum_vacuum_scale_factor = 0.02);
ALTER TABLE tool_calls SET (autovacuum_vacuum_scale_factor = 0.02);

-- A sign-up's refresh token is sealed against the identity now, so the
-- account's id no longer has to be fixed in advance. It is made when the
-- account is.
ALTER TABLE signups DROP COLUMN user_id;

-- Disabling a person updates their keys, and deleting one sets their keys'
-- user_id to NULL. Without an index both read every key, and every sandbox
-- adds one.
CREATE INDEX api_keys_user_id_idx ON api_keys (user_id) WHERE user_id IS NOT NULL;

-- usage_daily is never purged, and reports across every organisation narrow
-- it by day alone, which its primary key cannot serve.
CREATE INDEX usage_daily_day_idx ON usage_daily (day);

-- A fixed set of values gets a CHECK, as everywhere else. A sandbox's
-- isolation and backing get none: the first is copied from its class, and an
-- empty second means a sandbox the driver made.
ALTER TABLE sandbox_classes
    ADD CONSTRAINT sandbox_classes_isolation_check
        CHECK (isolation IN ('standard', 'isolated', 'vm')),
    ADD CONSTRAINT sandbox_classes_purposes_check
        CHECK (purposes <@ ARRAY['engineer', 'agent']);
ALTER TABLE sandboxes
    ADD CONSTRAINT sandboxes_purpose_check CHECK (purpose IN ('engineer', 'agent')),
    ADD CONSTRAINT sandboxes_state_check CHECK (state IN
        ('pending', 'ready', 'suspended', 'expired', 'failed', 'terminated'));
