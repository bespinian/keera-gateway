-- What v0.10.0 changes in the schema.

-- A key an administrator leaves for its holder to activate has no secret
-- until they do, so it has no hash either. NULL never matches a presented
-- key, and UNIQUE allows any number of them.
ALTER TABLE api_keys ALTER COLUMN key_hash DROP NOT NULL;
