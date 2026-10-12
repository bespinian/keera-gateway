-- What v0.7.0 changes in the schema.

-- Organisations are billed at list price and the gateway no longer tracks
-- what the provider charges, so a bill line no longer records it.
ALTER TABLE billing_lines DROP COLUMN provider_micros;
