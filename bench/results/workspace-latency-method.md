# Workspace hydrate/dehydrate and fork latency: method

This file records the METHOD for the EPIC W4 (issue #21) workspace latency
benchmarks. It deliberately contains NO measured numbers. Per CLAUDE.md
operating principle 1 (no unverified claims), a workspace latency number is
published only when it is reproducible from the script that produced it; when a
reference run is captured, its distribution and hardware are recorded in a dated
result file next to this one (mirroring `2026-06-13-bare-metal-husk.md`).

## Scripts

- `bench/workspace-hydrate-latency.sh <kubeconfig> <pool> <workspace> [namespace] [iterations]`
  measures, per cycle, the END-TO-END wall clock of:
  - `dehydrate`: commit-on-terminate of a workspace-bound sandbox until the
    workspace head advances to the new committed revision;
  - `hydrate`: a fresh bound sandbox from create until Ready with the committed
    `/workspace` state present (verified by reading back a marker file).
  It prints min / p50 / p95 / max for each phase and the observed STORE_MODE
  (node CAS or S3, encryption on or off), because at-rest encryption and S3
  egress change the wall clock and the result must say which backend produced it.

- `bench/workspace-fork-latency.sh <kubeconfig> <src-workspace> [namespace] [iterations]`
  measures the wall clock of `mitos ws fork` AND asserts the fork is O(0) new
  bytes: the forked revision's `contentManifest` must equal the parent revision's
  `contentManifest` (a content-addressed branch, ADR-0002 reason 1). If they
  differ the script FAILS, because that would mean the fork wrote new bytes and
  content-addressed dedup is broken. fork latency is therefore a control-plane
  number (revision-object create plus reconcile to Committed), not a data-path
  number; that is the design and what the script proves.

## What is already proven in unit tests (not cluster-gated)

The byte-identical round trip and chunk-level dedup that these latency scripts
exercise on a live cluster are proven offline in `internal/workspace`:

- node CAS and S3 produce the SAME revision digest for a tree
  (`TestS3DigestMatchesNodeCASDigest`);
- per-workspace encryption produces the SAME digest as plaintext and writes no
  new chunks for an identical tree (`TestEncryptedDigestMatchesPlaintextDigest`,
  `TestEncryptedDehydrateDedups`);
- S3 dedups by chunk-digest object key (`TestS3DedupsByChunkDigest`);
- the encrypted round trip is byte-identical and ciphertext at rest
  (`TestEncryptedDehydrateHydrateRoundTrip`, `TestEncryptedChunksAreCiphertextAtRest`,
  `TestS3EncryptedRoundTrip`).

## Capturing a reference run

Run each script on the reference KVM node against a warm pool and a Workspace
with a committed head, with a higher iteration count, and record the printed
distribution, the STORE_MODE, and the host (CPU, kernel, Firecracker version,
store backend, encryption setting) in a new dated result file. Do not publish a
number this harness did not produce.
