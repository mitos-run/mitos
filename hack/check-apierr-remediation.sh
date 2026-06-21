#!/usr/bin/env bash
set -euo pipefail

# Static guarantee for issue #28: every LLM-legible error path carries a
# non-empty remediation, even an unexercised one.
#
# The runtime CI test (internal/daemon/error_envelope_test.go) only proves this
# for paths a test actually hits. This static check parses the Go source with
# go/ast and fails the build if:
#   1. any apierr.Catalogue entry literal has an empty or missing Remediation, or
#   2. any apierr.Error composite literal anywhere in the tree (a call site that
#      builds an error directly instead of going through the catalogue) has an
#      empty or missing Remediation.
#
# Wired into the go-lint CI job. Run locally with:
#   ./hack/check-apierr-remediation.sh
#
# Exit non-zero on the first violation, listing file:line and the offending
# code so the fix is obvious.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

go run ./hack/apierrlint "$ROOT"
