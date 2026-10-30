-- What v0.12.0 changes in the schema.

-- When the directory first failed to answer, cleared once it answers again. A
-- sign-in lasts 30 days, so a directory that stays broken must not keep people
-- signed in unchecked that long. See "Asking the directory again" in
-- docs/sso.md.
ALTER TABLE identities ADD COLUMN directory_failing_since timestamptz;
