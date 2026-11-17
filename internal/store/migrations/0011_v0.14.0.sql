-- What v0.14.0 changes in the schema.

-- A billing line can come from another service, so far only Keera Agents
-- billing its machines. Such a line bills an item, such as CPU time, not a
-- model, so it says what it bills and how much of it. See docs/billing.md.
ALTER TABLE billing_lines
    -- Empty for the gateway's own lines; 'agents' for Keera Agents'.
    ADD COLUMN service         text NOT NULL DEFAULT '',
    -- What was billed, such as 'CPU', and in what, such as 'CPU-hours'.
    ADD COLUMN item            text NOT NULL DEFAULT '',
    ADD COLUMN unit            text NOT NULL DEFAULT '',
    -- How much, in millionths of the unit.
    ADD COLUMN quantity_micros bigint NOT NULL DEFAULT 0,
    -- The sender's id for the line, unique per service. A service sends a
    -- line again when it did not hear back, so it must be recorded, and
    -- charged, only once.
    ADD COLUMN external_id     text;
CREATE UNIQUE INDEX billing_lines_external_idx ON billing_lines (service, external_id)
    WHERE external_id IS NOT NULL;
