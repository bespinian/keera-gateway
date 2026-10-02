-- Passkeys, for accounts no directory vouches for.
--
-- This is its own migration rather than part of 0001, so a database that
-- already has the schema gets it too.
--
-- Such an account has external_id "keera:passkey:<user id>", so a first
-- single sign-on never adopts it, and a directory account never gets a
-- passkey: leaving the directory has to keep working as the way out.
CREATE TABLE passkeys (
    id            text PRIMARY KEY,
    user_id       text NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- The WebAuthn credential id the authenticator chose.
    credential_id bytea NOT NULL UNIQUE,
    -- SubjectPublicKeyInfo, DER, and the COSE algorithm it signs with. A
    -- public key is not a secret, so it is stored as it is.
    public_key    bytea NOT NULL,
    algorithm     int NOT NULL,
    -- The authenticator's signature counter. One that goes backwards means
    -- the key was copied. Synced passkeys always send zero.
    sign_count    bigint NOT NULL DEFAULT 0,
    -- What the person called it, such as "work laptop".
    name          text NOT NULL DEFAULT '',
    created_at    timestamptz NOT NULL DEFAULT now(),
    last_used_at  timestamptz
);
CREATE INDEX passkeys_user_id_idx ON passkeys (user_id);

-- A one-time link an administrator hands over, which lets its holder add a
-- passkey to one account. It is how the first passkey gets there, how a second
-- device does without the first, and how a person who lost theirs gets back in.
CREATE TABLE passkey_links (
    -- SHA-256 of the token in the link.
    id         bytea PRIMARY KEY,
    user_id    text NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL
);
CREATE INDEX passkey_links_user_id_idx ON passkey_links (user_id);
CREATE INDEX passkey_links_expires_at_idx ON passkey_links (expires_at);

-- A passkey registration in flight: the challenge the browser has to sign.
-- Sign-ins keep theirs in login_flows, next to the command-line hand-over.
CREATE TABLE passkey_challenges (
    id         text PRIMARY KEY,
    user_id    text NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    challenge  text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL
);
CREATE INDEX passkey_challenges_expires_at_idx ON passkey_challenges (expires_at);
