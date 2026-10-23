-- What v0.11.0 changes in the schema.

-- A router is now a tree of steps that a client names in the 'model' field, as
-- it names a model. What used to be a router of its own, the choice between
-- destinations, is one kind of step in it. The old routers do not map onto
-- the new ones, so they go, and are written again; see "Upgrading from v0.10"
-- in docs/routers.md.
--
-- Their aliases stay on the allow-lists that named them, so a router written
-- again under the same alias is allowed where the old one was.
DROP TABLE routers;

-- The steps are one tree, kept whole: a step means nothing apart from the
-- steps around it, and the gateway reads every router into memory anyway. See
-- policy.Router.
--
-- The steps name filters and models by alias, not by foreign key. The control
-- plane refuses to delete one a router still uses.
CREATE TABLE routers (
    org_id       text NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    alias        text NOT NULL,
    display_name text NOT NULL DEFAULT '',
    description  text NOT NULL DEFAULT '',
    steps        jsonb NOT NULL CHECK (jsonb_typeof(steps) = 'object'),
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, alias)
);

-- usage_events keeps its columns, with a new meaning: router names the router
-- the request named, and router_outcome and router_ms are what its last route
-- step decided. Older rows name the old routers, so a new router of the same
-- alias reports their traffic as its own.

-- A guardrail may hold several rate limits and budgets, at most one of each
-- per period, such as a budget per day beside one per month. A rate limit may
-- count per day or month as well as per minute, as a router's does. Each list
-- is kept whole, as policy.Limits holds it; NULL sets none.
ALTER TABLE guardrails
    ADD COLUMN rate_limits jsonb CHECK (jsonb_typeof(rate_limits) = 'array'),
    ADD COLUMN budgets jsonb CHECK (jsonb_typeof(budgets) = 'array'),
    -- The guardrail that capped output tokens per request is gone.
    DROP COLUMN max_output_tokens;

-- The old limits were one of each: a rate limit per minute, and a budget.
UPDATE guardrails
SET rate_limits = jsonb_build_array(jsonb_strip_nulls(jsonb_build_object(
        'requests', NULLIF(rpm, 0), 'tokens', NULLIF(tpm, 0), 'period', 'minute')))
WHERE rpm > 0 OR tpm > 0;
UPDATE guardrails
SET budgets = jsonb_build_array(jsonb_build_object(
        'budget_micros', budget_micros, 'period', COALESCE(budget_period, 'month')))
WHERE budget_micros > 0;

ALTER TABLE guardrails
    DROP COLUMN rpm,
    DROP COLUMN tpm,
    DROP COLUMN budget_micros,
    DROP COLUMN budget_period;

-- Deleting an organisation or a project, and each retention batch, finds a
-- scope's spend rows by the part of scope_id before the '|' (scopeOwner in
-- tenants.go). Without an index on that expression, each scans the whole
-- table, which grows without bound while retention is off.
CREATE INDEX spend_owner_idx ON spend (scope_type, split_part(scope_id, '|', 1));
