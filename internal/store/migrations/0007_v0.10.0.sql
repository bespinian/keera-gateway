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
