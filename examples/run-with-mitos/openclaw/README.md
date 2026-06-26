# OpenClaw, warm-forked on Mitos

[![Run with Mitos](https://mitos.run/badge.svg)](https://mitos.run/run?src=github.com/openclaw/openclaw)

Your own private, always-on OpenClaw assistant in roughly fork latency.

## What happens when you click

1. mitos reads this `mitos.yaml`.
2. You see one consent screen: which OpenClaw version forks, that it receives the
   keys you provide, and that it may egress only to anthropic.com plus your channels.
3. You paste your Claude key (and channel logins, optional). mitos can mint the
   gateway token for you.
4. mitos forks the warm golden OpenClaw (already booted and serving) and hands you
   `you.openclaw.mitos.run`.

No cold start, no `docker compose up`, no exposed gateway, no key on a shared host.

## Why this beats self-hosting

- Instant: fork a `ready` golden, not a 16-second cold boot.
- Private: a Firecracker microVM per instance; your keys are injected per-fork and
  never live in the shared golden.
- Locked down: the gateway sits behind the expose auth ladder; egress is default-deny
  to the endpoints OpenClaw declares.
- Always current: when OpenClaw releases, mitos re-snapshots the golden once and
  offers you a one-click rebase (new runtime, same `/home/node/.openclaw` Workspace),
  with one-click rollback and no downtime. The "self-hosters never update" problem,
  solved.

## State

Your config, history, and channel state live in a durable, versioned mitos Workspace
at `/home/node/.openclaw`. It survives updates, rebases, and node loss, and you can
fork it with the runtime to try a risky change against a copy.
