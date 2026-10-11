-- What v0.5.0 changes in the schema.

-- When each person last signed in to the panel or the command line. Sessions
-- are deleted when they expire, so this is the only record of it.
ALTER TABLE users ADD COLUMN signed_in_at timestamptz;
