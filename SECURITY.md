# Security Policy

## Reporting a Vulnerability

Please do not file public issues for vulnerabilities.

- **Preferred**: GitHub private vulnerability reporting. Go to the Security tab of this repository and click "Report a vulnerability".
- **Fallback**: email jannes@paperclip.inc.

We will acknowledge your report within 72 hours and keep you informed of progress toward a fix and disclosure.

## Supported Versions

This project is pre-1.0. Only the latest release receives security fixes.

## Scope

This project executes untrusted code in microVMs. The following are explicitly in scope:

- Guest-to-host escapes (Firecracker, vsock, forkd).
- Cross-sandbox leaks of any kind, including state leaking through snapshot forks.
- Snapshot integrity (tampering, substitution, restore of unverified snapshots).
- Secret handling (secrets appearing in logs, error messages, condition messages, or host paths).

## Current Status

This project has not yet had an external security review. The known threat surface and its per-row mitigation status are documented in [docs/threat-model.md](docs/threat-model.md). Read it before deploying to anything that matters.

## AI-Assisted Development Policy

Substantial portions of this codebase are AI-assisted. Security-sensitive paths receive named-human review before merge: `internal/fork`, `internal/firecracker`, `internal/daemon`, `guest/agent`, and future token/attenuation code. This policy is tracked in issue #35.
