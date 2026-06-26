# Run with Mitos: OpenClaw and Paperclip as flagships, and the auto-update no-brainer

Date: 2026-06-26
Status: proposal (extends issue #340, gated public path tracks #341)

## Context and provenance

This proposal is grounded in a real first deployment of mitos plus OpenClaw on a
single-node KVM box (box2). During that work a microVM sandbox was proven end to
end (snapshot-fork, networked guest, agent, exec), and OpenClaw was booted inside
a mitos microVM to its `ready` state (gateway listening, 7 plugins). Every platform
bug hit along the way now has a merged or open fix (console login #403/#431, husk
tap teardown #428/#429, jailer #425/#433, DAC_OVERRIDE #426/#435, ociroot debian
build #415/#436, husk-tls single-namespace #414/#437, console replicas #427/#438).
The concrete details below (the `ready` gate, secret non-inheritance, the egress
allowlist, the cold-start cost that makes fork shine, OpenClaw's refuse-to-bind
guard) come from that run.

This document extends issue #340 (Run with Mitos: button + `mitos.yaml` + fork-native
subagent SDK). It adds: OpenClaw as the public flagship, auto-update built on the
snapshot/fork model, the OpenClaw-specific security story, the state-persistence and
cost model, and proof that the very same mechanism carries Paperclip (a stateful,
multi-service control plane). The public arbitrary-repo path stays gated on #341.

## 0. The one sentence that makes it a no-brainer

Click a badge, and in roughly fork latency you have your own private, always-on
instance at `you.openclaw.mitos.run`, isolated in a Firecracker microVM, with your
keys never touching anyone else's, that silently stays patched to upstream forever.

Everything here serves that sentence. The reason it is a no-brainer and not just
another deploy button is fork: you are not building an instance, you are forking a
warm golden one that is already booted and serving. OpenClaw cold-starts in roughly
16s (node plus 7 plugins plus provider pre-warm, measured in the VM). A fork lands a
ready instance in fork latency. No deploy button does that.

## 1. The `mitos.yaml` manifest (schema v1)

A manifest in any repo makes it one-click warm-forkable on mitos. Convention over
destination, like Deploy-to-Heroku, Open-in-Gitpod, Run-on-Binder, pointed at
stateful apps and agents. The OpenClaw manifest, encoding every lesson from running
it, lives at `examples/run-with-mitos/openclaw/mitos.yaml`; the Paperclip one at
`examples/run-with-mitos/paperclip/mitos.yaml`. The load-bearing fields:

- `source.image` plus `source.track`: the golden is built from the image; `track`
  watches upstream releases and re-snapshots on a new tag (section 4).
- `run.command` plus `run.ready`: mitos snapshots the golden only AFTER `ready` is
  true (an HTTP health gate), so every fork boots already-serving. This `ready`
  gate is the trick that makes the click feel instant.
- `preview.port` plus `preview.auth`: the interactable port becomes the live URL,
  always behind the expose auth ladder, never raw-public.
- `secrets`: prompted at click, injected per-fork, never baked into the golden
  snapshot (fork-correctness secret non-inheritance, section 5).
- `egress.allow`: default-deny allowlist; the app reaches only what it declares.
- `workspace`: the durable, versioned path (section 8).
- `resources`: pool, size, lifetime.

Zero-config gradient (the load-bearing point in #340): for many repos mitos infers
`run.command` (image CMD), `preview.port` (EXPOSE), and declared `${VAR}` secrets,
so most repos need little or no manifest. If most repos needed hand-written config,
the "it just works" promise would break.

## 2. The badge and the click flow

```markdown
[![Run with Mitos](https://mitos.run/badge.svg)](https://mitos.run/run?src=github.com/openclaw/openclaw)
```

Click, and `mitos.run/run` fetches the repo's `mitos.yaml`, renders a single consent
screen ("this forks OpenClaw vX into your microVM; it will receive the keys you
provide; it can reach anthropic.com and telegram.org; your keys go only to your
VM"), takes the secrets, forks the warm golden, and redirects to the live URL.
Sub-second to interactable. The consent screen is the #341 secret-consent and egress
disclosure made concrete: what gets your secret, where it can egress, what version,
on one screen.

## 3. Why fork is the moat, not just speed

- Instant: fork a `ready` golden, not a cold boot. The `ready` gate means the
  snapshot is taken after the app is serving, so the fork inherits a live warm
  process.
- Cheap at scale: forks of one golden share its pages copy-on-write (section 9).
- Fork-native subagents (#340 piece 2): a multi-agent harness forks the warm
  sandbox to spawn a subagent (inheriting loaded deps and model handles) instead of
  cold-starting. That is the SDK hook that makes harness authors feel the fork. The
  fork-correctness handshake (RNG reseed, clock step, secret non-inheritance)
  applies per subagent.

## 4. Auto-update: the ambitious part, and it is mitos-native

Traditional self-host update is pull image, rebuild, restart, lose in-memory state,
downtime, and most people never do it (so self-hosters run months-old vulnerable
deps). mitos inverts this because the snapshot is the deploy artifact and the fork is
the deploy. When upstream releases:

1. The `track` watcher sees the new tag and re-snapshots the golden once, centrally
   (build the new template, boot to `ready`, snapshot). Cost paid once, not per user.
2. Every running instance is offered "update available, rebase?" (or auto, per
   policy).
3. Rebase is fork the new golden plus reattach your durable Workspace plus terminate
   the old fork. Your config and history carry over; the runtime is brand-new and
   patched. Near-instant, atomic, and rollback is just re-forking the old golden.

This covers all three distribution scenarios on one mechanism:

- We rebase our fork to upstream: our golden re-snapshots, instances rebase. We are
  the CD pipeline.
- Upstream adopts the `mitos.yaml`: their GitHub release is the trigger, and mitos
  becomes the official run-it-warm-and-always-current path. Zero infra for them,
  every user current.
- Standalone `awesome-mitos` repo: a repo that is only a README of buttons. Pure
  billboard and curated front door; each entry points `mitos.run/run` at the source
  repo's `mitos.yaml`.

The one-line DX promise: self-hosting that updates itself, with one-click rollback,
and no downtime. No self-host story has that.

## 5. Security: turn OpenClaw self-host risks into mitos selling points

OpenClaw is a worst-case secret profile: LLM keys (billing), channel session tokens
(WhatsApp, Telegram, Slack), the gateway token. Each self-host risk maps to a mitos
control, and each is a threat-model delta written in the same PR (operating
principle 2):

| OpenClaw self-host risk | mitos answer |
|---|---|
| Exposing the gateway to the internet (it refuses `--bind lan` without auth) | Expose auth ladder in front; never raw-public; the app's own guard is belt-and-suspenders |
| Keys leak via a shared host or baked into an image | Secrets injected per-fork, never in the golden snapshot (fork-correctness secret non-inheritance). The golden is publicly shareable because it holds no one's keys; each fork gets its own |
| Compromised dep or prompt-injection exfiltrates data | Per-sandbox egress allowlist; the app reaches only declared endpoints; arbitrary exfil is dropped at the tap |
| Container escape reaches host or other users | Firecracker microVM, kernel-level isolation, not shared-kernel containers |
| Nobody ever updates, so months-old CVEs | Auto-update re-snapshots from the patched image; fixes flow to every instance with no user action (section 4) |
| Public run-a-stranger's-repo abuse | Gated on #341 (AUP, ToS, DMCA, abuse detection, kill switch). The OpenClaw and Paperclip flagships are trusted first-party repos, so they are the dogfood that proves the capability before the public hub |

## 6. State and persistence: the two-layer model that makes it safe

Separate cattle (the microVM) from pets (your data). mitos has both layers; the
manifest wires them.

Layer 1, ephemeral warm state (the memory snapshot): a fork inherits the golden's
running memory (loaded code, warmed plugin handles, primed caches, idle
connections). This makes the click instant and is unique to fork-of-a-running-VM.

Layer 2, durable versioned state (the mitos Workspace): Workspaces are a
content-addressed, versioned filesystem with a revision DAG (git-like
WorkspaceRevisions), backed by S3-compatible object storage
(`internal/workspace/s3client.go`), with optional at-rest encryption. The manifest
`workspace` block points the durable path at one (OpenClaw `/home/node/.openclaw`,
Paperclip the Postgres data dir plus config).

Because durable state lives in the Workspace (S3), not in the VM:

- Survives node loss: VM on one node, Workspace in S3. Node dies, re-fork the golden
  on any node, reattach the Workspace, back with the same state.
- Survives auto-update: section 4 rebase is new runtime fork plus same Workspace.
  Data never moves; only cattle is replaced.
- Time-travel and rollback: Workspace revisions make "roll back to yesterday's
  config" or "restore the DB to before the bad migration" a revision checkout, not a
  backup-restore ticket.
- Fork your state, not just your runtime: `mitos fork` of a live instance CoW-forks
  the Workspace too. "Fork prod Paperclip to test a schema migration against a copy
  of real data" or "fork my OpenClaw to try a risky plugin" is instant, isolated,
  and zero risk to the original. No other platform forks production state safely.

## 7. Cost: the economics that make "everyone gets one" obvious

- CoW page sharing, measured not aspirational: mitos CI husk-probe proved CoW pages
  survive cgroup-v2 memcg boundaries. N forks of one golden share its pages; each is
  charged only its dirty delta. 1,000 OpenClaw instances are roughly one 1.5GB
  golden's pages shared plus 1,000 small dirty deltas, not 1,000 full RSS. The
  marginal user is dirty pages, not a whole VM.
- No cold-start tax: cold-booting 1,000 instances is 1,000 times roughly 16s of init
  CPU; forking is 1,000 times milliseconds of restore. The CPU you do not burn is the
  saving.
- Pause is scale-to-near-zero: mitos pause/resume snapshots full state and frees
  memory; resume restores warm. Idle instances pause and stop costing RAM; first
  request resumes them. You pay memory for active instances, S3 for everyone's state.
  Compute and storage decouple, per app instance.
- Honest per-tenant billing despite sharing: CoW-aware metering (#33) attributes
  shared-versus-private memory fairly, so you can bill or rate-limit each instance
  accurately even though they share a golden.

The unlock: marginal cost of one more user is roughly (dirty pages while active) plus
(S3 for their Workspace) plus (compute only when not paused). That is the number that
lets "click, your own always-on instance, free tier" be sustainable instead of a
money fire. It is why Binder could offer free ephemeral notebooks; mitos adds
persistent, warm, auto-updated on top.

## 8. The same mechanism, for Paperclip: proof it is a primitive

The Paperclip manifest (`examples/run-with-mitos/paperclip/mitos.yaml`) uses the
identical machinery: golden snapshot, CoW fork, Workspace persistence, `track`
re-snapshot auto-update, expose ladder, per-fork secrets. What is the same: the
schema, the `ready`-gated warm golden, secret non-inheritance, the egress allowlist,
the live URL, and auto-update as a re-fork plus Workspace reattach (so a Paperclip
release flows to every customer control plane with one-click rollback and no
migration downtime).

What is different and worth stating honestly: Paperclip is multi-service
(control-plane plus Postgres plus gateway). Two clean options on the same rails:
(a) a single golden running the stack under an init or supervisor, forking as one
unit; or (b) the DB as its own mitos Workspace volume so runtime and data scale and
roll back independently. Either way it is the same persistence and fork mechanism;
Paperclip just exercises the stateful, DB-backed, multi-process end of it.

Why this matters: if the same `mitos.yaml` plus fork plus Workspace plus `track`
makes both a viral third-party assistant (OpenClaw) and a complex company control
plane (Paperclip) one-click, warm, cheap, persistent, and self-updating, then Run
with Mitos is not a marketing button. It is the universal packaging format for any
stateful service to become instantly forkable, durably stateful, and auto-current.
OpenClaw is the public billboard; Paperclip is the dogfood proving it is load-bearing
for real, complex, stateful software. Same primitive, every app. That generality is
the no-brainer.

## 9. Production funnel: the cold-click to live-instance flow (tracked in #440)

The badge must be production-ready for non-users: a logged-out stranger clicks it and
must reach a live, isolated, metered instance. This is where most run buttons quietly
fail. The funnel runs on SaaS machinery already landing on main, state to real
surface:

1. Anonymous click to `mitos.run/run?src=...` reads the repo `mitos.yaml`, then the
   public onboarding funnel (`internal/saas/onboarding/`, #215; `mountOnboarding` is
   public and unauthenticated by design, server-gated by `caps.Signup`).
2. Identity: OAuth (GitHub or Google via the console OIDC) for provenance and
   throwaway-resistance; `account.FindOrCreateByEmail`.
3. ToS and AUP acceptance recorded at signup (#341).
4. Tenant provisioning: `internal/saas/orgprovision` (#410) provisions the per-org
   namespace; a brand-new signup lands on the most restricted quota tier by default
   (`internal/saas/quota/tier.go`, Daytona-style), so abuse and cost are bounded out
   of the box.
5. Consent and secrets: the per-fork secret screen (keys go only to your VM), scoped,
   encrypted, never logged, injected per-fork, never baked into the golden.
6. Provision and fork: fork the warm golden into the org namespace (pre-warmed org
   pools keep it instant), expose via the preview-proxy auth ladder
   (`internal/preview/proxy.go`: identity and tier routing).
7. Live and metered: usage on the billing ledger (`internal/saas/billing`); free tier
   is restricted quota plus pause-on-idle plus inactivity TTL; one-click upgrade.

The only latency a cold visitor pays over a returning user is one-time OAuth plus
secret paste; provisioning is fast (pre-warm plus the instant fork), so
stranger-to-live-instance is seconds of active time.

Two trust tiers with different gating:

- First-party flagships (OpenClaw, Paperclip): our trusted images, the user brings
  their keys. Nearer-term; needs identity, billing, quota, and per-org isolation, but
  not the full arbitrary-repo #341 gate (the code is vetted by us).
- Public arbitrary repos: untrusted code execution. Full #341 gate (AUP and ToS,
  abuse detection plus kill switch, DMCA and DSA, sanctions, public-open-source-only
  initial scope).

Production launch checklist for the public funnel: OAuth identity plus ToS recorded;
restricted-tier-by-default quota with per-org budget, rate, and resource ceilings
(#421, #341); abuse detection plus terminate-and-quarantine kill switch (#341); secret
consent plus encrypted scoped storage, never logged, per-fork injection (#341); billing
ledger plus free-tier limits plus pause-on-idle plus inactivity TTL; per-org isolation
(#410) over Firecracker microVMs with default-deny egress; DMCA designated agent, EU
DSA notice-and-action, and sanctions and geo screening on paid (counsel-led, #341); an
incident runbook (terminate, preserve evidence, notify).

## 10. Phasing (dogfood-first, public gated on #341)

1. Golden plus fork plus preview for OpenClaw (first-party, trusted): the
   `mitos.yaml`, the `ready`-gated snapshot, the expose URL. This is roughly two PRs
   of platform fixes away from working on box2 today (all fixes are merged or open).
2. Auto-update (`track` to re-snapshot to workspace-rebase): the differentiator;
   build it against OpenClaw, then Paperclip.
3. The badge, consent screen, and `mitos.run/run` front door.
4. Public arbitrary-repo path behind #341, plus the fork-native subagent SDK (#340
   piece 2).
5. `awesome-mitos` curated repo as the distribution layer.

## 11. Acceptance

A real repo with a `mitos.yaml` runs from a badge into a warm sandbox with a working
preview URL in roughly fork latency; secrets are injected per-fork and never appear
in the golden snapshot; egress is default-deny to the declared allowlist; the durable
Workspace survives a forced node move; a published upstream release re-snapshots the
golden and a running instance rebases onto it with its Workspace intact and a
one-click rollback; and a `bench/` number compares warm-fork provision and
warm-subagent spawn against the cold-start path. The same acceptance passes for both
the OpenClaw and Paperclip manifests, run from the same mechanism.
