# Legal scaffolds (DRAFT, for counsel review)

The documents in this directory are SCAFFOLDS, not finished legal agreements.
They exist so the hosted offering has a structured starting point for the Terms
of Service, the Acceptable Use Policy, and the DMCA / copyright process. Every
placeholder that requires a legal or business decision is marked inline as
`TODO(legal)`.

Status: DRAFT. None of these documents is in force. Do not present any of them to
a customer, link them from a signup flow, or rely on them as a binding agreement
until counsel has reviewed and finalized them and the business has filled in every
`TODO(legal)` placeholder (entity name, governing jurisdiction, contact
addresses, notice periods, and similar).

Files:

- `terms-of-service.md`: the master agreement scaffold (account, plans, payment,
  liability, termination).
- `acceptable-use-policy.md`: the AUP scaffold. The abuse-enforcement code path
  (the quota enforcer and the kill-switch in `internal/saas/quota`) enforces the
  technical floor of this policy: a violation of the AUP is what the automated
  abuse signals and the operator emergency stop act on.
- `dmca.md`: the copyright / DMCA notice-and-takedown process scaffold.

These scaffolds intentionally avoid asserting facts that have not been decided
(the legal entity, the jurisdiction, the registered DMCA agent). Filling those in
is a `TODO(legal)` task, not something this repository fabricates.
