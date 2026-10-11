-- What v0.5.0 changes in the schema.

-- When each person last signed in to the panel or the command line. Sessions
-- are deleted when they expire, so this is the only record of it.
ALTER TABLE users ADD COLUMN signed_in_at timestamptz;

-- A decision model is a classifier that decides for an instruction router or a
-- gate filter. See docs/routers.md.
ALTER TABLE models DROP CONSTRAINT models_kind_check,
    ADD CONSTRAINT models_kind_check
        CHECK (kind IN ('chat', 'completion', 'embedding', 'decision'));
