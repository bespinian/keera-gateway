-- What v0.5.0 changes in the schema.

-- When each person last signed in to the panel or the command line. Sessions
-- are deleted when they expire, so this is the only record of it.
ALTER TABLE users ADD COLUMN signed_in_at timestamptz;

-- A decision model is a classifier that decides for an instruction router or a
-- gate filter. See docs/routers.md.
ALTER TABLE models DROP CONSTRAINT models_kind_check,
    ADD CONSTRAINT models_kind_check
        CHECK (kind IN ('chat', 'completion', 'embedding', 'decision'));

-- One person can be in several organisations, with a role in each: one row in
-- users per organisation. The rows share the identity the person first signed
-- in with, so an identity is unique within an organisation, not across them.
DROP INDEX users_external_id_key;
CREATE UNIQUE INDEX users_org_external_id_key ON users (org_id, external_id)
    WHERE external_id IS NOT NULL;
CREATE INDEX users_external_id_idx ON users (external_id) WHERE external_id IS NOT NULL;

-- What a person's directory last said, kept once for the identity rather than
-- on each of its rows: a provider that rotates refresh tokens refuses the
-- second use of one, so two rows must never each spend a copy.
CREATE TABLE identities (
    -- users.external_id, as "<provider>:<subject>".
    external_id          text PRIMARY KEY,
    -- Sealed with KEERA_SECRET_KEY under the external id. Null when the
    -- provider issued none.
    refresh_token        bytea,
    directory_checked_at timestamptz
);

-- The refresh tokens on users were sealed under the row's id and cannot be
-- moved here. Ending the sign-ins that relied on one makes those people sign
-- in again, which stores a new one, rather than leaving a month-long token
-- that no directory check reaches.
DELETE FROM sessions WHERE user_id IN (SELECT id FROM users WHERE refresh_token IS NOT NULL);
DELETE FROM cli_tokens WHERE user_id IN (SELECT id FROM users WHERE refresh_token IS NOT NULL);
ALTER TABLE users DROP COLUMN refresh_token, DROP COLUMN directory_checked_at;
