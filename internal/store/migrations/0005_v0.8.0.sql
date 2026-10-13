-- What v0.8.0 changes in the schema.

-- A card payment carries a transaction fee on top, which is not credited.
-- Payments made before this had none.
ALTER TABLE payments ADD COLUMN fee_micros bigint NOT NULL DEFAULT 0;
