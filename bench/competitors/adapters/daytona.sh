#!/usr/bin/env bash
#
# daytona.sh -- adapter for self-hosted Daytona OSS (github.com/daytonaio/daytona).
#
# Status on the 2026-06-22 matched-hardware run (box2, Hetzner): the Daytona OSS
# stack was deployed and came up fully (14/14 docker compose services healthy),
# but the create -> first-exec measurement is BLOCKED at headless authentication.
# No number was produced and none is fabricated. See
# bench/competitors/results/2026-06-22-matched-hardware.md for the full record.
#
# THE BLOCKER (verbatim, reproducible):
#   - The sandbox API requires a user-scoped bearer token or API key:
#       POST /api/sandbox  (no auth) -> 401 {"message":"Invalid credentials"}
#       POST /api/api-keys (no auth) -> 401 {"message":"Invalid credentials"}
#   - Minting that API key requires completing the Dex OIDC login, and the
#     default Dex ships only browser flows:
#       grant_types_supported = ["authorization_code","refresh_token",
#                                "device_code","token-exchange"]
#     There is NO resource-owner password grant, so there is no documented
#     headless username/password -> token path. The authorization-code and
#     device-code flows both terminate in a Dex web login form
#     (dev@daytona.io / password) that a curl-only harness cannot drive
#     cleanly (the local-connector POST returns 400 without the browser session
#     state). A real measurement needs a human to log in once via the dashboard
#     (http://localhost:3000, dev@daytona.io / password) and mint an API key.
#
# So this adapter is wired with the REAL Daytona create + exec calls, but gated
# on a DAYTONA_API_KEY the reproducer supplies after that one-time login. With
# the key present it runs end to end; without it, it exits non-zero (never a
# fabricated number), exactly the run-comparison.sh contract.
#
# Deploy (what was done on box2, reproducible):
#   apt-get install -y git docker  # docker via https://get.docker.com
#   git clone --depth 1 https://github.com/daytonaio/daytona.git
#   cd daytona && docker compose -f docker/docker-compose.yaml up -d
#   # then: open http://localhost:3000, log in (dev@daytona.io / password),
#   #       mint an API key, export it as DAYTONA_API_KEY, and create a snapshot
#   #       (the create call needs a ready snapshot image in the internal
#   #       registry; the default is daytonaio/sandbox:<version>).
#
# "Warm" for Daytona: the compose stack up, the runner healthy, and the snapshot
# image pulled/ready, so the measured number is Daytona's warm create path, not
# a cold image pull. Document the snapshot id and runner state with the result.
#
# Required environment:
#   DAYTONA_API_KEY    a user API key minted via the dashboard after OIDC login.
# Optional:
#   DAYTONA_BASE       API base (default: http://localhost:3000/api).
#   DAYTONA_SNAPSHOT   snapshot/image id to create from (default: daytonaio/sandbox:0.4.3).
#
# Endpoints used (from the running OSS swagger at /api-json on box2):
#   POST   /api/sandbox                                   create a sandbox
#   POST   /api/toolbox/{id}/toolbox/process/execute      exec a command
#   DELETE /api/sandbox/{id}                              tear it down
#
# Sourced by run-comparison.sh; defines warm() and create_exec() only.

: "${DAYTONA_BASE:=http://localhost:3000/api}"
: "${DAYTONA_SNAPSHOT:=daytonaio/sandbox:0.4.3}"

warm() {
  command -v curl >/dev/null 2>&1 || { echo "daytona: curl required" >&2; return 1; }
  command -v jq   >/dev/null 2>&1 || { echo "daytona: jq required" >&2; return 1; }

  if [ -z "${DAYTONA_API_KEY:-}" ]; then
    echo "daytona: BLOCKED -- no DAYTONA_API_KEY. The OSS stack authenticates the" >&2
    echo "  sandbox API via Dex OIDC (browser authorization-code / device-code only;" >&2
    echo "  no password grant), so a key must be minted once via the dashboard" >&2
    echo "  (http://localhost:3000, dev@daytona.io / password) and exported as" >&2
    echo "  DAYTONA_API_KEY. Until then no Daytona number is OUR measurement." >&2
    return 1
  fi

  # Confirm the API is reachable and the key is accepted before measuring.
  local code
  code="$(curl -sS -o /dev/null -w '%{http_code}' \
    -H "Authorization: Bearer $DAYTONA_API_KEY" "$DAYTONA_BASE/api-keys/current" 2>/dev/null)" || {
    echo "daytona: API unreachable at $DAYTONA_BASE" >&2; return 1; }
  if [ "$code" = "401" ] || [ "$code" = "403" ]; then
    echo "daytona: DAYTONA_API_KEY rejected (HTTP $code)" >&2; return 1
  fi
  echo "daytona warmed: API reachable, key accepted, snapshot=$DAYTONA_SNAPSHOT" >&2
}

create_exec() {
  local t0 t1 created sid exec_out exit_code
  t0=$(date +%s%N)

  # Create one sandbox from the warmed snapshot.
  created="$(curl -sS -X POST "$DAYTONA_BASE/sandbox" \
    -H "Authorization: Bearer $DAYTONA_API_KEY" \
    -H 'Content-Type: application/json' \
    -d "{\"snapshot\":\"$DAYTONA_SNAPSHOT\"}" 2>/dev/null)" || return 1
  sid="$(printf '%s' "$created" | jq -r '.id // empty' 2>/dev/null)"
  if [ -z "$sid" ]; then
    echo "daytona: create returned no sandbox id: $created" >&2
    return 1
  fi

  # Daytona reports started sandboxes; if create returns before the toolbox is
  # reachable, the first exec is the readiness signal we are timing anyway, so
  # retry the exec briefly until the toolbox answers, then require exit 0.
  exit_code=""
  for _ in $(seq 1 200); do
    exec_out="$(curl -sS -X POST "$DAYTONA_BASE/toolbox/$sid/toolbox/process/execute" \
      -H "Authorization: Bearer $DAYTONA_API_KEY" \
      -H 'Content-Type: application/json' \
      -d '{"command":"true"}' 2>/dev/null)" || true
    exit_code="$(printf '%s' "$exec_out" | jq -r '.exitCode // .exit_code // empty' 2>/dev/null)"
    [ -n "$exit_code" ] && break
    sleep 0.1
  done
  if [ "$exit_code" != "0" ]; then
    echo "daytona: exec did not return exit 0 for $sid: $exec_out" >&2
    curl -sS -X DELETE "$DAYTONA_BASE/sandbox/$sid" -H "Authorization: Bearer $DAYTONA_API_KEY" >/dev/null 2>&1 || true
    return 1
  fi

  t1=$(date +%s%N)

  # Tear down off the measured window.
  curl -sS -X DELETE "$DAYTONA_BASE/sandbox/$sid" -H "Authorization: Bearer $DAYTONA_API_KEY" >/dev/null 2>&1 || true

  awk -v a="$t0" -v b="$t1" 'BEGIN { printf "%d\n", (b - a) / 1000000 }'
}
