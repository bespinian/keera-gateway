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
    day                 date NOT NULL,
    org_id              text NOT NULL,
    key_id              text NOT NULL,
    alias               text NOT NULL,
    client              text NOT NULL,
    project_id          text NOT NULL,
    user_id             text NOT NULL,
    requests            bigint NOT NULL DEFAULT 0,
    refused             bigint NOT NULL DEFAULT 0,
    failed              bigint NOT NULL DEFAULT 0,
    input_tokens        bigint NOT NULL DEFAULT 0,
    output_tokens       bigint NOT NULL DEFAULT 0,
    cached_input_tokens bigint NOT NULL DEFAULT 0,
    cache_write_tokens  bigint NOT NULL DEFAULT 0,
    cost_micros         bigint NOT NULL DEFAULT 0,
    list_cost_micros    bigint NOT NULL DEFAULT 0,
    last_at             timestamptz NOT NULL,
    PRIMARY KEY (org_id, day, key_id, alias, client, project_id, user_id)
) WITH (fillfactor = 80); -- room for the upserts to update rows in place

-- Fill the totals from the request log that is still there.
INSERT INTO usage_daily (day, org_id, key_id, alias, client, project_id, user_id,
    requests, refused, failed, input_tokens, output_tokens, cached_input_tokens,
    cache_write_tokens, cost_micros, list_cost_micros, last_at)
SELECT (ts AT TIME ZONE 'UTC')::date, org_id, COALESCE(key_id, ''), alias,
       COALESCE(client, ''), COALESCE(project_id, ''), COALESCE(user_id, ''),
       count(*), count(*) FILTER (WHERE status BETWEEN 400 AND 499),
       count(*) FILTER (WHERE status >= 500),
       sum(input_tokens), sum(output_tokens), sum(cached_input_tokens),
       sum(cache_write_tokens), sum(cost_micros), sum(list_cost_micros), max(ts)
FROM usage_events
GROUP BY 1, 2, 3, 4, 5, 6, 7;
