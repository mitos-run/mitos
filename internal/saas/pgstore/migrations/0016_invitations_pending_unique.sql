-- Guards against a race between two concurrent CreateInvite calls for the
-- same (org, email): the application-level precheck in
-- InvitationService.CreateInvite (ListInvitations then check) is a
-- non-atomic check-then-act, so this partial unique index is the real
-- backstop. It applies only to state = 'pending' rows, so an org can freely
-- re-invite an address once its earlier invitation has been accepted,
-- revoked, or (lazily) expired-and-superseded; lower(email) matches the
-- application's own case-insensitive dedup (invites.go stores email already
-- lowercased, but this makes the invariant explicit at the schema level
-- too). PgStore.CreateInvitation maps a violation of this specific index to
-- saas.ErrInvitePending (by constraint name), distinct from the generic
-- saas.ErrConflict a duplicate id or token_hash violation still returns.
CREATE UNIQUE INDEX invitations_org_email_pending_idx
    ON invitations (org_id, lower(email))
    WHERE state = 'pending';
