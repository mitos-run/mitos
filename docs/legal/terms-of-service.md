# Terms of Service (DRAFT scaffold, for counsel review)

> STATUS: DRAFT scaffold. This is NOT a finished or binding agreement. It is a
> structured outline for counsel to complete. Every `TODO(legal)` marks a
> decision that legal or the business must make. Do not put this in force or show
> it to customers until counsel has reviewed and finalized it.

## 0. Parties and definitions

- "Provider" means `TODO(legal): legal entity name`, a `TODO(legal): entity type
  and jurisdiction of incorporation`.
- "Service" means the hosted sandbox platform offered at `TODO(legal): service
  domain`.
- "Customer", "you", "Organization" mean the entity or individual that creates an
  account and agrees to these Terms.
- "Sandbox" means an isolated compute environment the Service provisions on the
  Customer's request.

## 1. Agreement to terms

By creating an account or using the Service, the Customer agrees to these Terms
and to the Acceptable Use Policy (`acceptable-use-policy.md`), which is
incorporated by reference. `TODO(legal): describe acceptance mechanics, e.g.
click-through at signup, and how updates to the Terms are noticed and take
effect.`

## 2. Accounts and organizations

- The Customer is responsible for the activity of every credential issued under
  its Organization, including API keys.
- The Customer must keep credentials confidential. `TODO(legal): set out the
  Customer's obligation to report suspected credential compromise and the
  Provider's right to revoke a compromised credential.`

## 3. Plans, quotas, and fair use

- The Service offers tiered plans. Each plan carries quotas and rate limits
  (concurrency, aggregate resource footprint, per-sandbox size, request and
  creation rates). The Provider enforces these limits programmatically.
- The Provider may change plan limits on `TODO(legal): notice period` notice.
- `TODO(legal): describe the consequence of exceeding a quota, e.g. requests are
  rejected with a documented error, not silently dropped, and how a Customer
  raises a limit (upgrade or contact).`

## 4. Payment

`TODO(legal): payment terms, billing cycle, currency, taxes, the hard spend cap
and dunning behavior, and what happens on non-payment, including suspension.`

The Provider operates a hard spend cap and a payment-retry (dunning) process. When
either is exhausted, the Provider may suspend the Organization until the matter is
resolved. `TODO(legal): align this paragraph with the finalized billing terms.`

## 5. Acceptable use and enforcement

The Customer must comply with the Acceptable Use Policy. The Provider may, to
protect the Service and third parties:

- rate-limit, quota-limit, or throttle the Organization;
- suspend the Organization (the "kill switch"), automatically in response to an
  abuse signal or manually on review; and
- in an emergency affecting the platform, suspend affected Organizations at once.

`TODO(legal): notice and reinstatement process for suspensions, distinguishing an
automated abuse suspension (held for human review) from a billing suspension.`

## 6. Customer content and responsibility

`TODO(legal): ownership of Customer content, the license the Customer grants the
Provider to operate the Service, and the Customer's representations that it has
the rights to the content it runs.`

## 7. Intellectual property and DMCA

The Provider responds to copyright complaints under the process described in
`dmca.md`. `TODO(legal): incorporate the finalized DMCA process and the
designated-agent details.`

## 8. Warranties and disclaimers

`TODO(legal): warranty disclaimer (the Service is provided "as is" to the extent
permitted by law), and any limited warranties the business chooses to offer.`

## 9. Limitation of liability

`TODO(legal): liability cap, exclusion of indirect and consequential damages, and
any carve-outs, drafted to the governing jurisdiction.`

## 10. Term and termination

`TODO(legal): how either party terminates, effect of termination on running
sandboxes and stored data, and the data-export / deletion window.`

## 11. Governing law and disputes

`TODO(legal): governing law, venue, and dispute-resolution mechanism (courts or
arbitration).`

## 12. Changes to these terms

`TODO(legal): how the Provider amends these Terms and how amendments are noticed
and take effect.`

## 13. Contact

`TODO(legal): legal / notices contact address.`
