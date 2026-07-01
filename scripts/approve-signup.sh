#!/bin/sh
# approve-signup.sh: approve a waitlisted signup by email address.
#
# Usage: approve-signup.sh <email> [note]
#
# Required environment variables:
#   MITOS_CONSOLE_APPROVE_SIGNUP_TOKEN - shared bearer secret for the endpoint
#   MITOS_CONSOLE_URL                  - base URL of the console service
#
# The bearer token is passed in the Authorization header and is never echoed
# to stdout or stderr. The email is printed to stdout on success.
set -e

usage() {
    printf 'Usage: %s <email> [note]\n' "$0" >&2
    printf '\n' >&2
    printf 'Environment variables required:\n' >&2
    printf '  MITOS_CONSOLE_APPROVE_SIGNUP_TOKEN  shared bearer secret\n' >&2
    printf '  MITOS_CONSOLE_URL                   base URL of the console (e.g. https://console.mitos.run)\n' >&2
    exit 1
}

if [ -z "$1" ]; then
    usage
fi

EMAIL="$1"
NOTE="${2:-}"

if [ -z "$MITOS_CONSOLE_APPROVE_SIGNUP_TOKEN" ]; then
    printf 'Error: MITOS_CONSOLE_APPROVE_SIGNUP_TOKEN is not set.\n' >&2
    exit 1
fi

if [ -z "$MITOS_CONSOLE_URL" ]; then
    printf 'Error: MITOS_CONSOLE_URL is not set.\n' >&2
    exit 1
fi

# Build the JSON body. Email addresses are ASCII with no JSON special characters.
# Notes should be plain text without quotes or backslashes; use jq for complex notes.
if [ -n "$NOTE" ]; then
    BODY=$(printf '{"email":"%s","note":"%s"}' "$EMAIL" "$NOTE")
else
    BODY=$(printf '{"email":"%s"}' "$EMAIL")
fi

curl -fsSL \
    -X POST \
    -H "Content-Type: application/json" \
    -H "Authorization: Bearer ${MITOS_CONSOLE_APPROVE_SIGNUP_TOKEN}" \
    -d "$BODY" \
    "${MITOS_CONSOLE_URL}/internal/approve-signup"
