# Contributing

Thanks for your interest in contributing.

## Build and test

See the Commands section of [CLAUDE.md](CLAUDE.md) for the full list. The short version:

```bash
make build
make test-unit
make test-controller   # needs setup-envtest
make test-python
```

Lint with both `golangci-lint run --timeout=5m` and `GOOS=linux golangci-lint run --timeout=5m`; the guest agent is linux-only.

## Commits

- Use conventional commits: feat, fix, docs, ci, chore, refactor, test.
- DCO sign-off is required on every commit: `git commit -s`. By signing off you certify the [Developer Certificate of Origin](https://developercertificate.org/). The final licensing ADR is tracked in issue #34.

## Pull requests

- Tests for every behavior change, in the same commit.
- Docs updated in the same PR.
- If the security surface moved, include the threat-model delta (docs/threat-model.md) in the same PR.
- All six CI checks must be green: go-test, go-lint, python-test, docker-build, kind-e2e, firecracker-test.

## Where to start

- Issues labeled "good first issue".
- ROADMAP.md is the priority order; pick something near the top.

## Style

No em or en dashes anywhere; see the Coding Conventions section of [CLAUDE.md](CLAUDE.md).
