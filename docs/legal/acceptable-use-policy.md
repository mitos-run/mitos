# Acceptable Use Policy (DRAFT scaffold, for counsel review)

> STATUS: DRAFT scaffold. This is NOT a finished or binding policy. It is a
> structured outline for counsel to complete. Every `TODO(legal)` marks a
> decision that legal or the business must make. Do not put this in force or show
> it to customers until counsel has reviewed and finalized it.

This Acceptable Use Policy ("AUP") governs use of the hosted sandbox Service. It
is incorporated into the Terms of Service (`terms-of-service.md`). A violation of
this AUP is grounds for the enforcement actions described in Section "Enforcement"
below, up to and including suspension of the Organization.

The technical floor of this AUP is enforced programmatically. The quota enforcer
and the kill switch (`internal/saas/quota`) reject over-quota and over-rate
requests and suspend Organizations that trip an automated abuse signal or an
operator emergency stop. Those controls are how the prohibitions below are made
effective at runtime; this document is the human-readable policy they enforce.

## Prohibited uses

The Customer must not use the Service to:

- send unsolicited bulk or commercial messages (spam), including by relaying
  outbound mail. `TODO(legal): confirm the mail-abuse language; the platform
  blocks well-known outbound mail ports fleet-wide as a technical control.`
- mine cryptocurrency or perform comparable resource-abuse workloads where the
  primary purpose is to consume compute the Customer has not paid for.
  `TODO(legal): confirm whether crypto-mining is fully prohibited or allowed only
  on specific plans.`
- conduct or facilitate attacks on third parties (denial-of-service, port
  scanning, credential stuffing, intrusion, or botnet command-and-control).
- distribute malware, ransomware, or other malicious code intended to harm third
  parties.
- attempt to break out of the sandbox isolation boundary, access other
  Organizations' data or sandboxes, or otherwise defeat the Service's security
  controls.
- evade or attempt to evade quotas, rate limits, or the abuse controls (for
  example by rapidly creating and destroying sandboxes to amortize a concurrency
  cap, or by rotating source addresses to dodge a per-source limit).
- store or process content the Customer has no right to use, or that is unlawful
  in the governing jurisdiction. `TODO(legal): list the unlawful-content
  categories the business chooses to call out, drafted to the jurisdiction.`
- `TODO(legal): any additional prohibitions the business requires (for example
  high-risk regulated workloads, sanctioned-party use, or specific industry
  restrictions).`

## Resource and fair-use limits

Use of the Service is subject to the plan's quotas and rate limits. Circumventing
these limits is a violation of this AUP. The current technical limits per plan are
documented with the plan; the Provider may adjust them on `TODO(legal): notice
period` notice.

## Reporting abuse

To report abuse of the Service by a Customer, contact `TODO(legal): abuse contact
address`. The Provider investigates reports and may act under "Enforcement".

## Enforcement

The Provider may, to protect the Service and third parties and proportionate to
the violation:

- rate-limit, quota-limit, or throttle the Organization;
- suspend the Organization automatically when an abuse signal fires (held for
  human review before reinstatement);
- suspend the Organization manually after review; and
- in an emergency, suspend affected Organizations at once.

`TODO(legal): the notice the Customer receives, the reinstatement / appeal
process, and the standard for an automated suspension versus a manual one.`

## Changes

`TODO(legal): how the Provider amends this AUP and how amendments are noticed and
take effect.`
