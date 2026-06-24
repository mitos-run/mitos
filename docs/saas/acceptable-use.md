# Acceptable Use Policy (hosted offering)

This Acceptable Use Policy (AUP) governs the hosted Mitos sandbox cloud. It is
the human-readable counterpart to the technical abuse controls: the
per-organization quotas, the per-org and per-IP rate limits, the per-tier egress
policy, and the kill-switch. By using the hosted offering you agree to these
terms; we enforce them automatically and, where needed, manually.

This document covers the HOSTED service. Self-hosters run their own clusters and
set their own policy; see the self-hoster template at the end.

## Why this exists

The hosted cloud runs untrusted code submitted by anonymous-ish signups. Without
abuse controls a public sandbox cloud is a free crypto-mining rig and an
outbound-attack platform. These controls are the hard gate on public self-serve
access: they exist BEFORE we open self-serve so that one abusive tenant cannot
turn the platform into a weapon against the rest of the internet or against other
tenants.

## Prohibited use

You may not use the hosted sandboxes to:

- Mine cryptocurrency or run proof-of-work for any purpose.
- Send unsolicited bulk email (spam) or operate a mail relay. Outbound SMTP
  (ports 25, 465, 587) is blocked on every plan tier, including paid tiers.
- Launch denial-of-service attacks, port scans, brute-force attempts, or any
  outbound attack against any host you do not own or have explicit permission to
  test.
- Distribute malware, host phishing pages, or stage command-and-control
  infrastructure.
- Attempt to escape the sandbox isolation boundary, attack the host, the control
  plane, another tenant, or the cloud provider's metadata endpoint.
- Circumvent the quotas, rate limits, egress policy, or any other abuse control,
  including by creating many organizations to amortize a per-org limit.
- Store or process content that is illegal in the jurisdictions we operate in.

## Resource limits (quotas)

Every organization is on a plan tier (a prepaid ladder). Each tier sets hard
ceilings on:

- concurrent running sandboxes,
- aggregate vCPU, memory, and storage across all your running sandboxes,
- the maximum size of a single sandbox,
- the sandbox creation rate, and
- the API request rate (per organization AND per source IP).

A new, unverified signup lands on the most restricted tier with a
deny-by-default network posture: untrusted code reaches NOTHING outbound until
you explicitly allowlist a destination within your tier. Climbing the prepaid
ladder (verifying and paying) widens the envelope. This is intentional: the
blast radius of any single key is bounded until the organization has established
a payment relationship.

When you exceed a quota the API returns a typed error: `quota_exceeded` (an
organization plan ceiling), `rate_limited` (a request-rate or creation-rate
ceiling), or `forbidden` (the organization is suspended or unverified). Each
carries a remediation telling you which limit fired and how to proceed.

## Network egress policy

Your tier sets a DEFAULT network posture. The free tier is deny-by-default (no
egress without an explicit allowlist). Paid tiers may default to open egress, but
on EVERY tier the cloud-metadata endpoint is hard-blocked and the well-known
abuse ports (outbound SMTP) are dropped before any allowlist, so even an open
tier cannot send mail spam or steal cloud IAM credentials. Real packet
enforcement is the KVM datapath; the tier selects which policy applies.

## Enforcement: suspension and the kill-switch

We enforce this policy automatically and manually:

- Automated abuse signals (for example a sudden egress spike consistent with
  mining or scanning) can suspend an organization immediately. A suspended
  organization fails closed: its API keys are rejected, new sandboxes are
  refused, and its running sandboxes are frozen.
- An operator can suspend a single organization (manual review hook) or trigger a
  pool-wide or organization-wide emergency stop (the big red button) during an
  incident.
- An automated suspension is held for human review before it is lifted; we do not
  auto-unsuspend an organization an abuse signal flagged.

Suspension is fail-closed by design: we would rather wrongly pause a legitimate
tenant for a few minutes than let an abusive tenant keep attacking. If you
believe a suspension was in error, contact support for review.

## Changes

We may update this policy as the abuse surface moves. Material changes to the
abuse-control envelope are reflected in the threat model (docs/threat-model.md)
in the same change.

---

## Self-hoster template

If you run your own Mitos cluster you are the operator, and you set your own
acceptable-use policy. The hosted controls (quotas, rate limits, per-tier egress,
kill-switch) ship as a reusable layer (`internal/saas/quota`), but the DEFAULTS
above are the hosted service's, not yours. If you expose your cluster to
untrusted or multi-tenant users you SHOULD adopt an equivalent policy. A minimal
starting template:

> ## Acceptable Use (self-hosted Mitos at <your-org>)
>
> Sandboxes on this cluster may not be used for cryptocurrency mining,
> spam, denial-of-service, attacks against any host, malware distribution, or any
> attempt to escape the sandbox or attack the cluster. Resource quotas, rate
> limits, and a deny-by-default network posture are enforced; abuse results in
> immediate suspension. Contact <your-abuse-contact> to report abuse or appeal a
> suspension.

Wire the controls to your own tiers and limits via `quota.DefaultTiers()` (override
the table for your plans) and a `quota.SuspensionStore` plus `quota.KillSwitch`
for the suspension verb. Until you do, do NOT expose a self-hosted cluster to
untrusted public signups: with no enforcer the front door imposes no
organization-level limit beyond the request-body size bound.
