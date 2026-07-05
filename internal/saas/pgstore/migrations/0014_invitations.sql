-- Org invitations (email invites + member management, workstream 3). The raw
-- invite token is NEVER stored, only its sha256 hash (token_hash), mirroring
-- pending_signups.token_hash. Expiry is enforced lazily at read time by the
-- application (saas.Invitation.EffectiveState); state stays 'pending' in
-- storage until an explicit transition (accept) writes a new value, so this
-- table never needs a background expiry sweep.
CREATE TABLE invitations (
    id          TEXT        PRIMARY KEY,
    org_id      TEXT        NOT NULL,
    email       TEXT        NOT NULL,
    role        TEXT        NOT NULL,
    token_hash  TEXT        NOT NULL UNIQUE,
    state       TEXT        NOT NULL DEFAULT 'pending',
    inviter_id  TEXT        NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL,
    expires_at  TIMESTAMPTZ NOT NULL
);
CREATE INDEX invitations_org_id_idx ON invitations (org_id);
