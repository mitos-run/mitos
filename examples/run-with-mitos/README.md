# Run with Mitos

One click, and in roughly fork latency you have your own private, always-on instance
of an app, isolated in a Firecracker microVM, with your keys never touching anyone
else's, that silently stays patched to upstream forever.

This directory holds the flagship `mitos.yaml` demos. Full design:
`docs/superpowers/specs/2026-06-26-run-with-mitos-flagship-proposal.md` (extends
issue #340; the public arbitrary-repo path tracks #341).

## The badge

```markdown
[![Run with Mitos](https://mitos.run/badge.svg)](https://mitos.run/run?src=github.com/openclaw/openclaw)
```

Click, and `mitos.run/run` reads the repo's `mitos.yaml`, shows one consent screen
(what version forks, what keys it receives, where it may egress), takes your secrets,
forks a warm golden, and redirects you to the live URL.

## Why fork, not deploy

You are not building an instance, you are forking a warm golden one that is already
booted and serving. The `ready` gate in the manifest means the golden is snapshotted
only after the app is serving, so a fork inherits a live, warm process. OpenClaw
cold-starts in roughly 16 seconds; a fork lands a ready instance in fork latency.

## What is in here

The same mechanism (golden snapshot, CoW fork, durable Workspace, auto-update,
expose ladder, per-fork secrets) across four app shapes, to show it is a primitive
and not a one-off:

- `openclaw/mitos.yaml`: the public flagship. A personal AI assistant, warm-forked.
- `paperclip/mitos.yaml`: a stateful, multi-service control plane with a database.
  Proves the mechanism carries the DB-backed, multi-process end.
- `deerflow/mitos.yaml`: a long-horizon multi-agent research harness
  (bytedance/deer-flow). The dogfood for the fork-native subagent SDK (#340 piece 2):
  its lead agent forks the warm golden to spawn each sub-agent instead of
  cold-starting. Built from source (no published image).
- `hermes-agent/mitos.yaml`: a self-improving personal agent
  (NousResearch/hermes-agent). Showcases fork-your-evolved-state and auto-update, and
  is a natural new fork-native backend alongside its existing Modal and Daytona ones.

## The four properties, on one mechanism

- Instant provision: fork a `ready` golden, not a cold boot.
- Cheap at scale: forks share the golden's pages copy-on-write; the marginal user is
  dirty pages, not a whole VM. Idle instances pause and stop costing RAM.
- Durable, versioned state: the mitos Workspace (content-addressed, S3-backed,
  revisioned) survives updates, rebases, and node loss, and can be forked with the
  runtime to branch production state safely.
- Auto-current: a published upstream release re-snapshots the golden once; running
  instances rebase (new runtime fork plus the same Workspace) with one-click rollback
  and no downtime.

## Security

Secrets are injected per-fork and never baked into the golden (so the golden is
publicly shareable without leaking anyone's keys). Egress is default-deny to the
declared allowlist. Each instance is a Firecracker microVM. Auto-update keeps deps
patched without user action. The public path for arbitrary third-party repos is gated
on the abuse and legal prerequisites in #341.
