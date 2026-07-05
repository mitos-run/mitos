-- Carries the (hashed) pending org-invite token from signup through to
-- verify so a fresh signup whose email was already invited to an org can
-- auto-join that org on verification (see PendingSignup.InviteTokenHash and
-- Service.autoJoinPendingInvite). Only the hash is stored; the raw invite
-- token is never persisted.
ALTER TABLE pending_signups ADD COLUMN invite_token_hash TEXT NOT NULL DEFAULT '';
