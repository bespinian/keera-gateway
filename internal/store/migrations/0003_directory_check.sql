-- Asking the directory again after sign-in.
--
-- A role is read from the identity provider at sign-in, and a session or
-- `keera login` token outlives that moment. So the gateway keeps the refresh
-- token the sign-in returned and spends it every few minutes: a person removed
-- from the directory loses access then, not when their credential expires.

-- Sealed with KEERA_SECRET_KEY, like a model's credential: it signs the
-- person in to the directory, so a database dump must not hold a working one.
-- Null when the provider issued none.
ALTER TABLE users ADD COLUMN refresh_token bytea;
-- When the directory last vouched for this person: their sign-in, or the
-- last refresh.
ALTER TABLE users ADD COLUMN directory_checked_at timestamptz;
