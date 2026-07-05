package pgstore

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// TestIsUniqueViolationMatchesOnSQLSTATEAlone guards against a regression
// where isUniqueViolation required pgconn.PgError.ConstraintName to be
// non-empty. pgx does not always resolve a 23505 violation to a named
// constraint (ConstraintName can legitimately be ""), so requiring it would
// stop PutAccount, PutApiKey, and the credit-ledger inserts from recognizing
// a genuine duplicate as saas.ErrConflict.
func TestIsUniqueViolationMatchesOnSQLSTATEAlone(t *testing.T) {
	pgErr := &pgconn.PgError{Code: pgUniqueViolation, ConstraintName: ""}
	if !isUniqueViolation(pgErr) {
		t.Fatal("isUniqueViolation must recognize a 23505 PgError even with an empty ConstraintName")
	}
	// Wrapped, as errors surface in practice.
	if !isUniqueViolation(fmt.Errorf("insert: %w", pgErr)) {
		t.Fatal("isUniqueViolation must unwrap to find the PgError")
	}
}

// TestIsUniqueViolationRejectsOtherErrors asserts non-23505 errors, including
// nil and non-PgError errors, are not misclassified as unique violations.
func TestIsUniqueViolationRejectsOtherErrors(t *testing.T) {
	if isUniqueViolation(nil) {
		t.Fatal("nil must not be a unique violation")
	}
	if isUniqueViolation(errors.New("boom")) {
		t.Fatal("a plain error must not be a unique violation")
	}
	other := &pgconn.PgError{Code: "23503", ConstraintName: "some_fk"} // foreign_key_violation
	if isUniqueViolation(other) {
		t.Fatal("a foreign-key violation must not be classified as a unique violation")
	}
}

// TestIsPendingInviteConflictRequiresBothSQLSTATEAndConstraintName asserts the
// invitation-specific helper still requires the 0016 partial index name: a
// generic 23505 on the invitations table (duplicate id or token_hash) must
// NOT be classified as a pending-invite conflict, only as isUniqueViolation.
func TestIsPendingInviteConflictRequiresBothSQLSTATEAndConstraintName(t *testing.T) {
	pending := &pgconn.PgError{Code: pgUniqueViolation, ConstraintName: invitationsPendingUniqueIndex}
	if !isPendingInviteConflict(pending) {
		t.Fatal("a 23505 on the pending-unique index must be a pending-invite conflict")
	}
	if !isUniqueViolation(pending) {
		t.Fatal("a pending-invite conflict is still a unique violation")
	}

	genericDup := &pgconn.PgError{Code: pgUniqueViolation, ConstraintName: "invitations_pkey"}
	if isPendingInviteConflict(genericDup) {
		t.Fatal("a duplicate id must not be misclassified as a pending-invite conflict")
	}
	if !isUniqueViolation(genericDup) {
		t.Fatal("a duplicate id is still a genuine unique violation (saas.ErrConflict)")
	}

	emptyConstraint := &pgconn.PgError{Code: pgUniqueViolation, ConstraintName: ""}
	if isPendingInviteConflict(emptyConstraint) {
		t.Fatal("an empty constraint name must not match the pending-invite index")
	}
	if !isUniqueViolation(emptyConstraint) {
		t.Fatal("a 23505 with an empty constraint name is still a unique violation")
	}
}
