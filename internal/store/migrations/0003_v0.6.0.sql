-- What v0.6.0 changes in the schema.

-- Every price, bill and credit is in CHF, so a bill line no longer says which
-- currency it is in.
ALTER TABLE billing_lines DROP COLUMN currency;
