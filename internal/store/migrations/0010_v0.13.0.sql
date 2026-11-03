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

-- A rerank model scores documents against a query, on /api/v1/rerank and
-- /api/v2/rerank.
ALTER TABLE models DROP CONSTRAINT models_kind_check,
    ADD CONSTRAINT models_kind_check
        CHECK (kind IN ('chat', 'completion', 'embedding', 'rerank', 'decision'));

-- A fee an operator charges an organisation every month or year, such as for a
-- dedicated model. It is charged to the saved card and not credited. See
-- docs/billing.md.
CREATE TABLE recurring_charges (
    id           text PRIMARY KEY,
    org_id       text NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    -- What it pays for, on the payment page and the receipt.
    name         text NOT NULL,
    -- Each period's price in CHF, without VAT and fee.
    micros       bigint NOT NULL CHECK (micros > 0),
    every        text NOT NULL CHECK (every IN ('month', 'year')),
    -- The first period starts at midnight UTC on this day. Period n starts n
    -- months or years later, counted from here so the 31st stays the 31st.
    starts_on    date NOT NULL,
    -- How many periods are paid. The next one is due when it starts.
    paid_periods int NOT NULL DEFAULT 0,
    -- Why charging the saved card last failed. Until it is cleared, the card
    -- is not tried again.
    error        text,
    actor        text NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX recurring_charges_org_idx ON recurring_charges (org_id);

-- A payment of one period of a recurring charge. charge_id has no foreign
-- key: the payment is kept when the charge is removed.
ALTER TABLE payments
    DROP CONSTRAINT payments_kind_check,
    ADD CONSTRAINT payments_kind_check CHECK (kind IN ('topup', 'auto', 'grant', 'charge')),
    ADD COLUMN charge_id    text,
    -- The charge's name when it was paid.
    ADD COLUMN item         text,
    ADD COLUMN period_start timestamptz,
    ADD COLUMN period_end   timestamptz;
-- At most one charge of the saved card per recurring charge is under way.
CREATE UNIQUE INDEX payments_one_charge_idx ON payments (charge_id)
    WHERE kind = 'charge' AND state = 'pending' AND NOT save_card;

-- Where a scope's models may run, such as ch or onprem, so prompts stay there.
-- NULL inherits, like allowed_models; an empty list allows nothing.
ALTER TABLE guardrails ADD COLUMN allowed_locations text[];

-- A billing line can be the gateway fee on a model the deployment does not
-- bill otherwise, rather than a provider's list price. See docs/billing.md.
ALTER TABLE billing_lines ADD COLUMN gateway_fee boolean NOT NULL DEFAULT false;

-- Write less on every request. Nothing reads who ran a filter or for which
-- model, only for which project, and one row per filter per request makes this
-- table as large as the request log.
ALTER TABLE filter_runs DROP COLUMN user_id, DROP COLUMN key_id, DROP COLUMN alias;
