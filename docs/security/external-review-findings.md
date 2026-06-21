# External security review: findings tracker (G4, #194)

This is the live tracker for the external security review that gates the 1.0 /
production-tenant claim (ROADMAP G4, issue #194). The review package and scope
are in [external-review-scope.md](external-review-scope.md). This page records
every finding the external reviewer raises and drives each to closure.

## Status

- Engagement: NOT STARTED (a reviewer has not yet been engaged). This is a human
  step; it must be scheduled AFTER G1 (#3 fork-correctness) and G3 (multi-tenant
  isolation: #172 done, #192 done, #193 done) are green, so the reviewed surface
  is the final multi-tenant one.
- README claim: the README must keep stating "no external security review has
  been performed" until this engagement completes and all blocking findings are
  resolved. Do not remove that line before then.

## Gate rule

Per the operating principles, security findings block the 1.0 cut. A finding at
Critical or High severity is a hard blocker; Medium/Low are tracked and may ship
with a documented mitigation and owner. No finding is silently closed: each row
links a resolving PR or an accepted-risk ADR.

## Findings

None recorded yet. When the engagement begins, append one row per finding.

| ID | Severity | Surface | Summary | Status | Resolution (PR / ADR) |
| --- | --- | --- | --- | --- | --- |
| _none yet_ | | | | | |

Severity: Critical / High / Medium / Low / Informational.
Status: Open / In progress / Resolved / Accepted-risk (ADR).

## Closure checklist (before flipping the README claim)

- [ ] Reviewer engaged and scope (external-review-scope.md) agreed.
- [ ] Every Critical/High finding Resolved (linked PR) with a regression test
      where the surface is code-testable.
- [ ] Every Medium/Low finding Resolved or Accepted-risk with an ADR in
      `docs/adr/` and a named owner.
- [ ] `docs/threat-model.md` updated with any surface delta the review surfaced.
- [ ] README "no external security review" line removed in the same change that
      records the completed review, not before.
