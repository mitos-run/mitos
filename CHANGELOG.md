# Changelog

## [1.32.0](https://github.com/mitos-run/mitos/compare/v1.31.0...v1.32.0) (2026-07-07)


### Features

* **husk:** install the vmstate-only-capable patched Firecracker ([#834](https://github.com/mitos-run/mitos/issues/834)) ([8972d36](https://github.com/mitos-run/mitos/commit/8972d3642f3d6fd6691f35002716f8ea6512b84b))
* **husk:** live-cow fork snapshots vmstate only, skips the 364ms mem-file write ([#833](https://github.com/mitos-run/mitos/issues/833)) ([0c26249](https://github.com/mitos-run/mitos/commit/0c262493d4e59dc57bb00b9cf83ae4e9d53c61a0))
* **husk:** wire live-cow fork to the vmstate-only snapshot (drops the 364ms mem write) ([#836](https://github.com/mitos-run/mitos/issues/836)) ([778ee7c](https://github.com/mitos-run/mitos/commit/778ee7c5094304fafd7fdde6bd8d3da37c4369a9))

## [1.31.0](https://github.com/mitos-run/mitos/compare/v1.30.0...v1.31.0) (2026-07-07)


### Features

* **fork:** per-stage timing for the co-location fork to find the latency bottleneck ([#830](https://github.com/mitos-run/mitos/issues/830)) ([cf3091a](https://github.com/mitos-run/mitos/commit/cf3091aea7ecdefcf58516b2e6f2444324f84d80))

## [1.30.0](https://github.com/mitos-run/mitos/compare/v1.29.0...v1.30.0) (2026-07-07)


### Features

* **husk:** live-cow child boots from the shared parent memfd (sub-100ms, KVM-tested) ([#827](https://github.com/mitos-run/mitos/issues/827)) ([4abdc6c](https://github.com/mitos-run/mitos/commit/4abdc6ccd18ed5e5853be6c4b79e4abf5c217b78))

## [1.29.0](https://github.com/mitos-run/mitos/compare/v1.28.0...v1.29.0) (2026-07-07)


### Features

* **husk:** live copy-on-write fork sharing parent memory (default-off, KVM-tested) ([#820](https://github.com/mitos-run/mitos/issues/820)) ([96efe00](https://github.com/mitos-run/mitos/commit/96efe0084754be468af40ff92dd266ce9e986f33))


### Bug Fixes

* **console:** serve /internal M2M endpoints on a cluster-internal listener (GHSA-rcf5-cfv3-jxvv) ([#742](https://github.com/mitos-run/mitos/issues/742)) ([6e22227](https://github.com/mitos-run/mitos/commit/6e22227362285625468af8a974f240edb451d836))
* **hosted:** defense-in-depth hardening for the front door ([#733](https://github.com/mitos-run/mitos/issues/733)) ([#743](https://github.com/mitos-run/mitos/issues/743)) ([b8154cd](https://github.com/mitos-run/mitos/commit/b8154cd2d0240aa0c1a2c3653d878b995a81d99f))

## [1.28.0](https://github.com/mitos-run/mitos/compare/v1.27.0...v1.28.0) (2026-07-07)


### Features

* **husk:** install the patched Firecracker binary (runtime-gated live-fork, stock-identical when off) ([#817](https://github.com/mitos-run/mitos/issues/817)) ([c97c577](https://github.com/mitos-run/mitos/commit/c97c5776cb3d37be1130d389c0f180b5f56b632b))

## [1.27.0](https://github.com/mitos-run/mitos/compare/v1.26.3...v1.27.0) (2026-07-07)


### Features

* **controller:** cross-fork co-location reservation so concurrent same-source forks never over-admit ([#814](https://github.com/mitos-run/mitos/issues/814)) ([106f150](https://github.com/mitos-run/mitos/commit/106f15070e7e6ef2c5636505a1c7f8e3e756518c))

## [1.26.3](https://github.com/mitos-run/mitos/compare/v1.26.2...v1.26.3) (2026-07-07)


### Bug Fixes

* **husk:** co-located fork restores from the parent snapshot so the child inherits ([#812](https://github.com/mitos-run/mitos/issues/812)) ([10d6981](https://github.com/mitos-run/mitos/commit/10d69816bfa6b683e67b76d4b3921bd02abf9b46))

## [1.26.2](https://github.com/mitos-run/mitos/compare/v1.26.1...v1.26.2) (2026-07-07)


### Bug Fixes

* **controller:** multi-vm fork co-location reconcile (canary re-get-pod-not-found) ([#809](https://github.com/mitos-run/mitos/issues/809)) ([80e98a1](https://github.com/mitos-run/mitos/commit/80e98a1b38c097910822600860ae84030c3ed1c5))

## [1.26.1](https://github.com/mitos-run/mitos/compare/v1.26.0...v1.26.1) (2026-07-06)


### Bug Fixes

* **husk:** route ForkSnapshot through the default instance state under multi-vm ([#807](https://github.com/mitos-run/mitos/issues/807)) ([00c7cde](https://github.com/mitos-run/mitos/commit/00c7cdec7c57dd38a0093c7a18fca8593262c274))

## [1.26.0](https://github.com/mitos-run/mitos/compare/v1.25.0...v1.26.0) (2026-07-06)


### Features

* **api:** add status.vmId to Sandbox for the shared-host multi-vm mapping ([#802](https://github.com/mitos-run/mitos/issues/802)) ([34229c1](https://github.com/mitos-run/mitos/commit/34229c12965d15f40e440008934bc7bd861a84b3))
* **controller:** account co-located fork VMs against the source pod memory budget ([#804](https://github.com/mitos-run/mitos/issues/804)) ([3064e00](https://github.com/mitos-run/mitos/commit/3064e00ea8d1377f5611f7fa77fb128425b34739))
* **controller:** route a fork to a VM in the source pod behind a flag ([#803](https://github.com/mitos-run/mitos/issues/803)) ([6596b43](https://github.com/mitos-run/mitos/commit/6596b43478e054dca70470f154403bd3b404e733))
* **controller:** start warm husk pods with --multi-vm when multi-vm-fork is on ([#805](https://github.com/mitos-run/mitos/issues/805)) ([0f13527](https://github.com/mitos-run/mitos/commit/0f13527532e48823744080e881adf11c53710bdf))
* **helm:** add controller.multiVMFork value to render --multi-vm-fork ([#806](https://github.com/mitos-run/mitos/issues/806)) ([9f6d75b](https://github.com/mitos-run/mitos/commit/9f6d75be61c5c583c12c2842300afa7dfa062934))
* **helm:** add forkd AppArmor profile value ([#747](https://github.com/mitos-run/mitos/issues/747)) ([48fbd48](https://github.com/mitos-run/mitos/commit/48fbd48ab6de99b53a7fc2f6c8bb91d6517c2e4c))
* **husk:** add the spawn-vm control op to add a VM to a running pod ([#801](https://github.com/mitos-run/mitos/issues/801)) ([869e671](https://github.com/mitos-run/mitos/commit/869e671ef973c45436c5582c395aa2d7dc688849))
* **husk:** spawn a real second firecracker with a per-VM tap and IP ([#799](https://github.com/mitos-run/mitos/issues/799)) ([db00921](https://github.com/mitos-run/mitos/commit/db00921875645c5eba57ba8daef9ba0fc67e8791))

## [1.25.0](https://github.com/mitos-run/mitos/compare/v1.24.2...v1.25.0) (2026-07-06)


### Features

* **husk:** host two same-tenant VMs in one stub behind the multi-vm flag ([#772](https://github.com/mitos-run/mitos/issues/772)) ([380b520](https://github.com/mitos-run/mitos/commit/380b520db443ef4f239ceb556027d9a37f3559e9))


### Bug Fixes

* **controller:** fork child inherits the source pool network, egress, and resources ([#769](https://github.com/mitos-run/mitos/issues/769)) ([1bfe0b5](https://github.com/mitos-run/mitos/commit/1bfe0b5a05fa21504a27e70f6279a8a11dfa47d4))

## [1.24.2](https://github.com/mitos-run/mitos/compare/v1.24.1...v1.24.2) (2026-07-06)


### Bug Fixes

* **fork:** resume the source after a live fork instead of leaving it paused ([#763](https://github.com/mitos-run/mitos/issues/763)) ([34db0db](https://github.com/mitos-run/mitos/commit/34db0db60af76e6f209fac9210cbdca95813ebe3))
* **saas:** quota denials carry a specific cause and actionable remediation ([#765](https://github.com/mitos-run/mitos/issues/765)) ([a71ae83](https://github.com/mitos-run/mitos/commit/a71ae83b39e38f0a09b7decd3600a8d5c73dbf34))
* **sdk:** fork POST waits longer than the server ready deadline ([#756](https://github.com/mitos-run/mitos/issues/756)) ([ecb8a78](https://github.com/mitos-run/mitos/commit/ecb8a78712e8ccb4a2d8362c5bdf584d966c229b))

## [1.24.1](https://github.com/mitos-run/mitos/compare/v1.24.0...v1.24.1) (2026-07-06)


### Bug Fixes

* **controller:** fork child pod inherits source pod scheduling constraints ([#749](https://github.com/mitos-run/mitos/issues/749)) ([d8fcef2](https://github.com/mitos-run/mitos/commit/d8fcef2a6b364a5a33cfe9ab1ba955102c07559a))

## [1.24.0](https://github.com/mitos-run/mitos/compare/v1.23.0...v1.24.0) (2026-07-06)


### Features

* **fork:** warm the run_code kernel into the template snapshot ([#736](https://github.com/mitos-run/mitos/issues/736)) ([1849d1e](https://github.com/mitos-run/mitos/commit/1849d1e769d154bc6a9394e2428acaa492735513))
* **saas:** complete the hosted live-fork surface (telemetry, secret opt-in, fork-child billing) ([#735](https://github.com/mitos-run/mitos/issues/735)) ([76cc54b](https://github.com/mitos-run/mitos/commit/76cc54b7fdbf586d0736fe4326ea49b35288e1c2))
* **saas:** route hosted per-sandbox fork to the live FromSandbox path ([#710](https://github.com/mitos-run/mitos/issues/710)) ([e7a4b64](https://github.com/mitos-run/mitos/commit/e7a4b64378295d388fb60614bc1fcb361b41349f))


### Bug Fixes

* **agentcli:** live per-sandbox fork route and both sandbox list shapes for hosted ([#738](https://github.com/mitos-run/mitos/issues/738)) ([49696c1](https://github.com/mitos-run/mitos/commit/49696c10306cfe7d433bc49749b79678b13d796d))
* **computer-use:** pin Chromium to 147, the debian-security 150 SIGTRAPs on launch ([#724](https://github.com/mitos-run/mitos/issues/724)) ([7338bc5](https://github.com/mitos-run/mitos/commit/7338bc5e2fe71f97e0c6e9661fb9e29a06270b98)), closes [#723](https://github.com/mitos-run/mitos/issues/723)
* **husk:** report real memory, storage, and egress from the husk-pod metering endpoint ([#740](https://github.com/mitos-run/mitos/issues/740)) ([1a25f5e](https://github.com/mitos-run/mitos/commit/1a25f5e148612ad7b7d2535d92e8db531c8db2b9))
* **saas:** refuse secretRef/workspace refs in single-tenant mode (GHSA-pgv2-9w24-j7wh) ([#739](https://github.com/mitos-run/mitos/issues/739)) ([bb2ea57](https://github.com/mitos-run/mitos/commit/bb2ea57b46c124e06676d6fc5f144523661e64ee))
* **saas:** watch sandbox readiness instead of the 250ms poll ([#734](https://github.com/mitos-run/mitos/issues/734)) ([2f8b40c](https://github.com/mitos-run/mitos/commit/2f8b40c9a7e4e2e21e7dfc6f40eb938f10d5b890))

## [1.23.0](https://github.com/mitos-run/mitos/compare/v1.22.0...v1.23.0) (2026-07-05)


### Features

* **console:** follow husk-pod logs for live sandbox log streaming ([5b09c6d](https://github.com/mitos-run/mitos/commit/5b09c6d8a0b7a59cbe377ac90600271c0a1fa7f9))
* **console:** live sandbox log streaming via husk pod-log follow ([#715](https://github.com/mitos-run/mitos/issues/715)) ([8b5835c](https://github.com/mitos-run/mitos/commit/8b5835c3eb2f9237d230a1860a342737fb48d62c))
* **console:** quality follow-ups: shared modal focus, operator-plane hardening, fork partial results, waitlist intake ([20eca5d](https://github.com/mitos-run/mitos/commit/20eca5d93e515f573e542fd951be6f923001004c))
* **saas:** federation Phase 0, region-shaped single cluster ([#712](https://github.com/mitos-run/mitos/issues/712)) ([#728](https://github.com/mitos-run/mitos/issues/728)) ([11b1f3e](https://github.com/mitos-run/mitos/commit/11b1f3e4b45cc9c4da08acc5e6d7d6074a047511))


### Bug Fixes

* **console:** bound live log stream memory by bytes; heartbeat empty streams ([#726](https://github.com/mitos-run/mitos/issues/726)) ([#727](https://github.com/mitos-run/mitos/issues/727)) ([f7bbecb](https://github.com/mitos-run/mitos/commit/f7bbecb710578a67f59b915e80a63e70194ed3cc))
* **console:** gate log routes on per-project access, not just org ([0d494f0](https://github.com/mitos-run/mitos/commit/0d494f0dc309c4b0a705a09c8c700d906b4dbee5))
* **console:** heartbeat the SSE log stream while a real follow blocks ([d127d04](https://github.com/mitos-run/mitos/commit/d127d04b909cd5df19001b6bc889476c94ad3e15))
* **console:** stop the upstream log follow before every SSE return ([1daca4e](https://github.com/mitos-run/mitos/commit/1daca4e5836f29c7b846534d43a38a858abed891))
* **console:** strip trailing CR on readBoundedLine's EOF-mid-line path ([2c63f21](https://github.com/mitos-run/mitos/commit/2c63f21abf063dd8052d59f8d40e089b91d3d3ae))
* **console:** truncate oversized pod-log lines instead of erroring ([ec146b8](https://github.com/mitos-run/mitos/commit/ec146b8284d436ba1cc682f4056d10e8719e49c5))
* **mcp:** cap HTTPBackend.Exec output at 1 MiB per stream ([75fdea7](https://github.com/mitos-run/mitos/commit/75fdea7fe0e30406b3fe31f6620e25358f5fd2c5))
* **web:** focus modal container while async initial-focus target loads ([fb56a09](https://github.com/mitos-run/mitos/commit/fb56a09b515cf5d3bdb3144ffea36c329671294e))

## [1.22.0](https://github.com/mitos-run/mitos/compare/v1.21.0...v1.22.0) (2026-07-05)


### Features

* **admin:** instance-admin capability, authz gate, and overview endpoint ([2c4d8e4](https://github.com/mitos-run/mitos/commit/2c4d8e45c09c26be3c25ef035dd180c14ac15b98))
* **admin:** node inventory view over the cluster's k8s nodes ([de69162](https://github.com/mitos-run/mitos/commit/de6916228a7b062e14b9f126f291065a02c4308f))
* **admin:** org rollup table and waitlist list/approve ([281f9c4](https://github.com/mitos-run/mitos/commit/281f9c406be69c8e7d9a5b721b092e28c97a4c2b))
* **admin:** SPA Operate section (overview, orgs, nodes, waitlist) ([a5c1d4d](https://github.com/mitos-run/mitos/commit/a5c1d4dc043fb35b3f8c3701826ed5f3326f4b12))
* **billing:** Box reservation catalog and monthly ledger grants ([fd9434d](https://github.com/mitos-run/mitos/commit/fd9434d279eff5bb53e1f6a173249ea637658d2c))
* **billing:** plan entitlements and per-org capabilities advertisement ([581aba1](https://github.com/mitos-run/mitos/commit/581aba15321f026330888b61b782d219c98d7e24))
* **brand:** add website semantic type tokens to brand tokens.css ([45faa6a](https://github.com/mitos-run/mitos/commit/45faa6a63c266a876483afe22b5dfc6f8a4ca4b3))
* **console:** add accessible theme toggle to the top bar ([146b94b](https://github.com/mitos-run/mitos/commit/146b94ba9a3f287131d67cf6573b913ec0a4f0a9))
* **console:** add Bring your team nudge to the Overview ([f6e028a](https://github.com/mitos-run/mitos/commit/f6e028a68d7de2d1d316f224f77e46d90bcf9399))
* **console:** add collectDiagnostics for one-click feedback ([ba0add6](https://github.com/mitos-run/mitos/commit/ba0add695c1c092466dfadd088ef09e11656e576))
* **console:** audit events carry actor and target names, not bare ids ([dc56938](https://github.com/mitos-run/mitos/commit/dc56938e10db4ada6e6b292a77f9afe735ed7010))
* **console:** audit sentences and shared date formatting across the SPA ([2968ae4](https://github.com/mitos-run/mitos/commit/2968ae4ebe2f608d19a27a48540e997c71610aef))
* **console:** Box catalog endpoint, org-plan wiring, and Billing UI ([9eef807](https://github.com/mitos-run/mitos/commit/9eef807edf8982018e821afd615f73e98f3917f8))
* **console:** console revamp: theming, human-readable audit, invites, operate verbs, pricing shapes, operator plane, onboarding, mobile, feedback ([b3ad8df](https://github.com/mitos-run/mitos/commit/b3ad8dffb8f7275173b5388067f31eeb2201c8ed))
* **console:** default theme to dark, not OS preference ([da9e1f7](https://github.com/mitos-run/mitos/commit/da9e1f725ccfa86452eae634bf3075996b918cd5))
* **console:** durable audit persistence gains actor/target fields and a list limit ([b161a73](https://github.com/mitos-run/mitos/commit/b161a7333a6ddbd501e39ac4c01d085480336738))
* **console:** endowed progress, waiting timeout, and fork-tree celebration in FirstRun ([74b8fef](https://github.com/mitos-run/mitos/commit/74b8feff95fe131805ff91c7db80d5334b5d6aa1))
* **console:** fork tree node selection and detail panel ([263050e](https://github.com/mitos-run/mitos/commit/263050e03545a0dc76e1584df4c5c0f5f8fa3997))
* **console:** invite management endpoints, RBAC, audit, and rate limit ([c724334](https://github.com/mitos-run/mitos/commit/c7243346aa46d1f00296be7832bcb8c91a3ec620))
* **console:** live log tail, run-command panel, and header operate actions ([cc86d15](https://github.com/mitos-run/mitos/commit/cc86d156bd72ef906d57aae4cc9622dd793fe458))
* **console:** members list shows a joined name and email, not a bare id ([0b950bd](https://github.com/mitos-run/mitos/commit/0b950bd517036dcba1e7181e5d3db1001045c09a))
* **console:** mobile pass - responsive tables, stat grids, sheet modals, touch targets ([5df7cad](https://github.com/mitos-run/mitos/commit/5df7cad9da5bb5975b80da4aff988d19e2ead17c))
* **console:** new-sandbox modal, create/fork/exec client, sandboxes CTA ([ecf7e22](https://github.com/mitos-run/mitos/commit/ecf7e22090e1f615b1f2895cd1fee981c6991fd5))
* **console:** one-click feedback button in TopBar ([26ec15a](https://github.com/mitos-run/mitos/commit/26ec15a40f1f04913053e1581418b116fa24ad8e))
* **console:** sandbox create/fork/exec/live-log seam and handlers ([286de77](https://github.com/mitos-run/mitos/commit/286de77f032322274f775f092eaf49aef8b4480f))
* **console:** server-advertise feedback channel and build version ([99d1c5c](https://github.com/mitos-run/mitos/commit/99d1c5c278d13b261a79cb90ca61ab888aca2c24))
* **console:** version footer under the sidebar ownership badge ([a381982](https://github.com/mitos-run/mitos/commit/a381982356a3d5c51fce19559e963aafeae72344))
* **console:** wire create/fork/exec to the real cluster sandbox control ([fcb22c0](https://github.com/mitos-run/mitos/commit/fcb22c01f7f7ccd75d7886726129886cf1c7e10b))
* **invites:** public lookup + accept flow, signup auto-join, SPA accept page ([fdbed71](https://github.com/mitos-run/mitos/commit/fdbed712e1bdf5651011554317052cc086bcad2d))
* **members:** invite modal, pending invites section, and member removal ([5b5979e](https://github.com/mitos-run/mitos/commit/5b5979e09b5481f50da53d693bda98b0d7bd0a82))
* **onboarding:** SMTP invite email delivery, wired into the console binary ([c168cd7](https://github.com/mitos-run/mitos/commit/c168cd7a798ca92311d632bc0a00e3d60b6df9c4))
* **saas:** org invitation entity, store, and lifecycle service ([9f2d81a](https://github.com/mitos-run/mitos/commit/9f2d81a72a0eb76e777155481e1a0a18b8ba9782))


### Bug Fixes

* **console:** abort the SSE probe fetch in useLogStream ([59fbfea](https://github.com/mitos-run/mitos/commit/59fbfeab426fe5d88cdbc60b57eaa298cab58089))
* **console:** add missing audit sentences, reuse error helper, fix CSS class ([dc926a3](https://github.com/mitos-run/mitos/commit/dc926a38e9a0f6d591266f6dbac485072cd38685))
* **console:** admin overview/orgs survive a single org's read failure ([af43d94](https://github.com/mitos-run/mitos/commit/af43d94a6f0faa11dc71523f2a04ee93f2d8806b))
* **console:** audit sentence copy for profile.update and session.revoke ([af0e342](https://github.com/mitos-run/mitos/commit/af0e3421d8341ff34d3596d6505b28238054b8d7))
* **console:** bound sandbox exec, add honest sizing and live-log copy ([97a2aa4](https://github.com/mitos-run/mitos/commit/97a2aa4218ae400281f67f56017b541f2c6169e5))
* **console:** default zero invitation ExpiresAt to CreatedAt plus InvitationTTL in both stores ([40e8aa8](https://github.com/mitos-run/mitos/commit/40e8aa8bcbcb77114f9f3f6dafeb1ea392114e3c))
* **console:** default zero invitation timestamps, unbreak audit remediation lint, bound size annotation parsing ([a847b7a](https://github.com/mitos-run/mitos/commit/a847b7a3fa4f3e74267e4c3add9a515a43a64ffc))
* **console:** honest dead-invite states, focus management, and self-leave UX ([26d04a6](https://github.com/mitos-run/mitos/commit/26d04a60e5559a2787e6724858925994ca4bc5f6))
* **console:** share appearance state across ThemeToggle and Settings ([cc864b7](https://github.com/mitos-run/mitos/commit/cc864b712244d388c34c249b214709af3b8d7602))
* **console:** stop the page body from scrolling horizontally on phones ([c6d1a51](https://github.com/mitos-run/mitos/commit/c6d1a512ce04ab388205a7a2c3815900d5d50e44))
* **console:** use shared modal pattern for remove-member confirm ([7a66127](https://github.com/mitos-run/mitos/commit/7a6612710d146f8746e791b05eb4af2719bf81e8))
* **firstrun:** drop the dead ExecStream curl from the synthetic trigger ([7a4ee50](https://github.com/mitos-run/mitos/commit/7a4ee50c44ce5bd897bb287cc68b7c4777fdf9cc))
* **firstrun:** ground synthetic-trigger curl in a real runtime route ([462fe67](https://github.com/mitos-run/mitos/commit/462fe6756d06167de86e81e9b9e895f981cc0052))
* **saas:** atomic invite delivery, honest rate-limit accounting, no self-demote on accept ([d8ce100](https://github.com/mitos-run/mitos/commit/d8ce1006bacaf1ccd4b7c7de2eea89511c6245fd))
* **saas:** close invite privilege-escalation, header-injection, and race findings ([b35b047](https://github.com/mitos-run/mitos/commit/b35b0478ed256b6b370d7be20a9c936b08e9beb7))
* unbreak unique-violation detection, type admin/orgs response, refix panel focus ([e060b1a](https://github.com/mitos-run/mitos/commit/e060b1a5fbb9d43b911c16a8dbfa53ce1ee2769e))

## [1.21.0](https://github.com/mitos-run/mitos/compare/v1.20.4...v1.21.0) (2026-07-05)


### Features

* **chart:** opt-in control-plane egress lockdown with SMTP allow derived from smtp values ([#705](https://github.com/mitos-run/mitos/issues/705)) ([0f92437](https://github.com/mitos-run/mitos/commit/0f924373e4d20a57b1a8883b2c6123746b114643))

## [1.20.4](https://github.com/mitos-run/mitos/compare/v1.20.3...v1.20.4) (2026-07-04)


### Bug Fixes

* **frontdoor:** route marketing pages + /mitos vanity to marketing; noindex console shell; www 301 ([#706](https://github.com/mitos-run/mitos/issues/706)) ([5299a81](https://github.com/mitos-run/mitos/commit/5299a81415c2c13fb45862b5eb4ef97815f7ed23))

## [1.20.3](https://github.com/mitos-run/mitos/compare/v1.20.2...v1.20.3) (2026-07-04)


### Bug Fixes

* **controller:** terminal-phase husk reap covers Failed claims; billing tail never dropped ([#701](https://github.com/mitos-run/mitos/issues/701)) ([7e947e9](https://github.com/mitos-run/mitos/commit/7e947e955dbb14b13fdbe77120e3944a74025800))

## [1.20.2](https://github.com/mitos-run/mitos/compare/v1.20.1...v1.20.2) (2026-07-04)


### Bug Fixes

* **controller:** fail a fork terminally when its source terminated or vanished ([#700](https://github.com/mitos-run/mitos/issues/700)) ([a539930](https://github.com/mitos-run/mitos/commit/a539930a57a459b3dda5f34f88442fb4163a00d1))

## [1.20.1](https://github.com/mitos-run/mitos/compare/v1.20.0...v1.20.1) (2026-07-04)


### Bug Fixes

* **controller:** lifetime terminate actually stops the husk VM ([#697](https://github.com/mitos-run/mitos/issues/697)) ([b657258](https://github.com/mitos-run/mitos/commit/b657258c4452f972ecbaab2e95699b46ff4acc3a))

## [1.20.0](https://github.com/mitos-run/mitos/compare/v1.19.5...v1.20.0) (2026-07-04)


### Features

* **saas:** alerting for the hosted control plane (gateway, billing webhooks, drawdown, collector, console) ([#693](https://github.com/mitos-run/mitos/issues/693)) ([33fbbc4](https://github.com/mitos-run/mitos/commit/33fbbc45813c3c85ae77664e2aac10f7ffa1aff8))


### Bug Fixes

* **controller:** husk pods are reapable after every rebuild, digest or no digest ([#686](https://github.com/mitos-run/mitos/issues/686)) ([c8f32ce](https://github.com/mitos-run/mitos/commit/c8f32ce73daf8aec3b7408544ba54d051b7e19d5))
* **saas:** enforce live concurrency at the gateway; console suspensions reach the shared store ([#689](https://github.com/mitos-run/mitos/issues/689)) ([32b33d1](https://github.com/mitos-run/mitos/commit/32b33d1b611da51f3d05e49c0cedd29274b42e4f))

## [1.19.5](https://github.com/mitos-run/mitos/compare/v1.19.4...v1.19.5) (2026-07-04)


### Bug Fixes

* **usage:** concurrent husk scrape, terminate-time final sample, cycle summary with settled cents ([#687](https://github.com/mitos-run/mitos/issues/687)) ([b7da3fa](https://github.com/mitos-run/mitos/commit/b7da3fa3f193c79d0b0160efd49c38f84a4b8a6f))
* **usage:** cycle-failure counter, reconciler-clock termination stamps, honest error-path stats ([#692](https://github.com/mitos-run/mitos/issues/692)) ([9e0ca82](https://github.com/mitos-run/mitos/commit/9e0ca82caddf352e4c5cfb0face28c6a1c59169c)), closes [#682](https://github.com/mitos-run/mitos/issues/682)

## [1.19.4](https://github.com/mitos-run/mitos/compare/v1.19.3...v1.19.4) (2026-07-04)


### Bug Fixes

* **guest:** init skips kernel-premounted targets, ending the false /dev ERROR ([#683](https://github.com/mitos-run/mitos/issues/683)) ([24d766c](https://github.com/mitos-run/mitos/commit/24d766cb05958b767ca773188a7fa93c5e09346b))

## [1.19.3](https://github.com/mitos-run/mitos/compare/v1.19.2...v1.19.3) (2026-07-03)


### Bug Fixes

* **daemon:** exec ws accepts origin-bearing clients; bearer token is the gate ([#680](https://github.com/mitos-run/mitos/issues/680)) ([66cf371](https://github.com/mitos-run/mitos/commit/66cf3714da0aed6264700bb782f2bb8c5588aecb))

## [1.19.2](https://github.com/mitos-run/mitos/compare/v1.19.1...v1.19.2) (2026-07-03)


### Bug Fixes

* **agent-rs:** mount devpts and honor PTY stdin_close so interactive PTY works ([#670](https://github.com/mitos-run/mitos/issues/670)) ([b4efde5](https://github.com/mitos-run/mitos/commit/b4efde527600e9236e62ddabbec750d739d0a4c5))

## [1.19.1](https://github.com/mitos-run/mitos/compare/v1.19.0...v1.19.1) (2026-07-03)


### Bug Fixes

* **saas:** drawdown skips settled windows before pricing; markers leave the ledger ([#675](https://github.com/mitos-run/mitos/issues/675)) ([d6880ec](https://github.com/mitos-run/mitos/commit/d6880ec5cd85d845b6fc37b1169445afcb7e3569)), closes [#672](https://github.com/mitos-run/mitos/issues/672)
* **saas:** usage records carry the API-visible sandbox id, not the husk pod name ([#673](https://github.com/mitos-run/mitos/issues/673)) ([d163f2e](https://github.com/mitos-run/mitos/commit/d163f2ef8b91c9260f151fd2163e7d06c88dbee1)), closes [#663](https://github.com/mitos-run/mitos/issues/663)

## [1.19.0](https://github.com/mitos-run/mitos/compare/v1.18.0...v1.19.0) (2026-07-03)


### Features

* **chart:** values.schema.json rejects unknown keys at install time ([#667](https://github.com/mitos-run/mitos/issues/667)) ([d30c633](https://github.com/mitos-run/mitos/commit/d30c6334b6c345c62fa82b33de9691c9e4241686))


### Bug Fixes

* **saas:** drawdown accumulates usage in milli-cents so sub-cent windows bill ([#666](https://github.com/mitos-run/mitos/issues/666)) ([c2976c5](https://github.com/mitos-run/mitos/commit/c2976c5b8416a77f0760a6d88f3b4e5544312c83)), closes [#662](https://github.com/mitos-run/mitos/issues/662)

## [1.18.0](https://github.com/mitos-run/mitos/compare/v1.17.0...v1.18.0) (2026-07-03)


### Features

* **husk:** expose per-VM metering so the usage collector meters husk sandboxes ([#627](https://github.com/mitos-run/mitos/issues/627)) ([2dc0c53](https://github.com/mitos-run/mitos/commit/2dc0c53abafefada7fc6da92f0fcec5b2eed534b))

## [1.17.0](https://github.com/mitos-run/mitos/compare/v1.16.0...v1.17.0) (2026-07-03)


### Features

* **fork:** live-state fork carries the source filesystem and running kernel (not the template) ([#611](https://github.com/mitos-run/mitos/issues/611)) ([c4ab5f5](https://github.com/mitos-run/mitos/commit/c4ab5f536083d734ba9bb879383221ea84f1e452))


### Bug Fixes

* **brand:** dark --ink-3 label token meets AA small-text contrast ([#652](https://github.com/mitos-run/mitos/issues/652)) ([38d4357](https://github.com/mitos-run/mitos/commit/38d43575be710f20ef8e0b03616c633eb31384fb)), closes [#635](https://github.com/mitos-run/mitos/issues/635)
* **cli:** windows build of mitos, statfs check split behind build tags ([#654](https://github.com/mitos-run/mitos/issues/654)) ([194c5ac](https://github.com/mitos-run/mitos/commit/194c5ac9b834e19849af001bc9d971c21ccfb9b8))

## [1.16.0](https://github.com/mitos-run/mitos/compare/v1.15.2...v1.16.0) (2026-07-03)


### Features

* **cli:** mitos init takes a new user from key-in-hand to a verified setup ([#639](https://github.com/mitos-run/mitos/issues/639)) ([fe1f9f4](https://github.com/mitos-run/mitos/commit/fe1f9f47a640230e1e0fbf767ecac0d5f2254227))
* **console:** light theme following prefers-color-scheme with manual override ([#634](https://github.com/mitos-run/mitos/issues/634)) ([fcfacff](https://github.com/mitos-run/mitos/commit/fcfacffdc11e36ea44fea3e3272310768d24fad9)), closes [#621](https://github.com/mitos-run/mitos/issues/621)
* **saas:** webhook and checkout write the org to billing-customer link ([#645](https://github.com/mitos-run/mitos/issues/645)) ([916fe09](https://github.com/mitos-run/mitos/commit/916fe0930b19b95def1fe91d1feeaff0a90e695f))


### Bug Fixes

* **saas:** durable audit log so the org audit trail survives console restarts ([#633](https://github.com/mitos-run/mitos/issues/633)) ([d40a701](https://github.com/mitos-run/mitos/commit/d40a701e537b8e669df939c1801ac4492148ab61)), closes [#616](https://github.com/mitos-run/mitos/issues/616)
* **saas:** durable, replica-shared kill-switch suspension store ([#632](https://github.com/mitos-run/mitos/issues/632)) ([80a50d4](https://github.com/mitos-run/mitos/commit/80a50d46fdb7dfd0b4dbd19142fc1beaaccdad8c))
* **saas:** graceful shutdown, real readiness, and PDBs for the hosted control plane ([#624](https://github.com/mitos-run/mitos/issues/624)) ([63b610b](https://github.com/mitos-run/mitos/commit/63b610b10600143283da6bbcb26957b3e8919246))

## [1.15.2](https://github.com/mitos-run/mitos/compare/v1.15.1...v1.15.2) (2026-07-03)


### Bug Fixes

* **saas:** create error journey speaks accurately (missing field, unknown route) ([#649](https://github.com/mitos-run/mitos/issues/649)) ([8713bab](https://github.com/mitos-run/mitos/commit/8713bab59ada905499ae80e970a8096dbb68f338))

## [1.15.1](https://github.com/mitos-run/mitos/compare/v1.15.0...v1.15.1) (2026-07-03)


### Bug Fixes

* **saas:** create fails fast and legibly on an unknown pool ([#646](https://github.com/mitos-run/mitos/issues/646)) ([206de66](https://github.com/mitos-run/mitos/commit/206de66e1f741303b2d343a460a0a8693b8e275a))

## [1.15.0](https://github.com/mitos-run/mitos/compare/v1.14.0...v1.15.0) (2026-07-03)


### Features

* **saas:** self-host operator knobs as first-class Helm values, configurable billing rates ([#623](https://github.com/mitos-run/mitos/issues/623)) ([a8162b3](https://github.com/mitos-run/mitos/commit/a8162b36c2213babd87ea76abcecd944da123918))


### Bug Fixes

* **console:** plain-language copy, signup gating on self-host, and boot loading states ([#626](https://github.com/mitos-run/mitos/issues/626)) ([a25e000](https://github.com/mitos-run/mitos/commit/a25e000f6803e68b85a432fe4edb504ecbda6fa7))
* **controller:** sandbox with a nonexistent pool fails terminally after a bounded grace period ([#637](https://github.com/mitos-run/mitos/issues/637)) ([2833fe7](https://github.com/mitos-run/mitos/commit/2833fe745e3a5b3a789fb8add6dfbda41b25f43f)), closes [#630](https://github.com/mitos-run/mitos/issues/630)
* **saas:** auth 401s speak to their own auth context, not the sandbox token ([#638](https://github.com/mitos-run/mitos/issues/638)) ([8e439a6](https://github.com/mitos-run/mitos/commit/8e439a62a4c5363d6cbf703827127b02236cf207))
* **saas:** billing status and org-customer map are durable in Postgres ([#629](https://github.com/mitos-run/mitos/issues/629)) ([e354f80](https://github.com/mitos-run/mitos/commit/e354f8013805ca8f1af086237cf256605758ce5d)), closes [#614](https://github.com/mitos-run/mitos/issues/614)

## [1.14.0](https://github.com/mitos-run/mitos/compare/v1.13.1...v1.14.0) (2026-07-02)


### Features

* **saas:** hosted pause and resume via the sandbox lifecycle proxy ([#609](https://github.com/mitos-run/mitos/issues/609)) ([5d9f7a7](https://github.com/mitos-run/mitos/commit/5d9f7a70c9aa8a54ff2861ffcf49ce4e2534fc35)), closes [#601](https://github.com/mitos-run/mitos/issues/601)
* **saas:** wire usage metering end to end (claim-time org attribution, chart knobs, drawdown driver) ([#610](https://github.com/mitos-run/mitos/issues/610)) ([73516c1](https://github.com/mitos-run/mitos/commit/73516c12554203caa516a5b4093778e0976f6505))


### Bug Fixes

* **console:** first-run cli snippets use the real verb, mitos sandbox exec ([#606](https://github.com/mitos-run/mitos/issues/606)) ([82f9f6c](https://github.com/mitos-run/mitos/commit/82f9f6cd36e916aff9910b0ac1ce40d98543e85a))
* **console:** first-run snippets are self-contained and produce real output ([#608](https://github.com/mitos-run/mitos/issues/608)) ([ae9414a](https://github.com/mitos-run/mitos/commit/ae9414a3a7711a135f952cc85fdbc5058a0c6b84))

## [1.13.1](https://github.com/mitos-run/mitos/compare/v1.13.0...v1.13.1) (2026-07-02)


### Bug Fixes

* **console:** first-run snippets use the hosted surface, not cluster-mode AgentRun ([#603](https://github.com/mitos-run/mitos/issues/603)) ([#604](https://github.com/mitos-run/mitos/issues/604)) ([2b8806a](https://github.com/mitos-run/mitos/commit/2b8806a7718e0b9b37017f2eda350238b2405afd))
* **saas:** sandboxes scope implies read-only so default keys can list ([#599](https://github.com/mitos-run/mitos/issues/599)) ([#600](https://github.com/mitos-run/mitos/issues/600)) ([fb3bc2a](https://github.com/mitos-run/mitos/commit/fb3bc2aed8fcfcb68badb24222ba4afc07a7047d))

## [1.13.0](https://github.com/mitos-run/mitos/compare/v1.12.0...v1.13.0) (2026-07-02)


### Features

* **api:** SandboxPool rebuild bookkeeping fields ([#584](https://github.com/mitos-run/mitos/issues/584)) ([e4f3eaa](https://github.com/mitos-run/mitos/commit/e4f3eaa35515b4d1944af5eef926248b88200913))
* **controller:** detect restore-failing husks and rebuild the template with backoff ([#584](https://github.com/mitos-run/mitos/issues/584)) ([f6ecb77](https://github.com/mitos-run/mitos/commit/f6ecb77220d3bb454748b280b5ecd63993051723))
* **controller:** TemplateBuilt condition and force-rebuild annotation ([#584](https://github.com/mitos-run/mitos/issues/584), closes [#578](https://github.com/mitos-run/mitos/issues/578)) ([e8d4dc4](https://github.com/mitos-run/mitos/commit/e8d4dc45da4aad5bf354383cc2f73097f31f7a8e))
* **fork:** reuse-or-rebuild gate for on-disk templates in CreateTemplate ([#584](https://github.com/mitos-run/mitos/issues/584)) ([bf9590d](https://github.com/mitos-run/mitos/commit/bf9590df2c3093b8583a518a4c2b2d46348d761c))
* **guest:** mitos-python base image with the run_code kernel baked in ([#572](https://github.com/mitos-run/mitos/issues/572)) ([#595](https://github.com/mitos-run/mitos/issues/595)) ([c606363](https://github.com/mitos-run/mitos/commit/c60636365a2a585a0406ef1d67a892165564c3e0))
* **sdk:** one-import Daytona-compat shim (mitos.daytona) + migration doc ([#592](https://github.com/mitos-run/mitos/issues/592)) ([765146f](https://github.com/mitos-run/mitos/commit/765146f9635b4811bf1fbe5abc9d36c94438cd25))
* self-healing template snapshot rebuild ([0aab570](https://github.com/mitos-run/mitos/commit/0aab5702d32dee4cf22511d23c47ec6ce084017b))


### Bug Fixes

* **ci:** stop the publish SBOM step from failing on release-asset upload ([db903b0](https://github.com/mitos-run/mitos/commit/db903b04bff8f82265ef0bb5b32d0b2fdba10000))
* **ci:** stop the publish SBOM step from failing on release-asset upload ([ab2787d](https://github.com/mitos-run/mitos/commit/ab2787d39afd58b7f2eaf69b0887fee2bffc4c99))
* **firecracker:** normalize template artifact ownership after the jailed build ([0eebf29](https://github.com/mitos-run/mitos/commit/0eebf2979fc78b886fafa5811625057da64efd1d))
* **firecracker:** normalize template artifact ownership after the jailed build ([#583](https://github.com/mitos-run/mitos/issues/583)) ([c3aa75c](https://github.com/mitos-run/mitos/commit/c3aa75cd2071a9d9c5087da1262b3f2e8b583eaf))
* **forkd:** fail-closed readiness gate on missing KVM devices ([095ca20](https://github.com/mitos-run/mitos/commit/095ca20f67e96f290128bbb8708a25591022648e))
* **saas:** mint label-safe ids; surface api server validation errors ([#593](https://github.com/mitos-run/mitos/issues/593)) ([#594](https://github.com/mitos-run/mitos/issues/594)) ([e98d113](https://github.com/mitos-run/mitos/commit/e98d113425e3dea6da43e0dea18d06d41b89ce22))

## [1.12.0](https://github.com/mitos-run/mitos/compare/v1.11.1...v1.12.0) (2026-07-01)


### Features

* **canary:** synthetic canary probing the fork+exec path with alerts ([#580](https://github.com/mitos-run/mitos/issues/580)) ([0f45cf7](https://github.com/mitos-run/mitos/commit/0f45cf7e24abba2c3494a8847277305d60fe88b1))
* **doctor:** add data-dir free-space check; document data-disk selection ([#577](https://github.com/mitos-run/mitos/issues/577)) ([9a32b92](https://github.com/mitos-run/mitos/commit/9a32b92188934409f4885553122593fc1aa3e209))
* **monitoring:** scrape control-plane metrics via PodMonitors ([#581](https://github.com/mitos-run/mitos/issues/581)) ([851d6c5](https://github.com/mitos-run/mitos/commit/851d6c5b74aa9bd350275cf7003c583f53b08273))


### Bug Fixes

* **sdk:** default to api.mitos.run, in-cluster-first resolution, robust onboarding ([#570](https://github.com/mitos-run/mitos/issues/570)) ([ccab707](https://github.com/mitos-run/mitos/commit/ccab707b749e7bc259a85087ff70321b3e22e84c))

## [1.11.1](https://github.com/mitos-run/mitos/compare/v1.11.0...v1.11.1) (2026-07-01)


### Bug Fixes

* **deploy:** inject /dev/vhost-vsock into forkd via the device plugin ([#573](https://github.com/mitos-run/mitos/issues/573)) ([af84bfa](https://github.com/mitos-run/mitos/commit/af84bfa5cb2d1fd9428ba246680add1b1fbae52c))

## [1.11.0](https://github.com/mitos-run/mitos/compare/v1.10.0...v1.11.0) (2026-07-01)


### Features

* **console:** complete the hosted journey (first-run aha, credits, Paddle top-up), gated ([#567](https://github.com/mitos-run/mitos/issues/567)) ([dcbeff5](https://github.com/mitos-run/mitos/commit/dcbeff5e134cd426bba511a9ccb62c9569b5a886))
* **console:** route /waitlist and show a calm not-configured add-credits state ([#569](https://github.com/mitos-run/mitos/issues/569)) ([d72f893](https://github.com/mitos-run/mitos/commit/d72f89384f0a3f48b112c42036d8fd0efbba2b7c))

## [1.10.0](https://github.com/mitos-run/mitos/compare/v1.9.0...v1.10.0) (2026-06-30)


### Features

* **compose:** Harbor compose provider contract (per-service ops, fail-closed backend) ([#562](https://github.com/mitos-run/mitos/issues/562)) ([1cd87e3](https://github.com/mitos-run/mitos/commit/1cd87e3de9b2e4c3ade09baeeb866220add38b27))
* **saas:** durable Postgres usage store for per-org metered usage ([#211](https://github.com/mitos-run/mitos/issues/211)) ([#563](https://github.com/mitos-run/mitos/issues/563)) ([ed6e66e](https://github.com/mitos-run/mitos/commit/ed6e66e49df1216bc006ccfe70e12945201f7edb))
* **sdk:** fork-native subagent hook with graceful off-mitos fallback ([#561](https://github.com/mitos-run/mitos/issues/561)) ([dc667b1](https://github.com/mitos-run/mitos/commit/dc667b12ed84817410f8a915f4fbfe655332b67d))
* **sniproxy:** host-side TLS SNI egress allowlist (peek-and-splice) ([#564](https://github.com/mitos-run/mitos/issues/564)) ([d460cbb](https://github.com/mitos-run/mitos/commit/d460cbb75d3b1c36f1b47d7777e27c052ea33536)), closes [#494](https://github.com/mitos-run/mitos/issues/494)

## [1.9.0](https://github.com/mitos-run/mitos/compare/v1.8.0...v1.9.0) (2026-06-30)


### Features

* **daemon:** add data-dir disk headroom to the capacity heartbeat ([#553](https://github.com/mitos-run/mitos/issues/553)) ([80111a7](https://github.com/mitos-run/mitos/commit/80111a79b024f25ba0c84f7bf1a6aa9e9411b330)), closes [#465](https://github.com/mitos-run/mitos/issues/465)
* **runmanifest:** inject MITOS_PUBLIC_URL and template ${MITOS_PUBLIC_URL} ([#559](https://github.com/mitos-run/mitos/issues/559)) ([309a013](https://github.com/mitos-run/mitos/commit/309a0133dec7fab6e77b1a36e663a06ddd5cac62)), closes [#476](https://github.com/mitos-run/mitos/issues/476)


### Bug Fixes

* **controller:** auto re-fork raw-forkd claims after node loss ([#372](https://github.com/mitos-run/mitos/issues/372)) ([#558](https://github.com/mitos-run/mitos/issues/558)) ([30a5a23](https://github.com/mitos-run/mitos/commit/30a5a2374a64081152c99729f6d260ff1787ccb8))
* **controller:** rebuild pool snapshot on template-content edits ([#554](https://github.com/mitos-run/mitos/issues/554)) ([93183d8](https://github.com/mitos-run/mitos/commit/93183d8d2931723db49173d7581fe4adb0c29c81))
* **controller:** surface DrainPolicy Checkpoint degrade honestly, not as a silent Kill ([#552](https://github.com/mitos-run/mitos/issues/552)) ([9912bf7](https://github.com/mitos-run/mitos/commit/9912bf7953141834813e38a589915494db24fcb1)), closes [#374](https://github.com/mitos-run/mitos/issues/374)
* **snapcompat:** refuse stale snapshots on guest-agent protocol skew ([#459](https://github.com/mitos-run/mitos/issues/459)) ([#549](https://github.com/mitos-run/mitos/issues/549)) ([3531ed8](https://github.com/mitos-run/mitos/commit/3531ed89ccfc74fde7ae8601a2f78864e80ade7c))

## [1.8.0](https://github.com/mitos-run/mitos/compare/v1.7.0...v1.8.0) (2026-06-30)


### Features

* **fork:** live-fork networked sandboxes via per-sandbox egress proxy ([#336](https://github.com/mitos-run/mitos/issues/336)) ([#547](https://github.com/mitos-run/mitos/issues/547)) ([e08b76b](https://github.com/mitos-run/mitos/commit/e08b76b7b9f2e8cdea0d22d71b47853ef8211f2f))


### Bug Fixes

* **expose:** forward the public host + X-Forwarded-* to exposed apps ([#476](https://github.com/mitos-run/mitos/issues/476)) ([#478](https://github.com/mitos-run/mitos/issues/478)) ([5111cfd](https://github.com/mitos-run/mitos/commit/5111cfdf0fa03fe7bee83ea2dc2ed92d9bff2044))

## [1.7.0](https://github.com/mitos-run/mitos/compare/v1.6.0...v1.7.0) (2026-06-29)


### Features

* **chart:** hardened-runtime forkd profile (Talos seccomp + CAP_FOWNER) ([732a95f](https://github.com/mitos-run/mitos/commit/732a95fffd12c90878b500f8f92a4fdfe1572558))
* **cli:** add HostedBackend for sandbox verbs against the hosted gateway ([103ad67](https://github.com/mitos-run/mitos/commit/103ad678ca6ba7dc5d1a9c3458e8d6a456affef9))
* **cli:** hosted mode (API key + base URL) for sandbox create/exec/fork/ls/terminate ([794e950](https://github.com/mitos-run/mitos/commit/794e9501de09295d7f08f35cb107aa7f828a08b2))
* **console:** show social login buttons only for configured connectors ([4e9d741](https://github.com/mitos-run/mitos/commit/4e9d74154ad5bc989704a5500d61150be52ae62c))
* **console:** show social login buttons only for configured connectors ([f88d8c8](https://github.com/mitos-run/mitos/commit/f88d8c811a43eddf80a478d072ead9e1bb401d9e))
* **controller:** map sandbox cpu to the husk limit with a low request for node overcommit ([6749205](https://github.com/mitos-run/mitos/commit/6749205fe879c004c9f9d381fab4735ce93f74a9))
* **controller:** map sandbox cpu to the husk limit with a low request for node overcommit ([10304f1](https://github.com/mitos-run/mitos/commit/10304f16a765f156bceb82170b96e82291648fda))
* **frontdoor:** add Pages dial override to marketing proxy (SNI pinned, no InsecureSkipVerify) ([c3e25b8](https://github.com/mitos-run/mitos/commit/c3e25b83cee4962f0fd784b65a339316c2679a21))
* **frontdoor:** parse MITOS_FRONTDOOR_MARKETING_PAGES_ADDRS and wire into ProxyConfig ([ef47cbe](https://github.com/mitos-run/mitos/commit/ef47cbe95e5f3f677f79e0ff9acc6c5714ac4cdf))
* **frontdoor:** proxy marketing to github pages (sni/host pinned) and mount resolve-token under frontdoor ([1b1d231](https://github.com/mitos-run/mitos/commit/1b1d231dec5ce1efc9f49262ec680adebd343436))
* **frontdoor:** render MITOS_FRONTDOOR_MARKETING_PAGES_ADDRS from frontdoor.marketingPagesAddrs ([80fce20](https://github.com/mitos-run/mitos/commit/80fce20bbc3fef49987a8693114e380646a409c8))
* **gateway:** proxy the interactive PTY WebSocket to the owning sandbox ([#532](https://github.com/mitos-run/mitos/issues/532)) ([7975556](https://github.com/mitos-run/mitos/commit/797555652bc19a881953575b8153a1e6090b3eee))
* **gateway:** single-tenant-namespace override for the control plane ([399bd85](https://github.com/mitos-run/mitos/commit/399bd85545f772f33ada2bb053d34dfc9d0d8741))
* **gateway:** single-tenant-namespace override for the control plane ([6bb2773](https://github.com/mitos-run/mitos/commit/6bb2773ce7d0d2a28845f406f9775d1a2be0833e))
* **onboarding:** add E2ETokenSink seam and MemE2ETokenSink ([62393e4](https://github.com/mitos-run/mitos/commit/62393e436c04fa956b7af7bff6e25f88b55ea1e3))
* **onboarding:** E2EHandler with bearer + domain + sink gates ([514686b](https://github.com/mitos-run/mitos/commit/514686b44009b102dc971bc559830a94a7e95154))
* **onboarding:** gated QA verify-token seam for hosted e2e ([55a8767](https://github.com/mitos-run/mitos/commit/55a8767a2880134e820be191f6ead3077acd3063))
* **onboarding:** wire E2E sink and mount gated endpoint in console ([3ed8b56](https://github.com/mitos-run/mitos/commit/3ed8b569ed377ff312f65aaeec300ea6f09605aa))


### Bug Fixes

* accept mitos_ keys in hosted harness; env-configurable signup credit ([6f24445](https://github.com/mitos-run/mitos/commit/6f244450f09523a1b3feb68f9f4c66f8984651d7))
* accept mitos_ keys in hosted harness; env-configurable signup credit ([45bc82f](https://github.com/mitos-run/mitos/commit/45bc82f3edcacce3eaede930971f0295a48b41d2))
* **chart:** harden marketing pod (read-only root) ([ae0fd18](https://github.com/mitos-run/mitos/commit/ae0fd18f6ba21067265901e13129a886e96f5e80))
* **chart:** marketing container port 8080 ([96668bc](https://github.com/mitos-run/mitos/commit/96668bce5fd81f431d3b70e56e761d93e5cef0a3))
* **chart:** marketing container port 8080 (nginx-unprivileged) so the service has endpoints ([8c4ee47](https://github.com/mitos-run/mitos/commit/8c4ee47c41ac933101b15c42a27c53ca7713d13c))
* **chart:** marketing read-only-root writable mounts ([4ffd756](https://github.com/mitos-run/mitos/commit/4ffd75673eb512cb18e8ecc6e3e35820a8cbe904))
* **chart:** marketing readOnlyRootFilesystem off ([3a53b08](https://github.com/mitos-run/mitos/commit/3a53b08cf95a25a1320f53586452a0711ddac41c))
* **chart:** marketing readOnlyRootFilesystem off (nginx-unprivileged entrypoint needs writes) ([d4cd8af](https://github.com/mitos-run/mitos/commit/d4cd8afa16d4fff4548da92adeb758aedeb5d796))
* **chart:** re-enable marketing readOnlyRootFilesystem by bypassing nginx entrypoint ([75deca7](https://github.com/mitos-run/mitos/commit/75deca76fb6bc8e251d18189f9da854fa28807d7))
* **charts:** mount MITOS_IDENTITY_RESOLVE_TOKEN in console when frontdoor.enabled ([3c60d05](https://github.com/mitos-run/mitos/commit/3c60d05abf38f2c11deaa71cc6f857bd05d889dd))
* **chart:** writable /tmp + /var/cache/nginx for read-only-root marketing pod ([04de768](https://github.com/mitos-run/mitos/commit/04de768c90c8a48f8b22860c03b25b2569a314d9))
* **forkd:** keep jailer template links CoW across the chroot mount ([#526](https://github.com/mitos-run/mitos/issues/526)) ([d79bd9d](https://github.com/mitos-run/mitos/commit/d79bd9d875157fe5cdb7134f2545ac142897bcf3))
* **fork:** gate SIGUSR2 broadcast on a handler ([#467](https://github.com/mitos-run/mitos/issues/467)) and reap stale-digest husk pods ([#461](https://github.com/mitos-run/mitos/issues/461)) ([#509](https://github.com/mitos-run/mitos/issues/509)) ([fa5d6ad](https://github.com/mitos-run/mitos/commit/fa5d6ad37068842a3543489fc0384e9fbb7d160d))
* **frontdoor:** /assets is the console Vite bundle (not marketing); /_astro is marketing ([da29d46](https://github.com/mitos-run/mitos/commit/da29d463c73f0b694c1759640aa8be7844887d3a))
* **frontdoor:** extension-based static assets to marketing ([14ad3b2](https://github.com/mitos-run/mitos/commit/14ad3b29b07795bbbdeea289fdeee268d7c12ce7))
* **frontdoor:** fix race in Pages dial test and uint64 index overflow ([46b50d9](https://github.com/mitos-run/mitos/commit/46b50d92334e72498cf227c2a07614ab839fb065))
* **frontdoor:** no session 302 on console paths ([3ed584a](https://github.com/mitos-run/mitos/commit/3ed584a705f86f57d3ee09ae4f72b4d3b0883c82))
* **frontdoor:** pass anon console requests through (console owns auth) instead of 302 to login ([6aaca62](https://github.com/mitos-run/mitos/commit/6aaca6230119de7714e26b16999abbe65b6ea079))
* **frontdoor:** route /assets to console, /_astro to marketing ([5ae12d0](https://github.com/mitos-run/mitos/commit/5ae12d05553e49114109500b1d57dd2419965efb))
* **frontdoor:** route /webhooks/* to console as public (was 302 to login) ([7398341](https://github.com/mitos-run/mitos/commit/739834162d5acc41829708f7249eba5a4b8208be))
* **frontdoor:** route /webhooks/* to console as public (was 302 to login) ([086b9e3](https://github.com/mitos-run/mitos/commit/086b9e31a7947ff7f9d4ada9b409f18c8de3a43e))
* **frontdoor:** route root static assets (by extension) to marketing so the site's images/css load ([2c0cdc2](https://github.com/mitos-run/mitos/commit/2c0cdc2537ecc8c7e2615752bd258541874104da))
* **gateway:** handle POST/GET /v1/templates so hosted SDK create works ([e787c5e](https://github.com/mitos-run/mitos/commit/e787c5e6db3d2fcccae1ac808c38536bd8ea651f))
* **gateway:** handle POST/GET /v1/templates so hosted SDK create works ([b1af307](https://github.com/mitos-run/mitos/commit/b1af307ed6debdc162c887dfb78951c0cc0820f5))
* **gateway:** map POST /v1/fork to sandbox.create so hosted SDK create+fork works ([47a27f9](https://github.com/mitos-run/mitos/commit/47a27f991407213b2901a4967ad67a962ca4fac4))
* **gateway:** map POST /v1/fork to sandbox.create so hosted SDK create+fork works ([d9dd45d](https://github.com/mitos-run/mitos/commit/d9dd45dbdac6e3babef98621575999a05b5496c5))
* **husk-stub:** remove stale per-vm jailer chroot before start so retries do not fail MkdirOldRoot ([bc37fbb](https://github.com/mitos-run/mitos/commit/bc37fbb7572b2e3dfa1071905c2f5c8061a3e0b8))
* **husk-stub:** remove stale per-vm jailer chroot before start so retries do not fail MkdirOldRoot ([b58340f](https://github.com/mitos-run/mitos/commit/b58340fecb008b716f31caaa4fdc4591e304422c))
* **husk:** self-heal a dead Firecracker instead of advertising a dead slot ([72fc826](https://github.com/mitos-run/mitos/commit/72fc8261f46b7da15eece49a14cd8b02ffe8996e))
* **kubectl-mitos:** register corev1 so exec can read the token Secret ([aff190f](https://github.com/mitos-run/mitos/commit/aff190f43180240eb83c480c59842ba529198633))
* **onboarding:** align E2EHandler gate numbering in comments with docstring ([f5f27fc](https://github.com/mitos-run/mitos/commit/f5f27fc68b5bf04359dcd73873e97f411c681961))
* Talos hardening batch ([#525](https://github.com/mitos-run/mitos/issues/525), [#526](https://github.com/mitos-run/mitos/issues/526), [#527](https://github.com/mitos-run/mitos/issues/527), [#528](https://github.com/mitos-run/mitos/issues/528)) ([25a2a81](https://github.com/mitos-run/mitos/commit/25a2a81dce575c708847e612b9183712917281c2))

## [1.6.0](https://github.com/mitos-run/mitos/compare/v1.5.0...v1.6.0) (2026-06-28)


### Features

* **chart:** hardened-runtime forkd profile (Talos seccomp + CAP_FOWNER) ([732a95f](https://github.com/mitos-run/mitos/commit/732a95fffd12c90878b500f8f92a4fdfe1572558))
* **cli:** add HostedBackend for sandbox verbs against the hosted gateway ([103ad67](https://github.com/mitos-run/mitos/commit/103ad678ca6ba7dc5d1a9c3458e8d6a456affef9))
* **cli:** hosted mode (API key + base URL) for sandbox create/exec/fork/ls/terminate ([794e950](https://github.com/mitos-run/mitos/commit/794e9501de09295d7f08f35cb107aa7f828a08b2))
* **computer-use:** headless Chromium + CDP browser-automation template ([#314](https://github.com/mitos-run/mitos/issues/314)) ([#510](https://github.com/mitos-run/mitos/issues/510)) ([cf5ec56](https://github.com/mitos-run/mitos/commit/cf5ec567b17efda6e2ec1cce2db70195ff801ee9))
* **console:** internal session-resolve endpoint for the front door ([32213a4](https://github.com/mitos-run/mitos/commit/32213a4b7bbba841258762fed98b408d9727a84a))
* **console:** mount pre-auth router (login/signup/verify) on unauthenticated state ([77ffc95](https://github.com/mitos-run/mitos/commit/77ffc957870199e470daf7f8cb1632ddbd757ec5))
* **console:** native Login page with GitHub/Google connector hints and email ([9351f0a](https://github.com/mitos-run/mitos/commit/9351f0ad60b5ce6f3ce1cac3848a3ec7f2d3c874))
* **console:** native Signup page wired to /onboarding/signup ([156c82e](https://github.com/mitos-run/mitos/commit/156c82ef8f4dc3ca6933aec513b12b72faefeaa0))
* **console:** native Verify page showing the first API key once ([36a66f1](https://github.com/mitos-run/mitos/commit/36a66f14ebf611db3ba8cbf8d5be0ab64090f6bc))
* **console:** set mitos_session cookie on fresh email verify ([106a7c0](https://github.com/mitos-run/mitos/commit/106a7c04a820a227d9e4b4cc099504ce4fb48c5a))
* **console:** use durable Postgres stores for credit, pending signups, sessions when DSN is set ([03fb243](https://github.com/mitos-run/mitos/commit/03fb243faf5ceb586cce8c30daa9b74cc43ce993))
* **controller:** map sandbox cpu to the husk limit with a low request for node overcommit ([6749205](https://github.com/mitos-run/mitos/commit/6749205fe879c004c9f9d381fab4735ce93f74a9))
* **controller:** map sandbox cpu to the husk limit with a low request for node overcommit ([10304f1](https://github.com/mitos-run/mitos/commit/10304f16a765f156bceb82170b96e82291648fda))
* **deploy:** Cilium edge gateway, front-door, and marketing for single-origin mitos.run (cluster-verified) ([28de02a](https://github.com/mitos-run/mitos/commit/28de02ad026e72732dd2622eac36fa54fa20bb72))
* **deploy:** Dex federation for GitHub and Google (cluster-verified) ([b8a97f8](https://github.com/mitos-run/mitos/commit/b8a97f8d53a707f29fbe94b1fad5cfd4f4a8be67))
* **facade:** migrate agents.x-k8s.io conformance to v0.5.0/v1beta1 and prove predicate-level KVM conformance ([#508](https://github.com/mitos-run/mitos/issues/508)) ([4414409](https://github.com/mitos-run/mitos/commit/44144090f1603d76317ca4931ce9d30d195eed15))
* **frontdoor:** auth+slug routing reverse proxy with session fork and header injection ([fe252e8](https://github.com/mitos-run/mitos/commit/fe252e836572868aeef4a6e7fa6c50bdd27fb91b))
* **frontdoor:** binary, HTTP session resolver, and image ([a0df52f](https://github.com/mitos-run/mitos/commit/a0df52f76684a16437f60122126149028bc89b91))
* **gateway:** single-tenant-namespace override for the control plane ([399bd85](https://github.com/mitos-run/mitos/commit/399bd85545f772f33ada2bb053d34dfc9d0d8741))
* **gateway:** single-tenant-namespace override for the control plane ([6bb2773](https://github.com/mitos-run/mitos/commit/6bb2773ce7d0d2a28845f406f9775d1a2be0833e))
* hosted launch spine (durable stores, native auth, front-door, Dex) ([7783645](https://github.com/mitos-run/mitos/commit/77836453f44b8efc4625bb9da2363cae8e627110))
* **oidcauth:** pass connector_id hint so GitHub/Google skip the Dex chooser ([a932f6e](https://github.com/mitos-run/mitos/commit/a932f6efddf8087997b2d7c13174bb793724f922))
* **onboarding:** add E2ETokenSink seam and MemE2ETokenSink ([62393e4](https://github.com/mitos-run/mitos/commit/62393e436c04fa956b7af7bff6e25f88b55ea1e3))
* **onboarding:** E2EHandler with bearer + domain + sink gates ([514686b](https://github.com/mitos-run/mitos/commit/514686b44009b102dc971bc559830a94a7e95154))
* **onboarding:** gated QA verify-token seam for hosted e2e ([55a8767](https://github.com/mitos-run/mitos/commit/55a8767a2880134e820be191f6ead3077acd3063))
* **onboarding:** wire E2E sink and mount gated endpoint in console ([3ed8b56](https://github.com/mitos-run/mitos/commit/3ed8b569ed377ff312f65aaeec300ea6f09605aa))
* **pgstore:** durable PgCreditLedger ([ac535de](https://github.com/mitos-run/mitos/commit/ac535de04cb0320ed33714513a1a6b3648cb8fa6))
* **pgstore:** durable PgPendingStore ([40af883](https://github.com/mitos-run/mitos/commit/40af8837c7b3828baf10dfeaefc6e8df15686f19))
* **pgstore:** migration 0002 for sessions, credit ledger, pending signups ([37f1224](https://github.com/mitos-run/mitos/commit/37f12242a9f7c3328bab9806320b956304384c4b))
* **saas:** Sessions interface and durable PgSessionStore ([2393ef4](https://github.com/mitos-run/mitos/commit/2393ef4debc3c68d2d3ca8e84e1fcca97435fa02))


### Bug Fixes

* accept mitos_ keys in hosted harness; env-configurable signup credit ([6f24445](https://github.com/mitos-run/mitos/commit/6f244450f09523a1b3feb68f9f4c66f8984651d7))
* accept mitos_ keys in hosted harness; env-configurable signup credit ([45bc82f](https://github.com/mitos-run/mitos/commit/45bc82f3edcacce3eaede930971f0295a48b41d2))
* **chart:** stop fresh installs referencing a phantom pull secret ([#399](https://github.com/mitos-run/mitos/issues/399)) ([#500](https://github.com/mitos-run/mitos/issues/500)) ([2dba7fe](https://github.com/mitos-run/mitos/commit/2dba7fe7b200d5f36d862325c2110fc992b91b0c))
* **console:** Login propagates next on email path, robust focus ring, a11y polish ([dd29de4](https://github.com/mitos-run/mitos/commit/dd29de4d753992d912d59d2ef6ce30033ac85b63))
* **console:** magenta keyboard-focus ring on auth inputs per brand ([e2088ad](https://github.com/mitos-run/mitos/commit/e2088ad4ea752308dd3cf1e54281017ae95f47d5))
* **console:** pre-auth router skips 401 retry and redirects unmatched paths to login ([a7406be](https://github.com/mitos-run/mitos/commit/a7406be952f1643adf7539f6a207ddd74e05360f))
* **console:** share one credit ledger between onboarding grant and billing view ([5ced231](https://github.com/mitos-run/mitos/commit/5ced231ed1954f1cd2c5862d23ad35af7269d8b8))
* **console:** Verify guards one-time POST, stops aria re-reading the key, handles clipboard failure ([fd6e79f](https://github.com/mitos-run/mitos/commit/fd6e79f7ad9481ab5b0cf047156aa9465424c384))
* **deploy:** Dex and console share one client-secret key so the console pod starts ([ee185ca](https://github.com/mitos-run/mitos/commit/ee185ca3cafa671dac710dd0c719bd83ed678962))
* **deploy:** edge HTTPS listener accepts www SNI (drop listener hostname pin) ([9448375](https://github.com/mitos-run/mitos/commit/9448375fdc97717b09df24a13ba1e6bdcb4789e6))
* **forkd:** keep jailer template links CoW across the chroot mount ([#526](https://github.com/mitos-run/mitos/issues/526)) ([d79bd9d](https://github.com/mitos-run/mitos/commit/d79bd9d875157fe5cdb7134f2545ac142897bcf3))
* **fork:** gate SIGUSR2 broadcast on a handler ([#467](https://github.com/mitos-run/mitos/issues/467)) and reap stale-digest husk pods ([#461](https://github.com/mitos-run/mitos/issues/461)) ([#509](https://github.com/mitos-run/mitos/issues/509)) ([fa5d6ad](https://github.com/mitos-run/mitos/commit/fa5d6ad37068842a3543489fc0384e9fbb7d160d))
* **frontdoor:** remove dead reserved param, normalize path in Decide, strengthen tests ([959001c](https://github.com/mitos-run/mitos/commit/959001c3c6bcdefa122227020631eefd901dd52a))
* **gateway:** handle POST/GET /v1/templates so hosted SDK create works ([e787c5e](https://github.com/mitos-run/mitos/commit/e787c5e6db3d2fcccae1ac808c38536bd8ea651f))
* **gateway:** handle POST/GET /v1/templates so hosted SDK create works ([b1af307](https://github.com/mitos-run/mitos/commit/b1af307ed6debdc162c887dfb78951c0cc0820f5))
* **gateway:** map POST /v1/fork to sandbox.create so hosted SDK create+fork works ([47a27f9](https://github.com/mitos-run/mitos/commit/47a27f991407213b2901a4967ad67a962ca4fac4))
* **gateway:** map POST /v1/fork to sandbox.create so hosted SDK create+fork works ([d9dd45d](https://github.com/mitos-run/mitos/commit/d9dd45dbdac6e3babef98621575999a05b5496c5))
* **husk-stub:** remove stale per-vm jailer chroot before start so retries do not fail MkdirOldRoot ([bc37fbb](https://github.com/mitos-run/mitos/commit/bc37fbb7572b2e3dfa1071905c2f5c8061a3e0b8))
* **husk-stub:** remove stale per-vm jailer chroot before start so retries do not fail MkdirOldRoot ([b58340f](https://github.com/mitos-run/mitos/commit/b58340fecb008b716f31caaa4fdc4591e304422c))
* **husk:** self-heal a dead Firecracker instead of advertising a dead slot ([72fc826](https://github.com/mitos-run/mitos/commit/72fc8261f46b7da15eece49a14cd8b02ffe8996e))
* **kubectl-mitos:** register corev1 so exec can read the token Secret ([aff190f](https://github.com/mitos-run/mitos/commit/aff190f43180240eb83c480c59842ba529198633))
* **onboarding:** align E2EHandler gate numbering in comments with docstring ([f5f27fc](https://github.com/mitos-run/mitos/commit/f5f27fc68b5bf04359dcd73873e97f411c681961))
* **pgstore:** open (run migrations) before truncating so fresh-DB CI passes ([02e38cd](https://github.com/mitos-run/mitos/commit/02e38cd87717b77c06c5b986b6dd0f8a3977b93c))
* **pgstore:** PgSessionStore lists sessions most-recent-first with UTC parity ([b01a32d](https://github.com/mitos-run/mitos/commit/b01a32d0d30f0ef810857f6cb983ca144146eda2))
* **pgstore:** PgSessionStore Revoke returns ErrNotFound and ListByAccount checks rows.Err for parity ([f8cc1c0](https://github.com/mitos-run/mitos/commit/f8cc1c02b626097ca19484d3fcb616078d0133ba))
* **security:** resolve CodeQL path-injection and harden the supply chain ([#503](https://github.com/mitos-run/mitos/issues/503)) ([17ac93b](https://github.com/mitos-run/mitos/commit/17ac93b5db4b96512882ec0b8b6706ce1af9a0f7))
* Talos hardening batch ([#525](https://github.com/mitos-run/mitos/issues/525), [#526](https://github.com/mitos-run/mitos/issues/526), [#527](https://github.com/mitos-run/mitos/issues/527), [#528](https://github.com/mitos-run/mitos/issues/528)) ([25a2a81](https://github.com/mitos-run/mitos/commit/25a2a81dce575c708847e612b9183712917281c2))

## [1.5.0](https://github.com/mitos-run/mitos/compare/v1.4.0...v1.5.0) (2026-06-27)


### Features

* **serving:** first-class serving workload started during build and surviving forks ([#460](https://github.com/mitos-run/mitos/issues/460)) ([#468](https://github.com/mitos-run/mitos/issues/468)) ([255da9b](https://github.com/mitos-run/mitos/commit/255da9bef2d3c97edae37aa7f9d9ae63184aeec4))


### Bug Fixes

* **build:** add the preview-proxy Dockerfile and publish it (expose proxy was never deployable) ([#457](https://github.com/mitos-run/mitos/issues/457)) ([a072fee](https://github.com/mitos-run/mitos/commit/a072fee4a7a965e057d4c5af64d9f5c5f8ba0549))
* **cas:** unpin deleted templates and drive periodic CAS GC ([#464](https://github.com/mitos-run/mitos/issues/464)) ([#470](https://github.com/mitos-run/mitos/issues/470)) ([30fa68a](https://github.com/mitos-run/mitos/commit/30fa68a96608cd0ce1a2e8e6503dc9995c5ba548))
* **chart:** sync the chart CRDs with the generated set (Sandbox expose, orgs) ([#454](https://github.com/mitos-run/mitos/issues/454)) ([f2ce17c](https://github.com/mitos-run/mitos/commit/f2ce17cb46fb7b86f19a15fa82b3c6e8ee3f00c1))
* **forkd:** journal and reap build-time orphans on ungraceful death ([#469](https://github.com/mitos-run/mitos/issues/469)) ([#471](https://github.com/mitos-run/mitos/issues/471)) ([86d542f](https://github.com/mitos-run/mitos/commit/86d542f746467bc0494ffe269f7ce8c7e30afe52))
* **forkd:** run forkd at system-node-critical so DiskPressure cannot evict it ([#466](https://github.com/mitos-run/mitos/issues/466)) ([fac5236](https://github.com/mitos-run/mitos/commit/fac5236c1f0936f7dd2cffcec6c810d91296b721))
* **husk:** make tap creation idempotent so warm-pod re-activation never EBUSYs ([#428](https://github.com/mitos-run/mitos/issues/428)) ([#458](https://github.com/mitos-run/mitos/issues/458)) ([a2674f7](https://github.com/mitos-run/mitos/commit/a2674f7a3416a422a5221070f91c1e270b2f5d90))
* **release:** resolve the publish tag from the latest release, not a flaky action output ([#452](https://github.com/mitos-run/mitos/issues/452)) ([ce394e2](https://github.com/mitos-run/mitos/commit/ce394e21d570d11b758e070b2bc1fe269754d0d7))
* **snapshot:** fsync template snapshot files before recording the digest ([#461](https://github.com/mitos-run/mitos/issues/461)) ([#462](https://github.com/mitos-run/mitos/issues/462)) ([c79b139](https://github.com/mitos-run/mitos/commit/c79b1394bf246dd727f33ee3e3a17361fe3321cc))

## [1.4.0](https://github.com/mitos-run/mitos/compare/v1.3.1...v1.4.0) (2026-06-26)


### Features

* **controller:** Run with Mitos auto-update reconciler [DRAFT, needs review] ([#340](https://github.com/mitos-run/mitos/issues/340)/[#440](https://github.com/mitos-run/mitos/issues/440)) ([#447](https://github.com/mitos-run/mitos/issues/447)) ([753c87a](https://github.com/mitos-run/mitos/commit/753c87a01694518bbc23ccf1bf9a6cf1686926f7))
* **kubectl-mitos:** label guest ps with claim/pool/workspace from the Sandbox CRD ([#164](https://github.com/mitos-run/mitos/issues/164)) ([#448](https://github.com/mitos-run/mitos/issues/448)) ([cd91763](https://github.com/mitos-run/mitos/commit/cd91763a03e1b38bf0b474e1bb7960f23f706250))
* **runmanifest:** mitos.yaml -&gt; golden pool + per-fork provisioner (Run with Mitos, slices 1-2) ([#442](https://github.com/mitos-run/mitos/issues/442)) ([c8d1fef](https://github.com/mitos-run/mitos/commit/c8d1fef88688dcbebe71b9098dfa4ff5c22e4fa6))
* **runservice:** click-to-provision run service + HTTP endpoints (Run with Mitos, slice 4) ([#443](https://github.com/mitos-run/mitos/issues/443)) ([ead7f99](https://github.com/mitos-run/mitos/commit/ead7f99a5b5475a0a7ba0664cb08d79d18c85b45))
* **sdk:** fork(n) ergonomics across the TypeScript SDK and CLI ([#311](https://github.com/mitos-run/mitos/issues/311)) ([#444](https://github.com/mitos-run/mitos/issues/444)) ([6b35073](https://github.com/mitos-run/mitos/commit/6b3507393f019543aeb5c4793a9bf163e1efd2ad))


### Bug Fixes

* **chart:** default console.replicas to 1 so OIDC login works ([#427](https://github.com/mitos-run/mitos/issues/427)) ([#438](https://github.com/mitos-run/mitos/issues/438)) ([b21a15d](https://github.com/mitos-run/mitos/commit/b21a15df5cf254069551e3b4a39522ebf66d17c0))
* **controller:** issue the husk TLS leaf in the controller namespace too ([#414](https://github.com/mitos-run/mitos/issues/414)) ([#437](https://github.com/mitos-run/mitos/issues/437)) ([3667975](https://github.com/mitos-run/mitos/commit/36679759d32271c4213ce0fdb2b4e10417413f5a))
* **forkd:** grant CAP_DAC_OVERRIDE so the builder can rebuild its own rootfs ([#426](https://github.com/mitos-run/mitos/issues/426)) ([#435](https://github.com/mitos-run/mitos/issues/435)) ([9070360](https://github.com/mitos-run/mitos/commit/9070360565c4b3c446e460e981ce0c855fae15a8))
* **forkd:** ship the jailer binary in the forkd and husk-stub images ([#425](https://github.com/mitos-run/mitos/issues/425)) ([#433](https://github.com/mitos-run/mitos/issues/433)) ([a6177bb](https://github.com/mitos-run/mitos/commit/a6177bbe7e66f7262c983f9ec08e7c7ab16f7349))
* **ociroot:** tolerate unreadable subtrees when measuring rootfs size ([#415](https://github.com/mitos-run/mitos/issues/415)) ([#436](https://github.com/mitos-run/mitos/issues/436)) ([9f2f7ef](https://github.com/mitos-run/mitos/commit/9f2f7efa28abf3266b033d3cdf8cfa445c325550))
* **release:** auto-publish signed images on release and track the version in the chart ([#450](https://github.com/mitos-run/mitos/issues/450)) ([196928c](https://github.com/mitos-run/mitos/commit/196928ceaf33127c022702407346a3e8e1345c50))

## [1.3.1](https://github.com/mitos-run/mitos/compare/v1.3.0...v1.3.1) (2026-06-26)


### Bug Fixes

* **console:** request email/profile scope so OIDC login is not always rejected ([#431](https://github.com/mitos-run/mitos/issues/431)) ([acd9bc9](https://github.com/mitos-run/mitos/commit/acd9bc9e602bdf07fce237a0d1359326e77d3007)), closes [#430](https://github.com/mitos-run/mitos/issues/430)
* **husk:** tear down the tap on partial egress-filter failure ([#428](https://github.com/mitos-run/mitos/issues/428)) ([#429](https://github.com/mitos-run/mitos/issues/429)) ([10c4ce2](https://github.com/mitos-run/mitos/commit/10c4ce233a8249332bb854bd0ebf250def6b7aee))

## [1.3.0](https://github.com/mitos-run/mitos/compare/v1.2.0...v1.3.0) (2026-06-26)


### Features

* **console:** B3d-2 per-project membership ([#406](https://github.com/mitos-run/mitos/issues/406)) ([4c9c521](https://github.com/mitos-run/mitos/commit/4c9c521f610746efec4764b891a58bf571140be3))
* **console:** B3d-3 resource project tagging (sandboxes) ([#409](https://github.com/mitos-run/mitos/issues/409)) ([1542930](https://github.com/mitos-run/mitos/commit/15429303e199138d1d9337e78969aea81306a5fb))
* **console:** B3d-4 per-project access enforcement on sandboxes ([#411](https://github.com/mitos-run/mitos/issues/411)) ([0994470](https://github.com/mitos-run/mitos/commit/0994470c9589579451eda65e9d931e5a08bc9fd7))
* **controller:** per-org namespace tenancy provisioner ([#288](https://github.com/mitos-run/mitos/issues/288)) ([#410](https://github.com/mitos-run/mitos/issues/410)) ([ba36785](https://github.com/mitos-run/mitos/commit/ba36785b6f4ff4c6d4e81b25cfafee0c0ba7bee7))
* **expose:** mitos workspace serve + Go SDK .url handle (slice 5a, [#312](https://github.com/mitos-run/mitos/issues/312)) ([#416](https://github.com/mitos-run/mitos/issues/416)) ([ee34eab](https://github.com/mitos-run/mitos/commit/ee34eabca547cd41aac6d564854f175ab865f8c5))
* **expose:** the auth ladder with native OIDC (slice 4) ([#407](https://github.com/mitos-run/mitos/issues/407)) ([3d4bc14](https://github.com/mitos-run/mitos/commit/3d4bc142e3e56bed02f449dd7809157a0de3efe4))
* **saas:** durable Postgres persistence + best-practice external-DB chart ([#412](https://github.com/mitos-run/mitos/issues/412)) ([74d0c18](https://github.com/mitos-run/mitos/commit/74d0c185f1c7aca6ca34d88340a108d752624c60))
* **saas:** enforce real quota + abuse + kill-switch at the hosted gateway ([#341](https://github.com/mitos-run/mitos/issues/341)) ([#421](https://github.com/mitos-run/mitos/issues/421)) ([ca462ed](https://github.com/mitos-run/mitos/commit/ca462ed9fa1c15e2da91cc4c0b852197982410bd))
* **saas:** mount onboarding HTTP + real SMTP email + tenant provisioning on signup ([#215](https://github.com/mitos-run/mitos/issues/215)) ([#420](https://github.com/mitos-run/mitos/issues/420)) ([ed9a143](https://github.com/mitos-run/mitos/commit/ed9a1430e5d38bcbd68b8980805425cea0766a08))
* **saas:** privacy-first product telemetry pipeline ([#281](https://github.com/mitos-run/mitos/issues/281)) ([#422](https://github.com/mitos-run/mitos/issues/422)) ([79c6f18](https://github.com/mitos-run/mitos/commit/79c6f18d2f86a28eb3f309bc3f0b7516407a2ed7))
* **saas:** real hosted control plane (gateway creates + proxies real sandboxes) ([#405](https://github.com/mitos-run/mitos/issues/405)) ([3e15ab4](https://github.com/mitos-run/mitos/commit/3e15ab45e074e38c225e458398f3ba840133b81a))
* **saas:** real Paddle billing provider ([#212](https://github.com/mitos-run/mitos/issues/212)) ([#418](https://github.com/mitos-run/mitos/issues/418)) ([1c673f2](https://github.com/mitos-run/mitos/commit/1c673f2e0be001954cee86a03781fec97f94aa19))
* **saas:** wire console usage instruments + fork-tree to real cluster data ([#417](https://github.com/mitos-run/mitos/issues/417)) ([34d800d](https://github.com/mitos-run/mitos/commit/34d800dd3594c6bd2511da5111c1db48e72c90a9))
* **sdk:** workspace serve .url parity across Python, TypeScript, Ruby, Rust, Java (slice 5b, [#312](https://github.com/mitos-run/mitos/issues/312)) ([#419](https://github.com/mitos-run/mitos/issues/419)) ([ffa2ff5](https://github.com/mitos-run/mitos/commit/ffa2ff5a556659e5c61fce166e8e85816f15953b))


### Bug Fixes

* **daemon:** per-sandbox expose concurrency cap and force-close on terminate ([#413](https://github.com/mitos-run/mitos/issues/413)) ([17d2626](https://github.com/mitos-run/mitos/commit/17d2626978054eaee01d7bb67212cffbf64fc839))

## [1.2.0](https://github.com/mitos-run/mitos/compare/v1.1.0...v1.2.0) (2026-06-25)


### Features

* **console:** B3d-1 per-verb permission enforcement and custom roles [SECURITY REVIEW REQUIRED] ([#395](https://github.com/mitos-run/mitos/issues/395)) ([c949f55](https://github.com/mitos-run/mitos/commit/c949f55f1f2374501dbeaab77186fd78bad5297a))
* **console:** Phase B2a Sandboxes (list + detail tabs + fork-tree deep-link) ([#361](https://github.com/mitos-run/mitos/issues/361)) ([b1167ed](https://github.com/mitos-run/mitos/commit/b1167ed87dd8c7e7726ef08fc468ab3ce9e1c0b3))
* **console:** Phase B2b views (keys, secrets, usage, audit, templates, billing) ([#363](https://github.com/mitos-run/mitos/issues/363)) ([d1cd113](https://github.com/mitos-run/mitos/commit/d1cd1133f689b19b724812f0562152dd4d991a68))
* **console:** Phase B2c best-practice roles + role management + Projects ([#365](https://github.com/mitos-run/mitos/issues/365)) ([8985664](https://github.com/mitos-run/mitos/commit/89856643c7f0d37c0ce482bf1e08f25313f8b1cf))
* **console:** Phase B2d account settings (profile, sessions, appearance) ([#368](https://github.com/mitos-run/mitos/issues/368)) ([16e7f3d](https://github.com/mitos-run/mitos/commit/16e7f3d64ab9d2817dd1a9953da6fd7787a14143))
* **console:** Phase B3a Trust and compliance surface ([#370](https://github.com/mitos-run/mitos/issues/370)) ([2631ad7](https://github.com/mitos-run/mitos/commit/2631ad7da2d3e608671df09b5dfb6b22de282b3c))
* **console:** Phase B3b audit retention, NDJSON export, and SIEM sinks ([#375](https://github.com/mitos-run/mitos/issues/375)) ([c37a03f](https://github.com/mitos-run/mitos/commit/c37a03f839c6fe346e33b8b5502c7341b89e46e4))
* **console:** Phase B3c data-retention policies and legal hold ([#378](https://github.com/mitos-run/mitos/issues/378)) ([9563eb4](https://github.com/mitos-run/mitos/commit/9563eb4af5d274cd6336e9f3d779a6a9a01feb74))
* **console:** shell part 2 (nav regroup, operational Overview home) ([#387](https://github.com/mitos-run/mitos/issues/387)) ([ac83f7e](https://github.com/mitos-run/mitos/commit/ac83f7eb73141fd65241d53ba235474f34837ac2))
* **console:** shell part 3 (table search toolbar on list views) ([#389](https://github.com/mitos-run/mitos/issues/389)) ([c20db2d](https://github.com/mitos-run/mitos/commit/c20db2d5a5a2fef8530656b93c89385098a8017c))
* **console:** shell upgrade for website continuity (brand, global top bar, page headers) ([#386](https://github.com/mitos-run/mitos/issues/386)) ([ed77355](https://github.com/mitos-run/mitos/commit/ed77355180cd9d5f97a91a3198de3f3e11e9b4eb))
* **controller,tenant,usage:** stamp mitos.run/org from the trusted namespace + live OrgResolver ([#164](https://github.com/mitos-run/mitos/issues/164)) ([4747d97](https://github.com/mitos-run/mitos/commit/4747d97aeb6f0475ef7d25414f84c8bb2c53b096))
* **daemon:** audit + exec timeout ceiling on the Connect runtime path ([#358](https://github.com/mitos-run/mitos/issues/358), [#216](https://github.com/mitos-run/mitos/issues/216)) ([#388](https://github.com/mitos-run/mitos/issues/388)) ([7e45ec0](https://github.com/mitos-run/mitos/commit/7e45ec0f735b814a748d754c642e0f46d400075c))
* **daemon:** bidi Exec (PTY) over a Connect WebSocket transport ([#358](https://github.com/mitos-run/mitos/issues/358) Task 1) ([#379](https://github.com/mitos-run/mitos/issues/379)) ([3c6227d](https://github.com/mitos-run/mitos/commit/3c6227d790c23d7acca74de74a29639a43b415ff))
* **daemon:** deprecate the legacy /v1 runtime endpoints in favor of Connect ([#24](https://github.com/mitos-run/mitos/issues/24)) ([de0a161](https://github.com/mitos-run/mitos/commit/de0a161d6546677311cec7bda18d86a713a25f24))
* **expose:** authenticated SSE-safe guest HTTP proxy (Mitos Expose slice 1) ([#384](https://github.com/mitos-run/mitos/issues/384)) ([175f4e0](https://github.com/mitos-run/mitos/commit/175f4e03fbfdb03a8aab571c9fb793258e321dc0))
* **expose:** controller route-sync reconciler (slice 2b) ([#394](https://github.com/mitos-run/mitos/issues/394)) ([855bedc](https://github.com/mitos-run/mitos/commit/855bedc831bd423abf3cf4ba4aa2da3be8451055))
* **expose:** edge proxy single-label subdomains and forkd backend (slice 2a) ([#392](https://github.com/mitos-run/mitos/issues/392)) ([09f117e](https://github.com/mitos-run/mitos/commit/09f117e9997f5da60d9e360224948a091f3f2609))
* **expose:** wildcard cert, post-quantum TLS guardrail, and deploy the proxy (slice 3) ([#396](https://github.com/mitos-run/mitos/issues/396)) ([9f5856c](https://github.com/mitos-run/mitos/commit/9f5856c0118a7a1437130a47daa314f912e82026))
* **firecracker:** assert and fail closed on VMM seccomp enforcement ([#353](https://github.com/mitos-run/mitos/issues/353)) ([df139b1](https://github.com/mitos-run/mitos/commit/df139b112ecb4810f33384560e51896c8a52be0c))
* **forkd:** drop privileged, run the builder under the explicit jailer capability set ([#352](https://github.com/mitos-run/mitos/issues/352)) ([915eb59](https://github.com/mitos-run/mitos/commit/915eb5938757c91128c6c2bf4db33e5439b78b32))
* **kubectl-mitos:** run exec and guest vitals over Connect, retire /v1 ([#358](https://github.com/mitos-run/mitos/issues/358)) ([#385](https://github.com/mitos-run/mitos/issues/385)) ([dd5d3d6](https://github.com/mitos-run/mitos/commit/dd5d3d690a0d6c28ff7d45ecac82046527b74da9))
* **mcp:** run the mitos-mcp backend exec/files over Connect ([#358](https://github.com/mitos-run/mitos/issues/358)) ([#393](https://github.com/mitos-run/mitos/issues/393)) ([4a7bcc3](https://github.com/mitos-run/mitos/commit/4a7bcc38a42ca76fbd1307d027c862a8a0bb3362))
* **observability:** guest Vitals + metering metrics + fork trace tail ([#164](https://github.com/mitos-run/mitos/issues/164) Phase 1) ([#377](https://github.com/mitos-run/mitos/issues/377)) ([4888df1](https://github.com/mitos-run/mitos/commit/4888df1934041734f25a3662005d0ce7c331587b))
* **sdk/go:** run exec over the Connect runtime protocol, add streaming ExecStream ([#358](https://github.com/mitos-run/mitos/issues/358)) ([2e2ea1f](https://github.com/mitos-run/mitos/commit/2e2ea1f8b81d029b882907b7f061ade44c005e3a))
* **sdk/python:** run PTY over the Connect WebSocket transport, retire /v1/pty ([#358](https://github.com/mitos-run/mitos/issues/358)) ([#380](https://github.com/mitos-run/mitos/issues/380)) ([b5a2e9e](https://github.com/mitos-run/mitos/commit/b5a2e9ec395c52f513719ac2d8527da3574d8f09))
* **sdk/typescript:** run PTY over the Connect WebSocket transport, retire /v1/pty ([#358](https://github.com/mitos-run/mitos/issues/358)) ([#381](https://github.com/mitos-run/mitos/issues/381)) ([684342e](https://github.com/mitos-run/mitos/commit/684342e5fcd7a0f5240b6b4b1d0c7ef504d3c3b0))
* **sdk:** migrate Ruby, Java, Rust exec to the Connect runtime protocol ([#358](https://github.com/mitos-run/mitos/issues/358)) ([6a1541f](https://github.com/mitos-run/mitos/commit/6a1541f76a843bab69f61307a98de1dc8c2a399e))
* **sdk:** move the Python and TypeScript runtime surface onto Connect ([#358](https://github.com/mitos-run/mitos/issues/358)) ([#376](https://github.com/mitos-run/mitos/issues/376)) ([1a30451](https://github.com/mitos-run/mitos/commit/1a3045186c126d5b3d75b0591091556e0e86b60e))
* **usage:** live metering scraper + org attribution spine ([#164](https://github.com/mitos-run/mitos/issues/164)) ([fb8603f](https://github.com/mitos-run/mitos/commit/fb8603f8ed31ade8d5292ca5acf02bf67054e9ac))
* **usage:** live multi-node metering scraper + collector loop + per-org usage metric ([#164](https://github.com/mitos-run/mitos/issues/164)) ([0565f42](https://github.com/mitos-run/mitos/commit/0565f42a1bd1d73b16b5a40fc692deee529a9f5d))


### Bug Fixes

* **chart:** emit console OIDC redirect URL and add console.extraEnv ([#403](https://github.com/mitos-run/mitos/issues/403)) ([bd6cb64](https://github.com/mitos-run/mitos/commit/bd6cb64aa68ec0be0211be11606461df7350d3c4)), closes [#398](https://github.com/mitos-run/mitos/issues/398)
* **ci:** boot a microVM before asserting VMM seccomp ([#353](https://github.com/mitos-run/mitos/issues/353)) ([f20fb42](https://github.com/mitos-run/mitos/commit/f20fb42a8f97ca0cb8e192ddbedbaa751d5c3549))
* **usage:** store-fed cumulative usage metric + bounded record store + scrape timeout ([#164](https://github.com/mitos-run/mitos/issues/164)) ([e91f9ae](https://github.com/mitos-run/mitos/commit/e91f9aee10de099e51ec8780cd71e113f22b4f9a))

## [1.1.0](https://github.com/mitos-run/mitos/compare/v1.0.0...v1.1.0) (2026-06-25)

All six SDKs (Python, TypeScript, Go, Ruby, Rust, Java) now have Kubernetes cluster mode (an AgentRun that drives the mitos.run/v1 CRDs through the Kubernetes API), with byte-for-byte identical default-pool naming. The Go, Ruby, Rust, and Java SDKs add a thin Kubernetes REST client built on each language standard library (no heavy client dependency), so direct mode stays unchanged and the dependency-free SDKs stay dependency-free.



### Features

* **console:** Phase B1 hero views (instrument cockpit + live fork tree) ([#348](https://github.com/mitos-run/mitos/issues/348)) ([602a81b](https://github.com/mitos-run/mitos/commit/602a81ba1a9324cde1dca5920cd01eb9cfa13a02))
* **sdk-go:** Kubernetes cluster mode (AgentRun) ([#303](https://github.com/mitos-run/mitos/issues/303)) ([39ef093](https://github.com/mitos-run/mitos/commit/39ef09324da03af768eed44995061a64df23ff82))
* **sdk-go:** Kubernetes cluster mode (AgentRun) ([#303](https://github.com/mitos-run/mitos/issues/303)) ([fdae520](https://github.com/mitos-run/mitos/commit/fdae520673dcfcc29847f48ef97f90ae89397cc7))
* **sdk-java:** Kubernetes cluster mode (AgentRun) ([#306](https://github.com/mitos-run/mitos/issues/306)) ([5b36419](https://github.com/mitos-run/mitos/commit/5b364199e8022da176175cc27bd326a4bb20b009))
* **sdk-java:** Kubernetes cluster mode (AgentRun) ([#306](https://github.com/mitos-run/mitos/issues/306)) ([52fd768](https://github.com/mitos-run/mitos/commit/52fd7680954ba6fcd92284f98a88749549727592))
* **sdk-ruby:** Kubernetes cluster mode (AgentRun) ([#304](https://github.com/mitos-run/mitos/issues/304)) ([4a121ab](https://github.com/mitos-run/mitos/commit/4a121ab7600ef55dac5df7fb7b19e80e8a91cb3b))
* **sdk-ruby:** Kubernetes cluster mode (AgentRun) ([#304](https://github.com/mitos-run/mitos/issues/304)) ([114ee1d](https://github.com/mitos-run/mitos/commit/114ee1d00e6dc96d47209dd9d1a6fe80806e7e42))
* **sdk-rust:** Kubernetes cluster mode (AgentRun) ([#305](https://github.com/mitos-run/mitos/issues/305)) ([12cfe82](https://github.com/mitos-run/mitos/commit/12cfe829c843e7dae90225defb5a3ea6dd3cf8bc))
* **sdk-rust:** Kubernetes cluster mode (AgentRun) ([#305](https://github.com/mitos-run/mitos/issues/305)) ([4682f7d](https://github.com/mitos-run/mitos/commit/4682f7d7880b1793be2b0085626bc3c0ad3c64da))

## [1.0.0](https://github.com/mitos-run/mitos/compare/v0.14.0...v1.0.0) (2026-06-24)

The Rust guest agent is now the sole guest agent. SP1.5 migrated every host caller to the gRPC contract (AgentGRPCPort 53); the legacy JSON vsock protocol and the Go agent are removed. Measured on bare metal (both agents, same gRPC contract, only /init differs): about 17 to 19 percent faster fork-to-first-response, about 4.7x smaller static binary, with round-trip latency and per-VM RSS within noise and no regression.



### Features

* **agent-rs:** add error.rs and env.rs shared primitives ([#310](https://github.com/mitos-run/mitos/issues/310)) ([8022bd8](https://github.com/mitos-run/mitos/commit/8022bd82e9f72bb011962495d406376cefebf867))
* **agent-rs:** add sys/ module: AF_VSOCK, RNDADDENTROPY, clock_settime wrappers ([#310](https://github.com/mitos-run/mitos/issues/310)) ([10a0340](https://github.com/mitos-run/mitos/commit/10a03408a87b6ee9e6767cea084b0a5ffcec4355))
* **agent-rs:** Archive and Upload RPC implementations ([#310](https://github.com/mitos-run/mitos/issues/310)) ([48499ca](https://github.com/mitos-run/mitos/commit/48499ca053f33229283a64cd2ceef29adcefa425))
* **agent-rs:** clippy -D warnings clean, musl size gate, 3 hygiene fixes ([#310](https://github.com/mitos-run/mitos/issues/310)) ([244569e](https://github.com/mitos-run/mitos/commit/244569eb893b86b038f76d0dd20a818085168132))
* **agent-rs:** Exec and PTY RPC implementation ([#310](https://github.com/mitos-run/mitos/issues/310)) ([945d90d](https://github.com/mitos-run/mitos/commit/945d90d74c377247778db69dd7e409732141a281))
* **agent-rs:** fork/clock - CLOCK_REALTIME step with 500ms threshold ([#310](https://github.com/mitos-run/mitos/issues/310)) ([4c9c5b8](https://github.com/mitos-run/mitos/commit/4c9c5b8e3f2828ffc83228a2588d4a30d8c9c6cd))
* **agent-rs:** fork/mod - handle_notify_forked orchestrator ([#310](https://github.com/mitos-run/mitos/issues/310)) ([4dc89e5](https://github.com/mitos-run/mitos/commit/4dc89e57eaf481e7f597a4cdc54f897f34f5eecc))
* **agent-rs:** fork/network - eth0 reconfiguration via raw netlink ([#310](https://github.com/mitos-run/mitos/issues/310)) ([b17cadd](https://github.com/mitos-run/mitos/commit/b17cadd6f24c6c390c64bad40ceb9820d2bc22ca))
* **agent-rs:** fork/reseed - credited CRNG reseed, fail-closed ([#310](https://github.com/mitos-run/mitos/issues/310)) ([7cb6ec7](https://github.com/mitos-run/mitos/commit/7cb6ec756b36d726f135e29ebeaf9a3e43410885))
* **agent-rs:** fork/volumes - per-fork volume mounts ([#310](https://github.com/mitos-run/mitos/issues/310)) ([939174b](https://github.com/mitos-run/mitos/commit/939174b8321e60f9920225d652f08155b00ee65c))
* **agent-rs:** implement ExecStream gRPC RPC (production /init parity) ([#310](https://github.com/mitos-run/mitos/issues/310)) ([d64b1a8](https://github.com/mitos-run/mitos/commit/d64b1a8f964b683341f2e73ed80e98caad2444e3))
* **agent-rs:** init/ module and tonic gRPC skeleton over vsock ([#310](https://github.com/mitos-run/mitos/issues/310)) ([bcdb3a8](https://github.com/mitos-run/mitos/commit/bcdb3a8369d6bc13c42e41b74fc439dc89e6ea2c))
* **agent-rs:** PortForward bidirectional TCP splice RPC ([#310](https://github.com/mitos-run/mitos/issues/310)) ([569ebae](https://github.com/mitos-run/mitos/commit/569ebae8ff0d1d63b3a1e6bc9afb3e71f379e58d))
* **agent-rs:** production Rust guest agent, full sandbox.v1 gRPC parity (WIP, [#310](https://github.com/mitos-run/mitos/issues/310)) ([cb36e5c](https://github.com/mitos-run/mitos/commit/cb36e5c9c6b09bd6cc00fe08b39db8fcdc7e6321))
* **agent-rs:** ReadFile/WriteFile/List/Stat/Mkdir/Remove RPC implementations ([#310](https://github.com/mitos-run/mitos/issues/310)) ([b65d219](https://github.com/mitos-run/mitos/commit/b65d219b355478cb4b08c0d78ef2e8363d8272b9))
* **agent-rs:** replace spike scaffolding with gRPC production toolchain ([#310](https://github.com/mitos-run/mitos/issues/310)) ([1ecd3cd](https://github.com/mitos-run/mitos/commit/1ecd3cd63f4862d504a4e3c5268e994b5ed7b7b3))
* **agent-rs:** RunCode RPC + KernelManager driver subprocess ([#310](https://github.com/mitos-run/mitos/issues/310)) ([d9f907d](https://github.com/mitos-run/mitos/commit/d9f907d106951085a0cb40e9b87151645ff9287e))
* **agent-rs:** Vitals streaming RPC with /proc sampler ([#310](https://github.com/mitos-run/mitos/issues/310)) ([f14466b](https://github.com/mitos-run/mitos/commit/f14466b7dcd1e9fa203c7ff1b5e82c882367cd36))
* **agent-rs:** Watch RPC with inotify event streaming ([#310](https://github.com/mitos-run/mitos/issues/310)) ([6b0477d](https://github.com/mitos-run/mitos/commit/6b0477dead46b01ce1597e408c10736595b655a8))
* **agent-rs:** wire NotifyForked control service and final main ([#310](https://github.com/mitos-run/mitos/issues/310)) ([ab13eb0](https://github.com/mitos-run/mitos/commit/ab13eb0f61757e5e5ee59bbf3e7fad427c499e6a))
* **api:** consolidate to stable mitos.run/v1 (three nouns), remove v1alpha1 ([#299](https://github.com/mitos-run/mitos/issues/299)) ([30a0484](https://github.com/mitos-run/mitos/commit/30a0484461b62c0758f81cb08ea140c494d97d6a))
* Connect runtime protocol (gRPC over vsock) + Python SDK ([#24](https://github.com/mitos-run/mitos/issues/24)) ([f1f04e4](https://github.com/mitos-run/mitos/commit/f1f04e44c0dccba84439e38832cf39c18faea326))
* **console:** Phase B0 dashboard shell (responsive, accessible) + console design spec ([#332](https://github.com/mitos-run/mitos/issues/332)) ([2111842](https://github.com/mitos-run/mitos/commit/2111842e85f1c62add9d1b3ecf8ca4ebb32c513d))
* **daemon:** real vsockGuestConn over gRPC for Connect service ([#24](https://github.com/mitos-run/mitos/issues/24) stage 5) ([c071e08](https://github.com/mitos-run/mitos/commit/c071e083ebdee0f25a9e6685dd1cbc0acc597dcc))
* **guest-agent-rs:** implement Processes and Signal RPCs (task 2.5, [#310](https://github.com/mitos-run/mitos/issues/310)) ([292decb](https://github.com/mitos-run/mitos/commit/292decb69b532ce9ddc83625ac082a7c3544f3b3))
* **guestgrpc:** host-side gRPC guest client over vsock with ready-retry ([#310](https://github.com/mitos-run/mitos/issues/310)) ([f8f5c05](https://github.com/mitos-run/mitos/commit/f8f5c05c2bc51e48445b5e569c5b04be48da9ae9))
* **guest:** implement ExecStream and RunCodeStream gRPC RPCs in the Go agent ([#310](https://github.com/mitos-run/mitos/issues/310)) ([f3d5e37](https://github.com/mitos-run/mitos/commit/f3d5e37e3b7c00cd170b588be819a9056eab163d))
* **guest:** implement reuse-able Sandbox gRPC RPCs in the guest agent ([f3112d6](https://github.com/mitos-run/mitos/commit/f3112d6365399efa03256030ca5d2590d6a40bf2))
* **guest:** implement Watch, Processes, Signal gRPC RPCs (Task 5.1c) ([a278452](https://github.com/mitos-run/mitos/commit/a278452e3a167f6744976de06b6c18b06284e5c0))
* **guest:** serve gRPC Exec and Control over vsock alongside JSON loop ([80a297c](https://github.com/mitos-run/mitos/commit/80a297c6119c14dcb4d2a00ee7a3ff93ffe12a40))
* **host:** migrate guest callers from JSON to gRPC (SP1.5, [#310](https://github.com/mitos-run/mitos/issues/310)) ([f8bbcfd](https://github.com/mitos-run/mitos/commit/f8bbcfdb21da4c512e09cf8f1b65f3dcf491f581))
* **proto:** add RunCode, Mkdir, Remove, Upload to sandbox.v1 ([e873234](https://github.com/mitos-run/mitos/commit/e8732349d9df6d2f7206c5dc67d88d92a4be6cdf))
* **proto:** internal control service (NotifyForked, Configure, Ping) ([c240d73](https://github.com/mitos-run/mitos/commit/c240d73f3a1736bd3826f7528cf033527984edc9))
* **rootfs,ci:** make the Rust guest agent the production default; firecracker-test validates it ([#310](https://github.com/mitos-run/mitos/issues/310)) ([58b6f9b](https://github.com/mitos-run/mitos/commit/58b6f9b78b9a43636cba86040dc07e0d720844ab))
* **rootfs,ci:** make the Rust guest agent the production default; firecracker-test validates it ([#310](https://github.com/mitos-run/mitos/issues/310)) ([de66b03](https://github.com/mitos-run/mitos/commit/de66b034d6752fe40f9b46acf0fed10f791ffada))
* **sandbox:** add ExecStream and RunCodeStream server-streaming RPCs ([#24](https://github.com/mitos-run/mitos/issues/24)) ([5530c26](https://github.com/mitos-run/mitos/commit/5530c26a4abad51be31c1e0369fc013928fe1736))
* **sandboxrpc:** file RPCs (ReadFile, WriteFile, List, Stat, Mkdir, Remove) ([40e1af2](https://github.com/mitos-run/mitos/commit/40e1af28d20261c9549fb76bd04bfbed540b9fdc))
* **sandboxrpc:** GuestConn port and Service.Guest field; Exec streams via a fake guest ([5664ccc](https://github.com/mitos-run/mitos/commit/5664ccc7a342ef21966c49d413311a655b909d80))
* **sandboxrpc:** implement Archive (download) and Upload (untar) RPCs ([e30f26f](https://github.com/mitos-run/mitos/commit/e30f26fad98b046b666c6cc923cc5d56edf7c1d4))
* **sandboxrpc:** implement PortForward, Vitals, Watch, Processes, Signal RPCs ([b278610](https://github.com/mitos-run/mitos/commit/b27861037639a93cf71b7c78a8f9cab60bd049a7))
* **sandboxrpc:** implement RunCode RPC (Task 2.3) ([9fb4037](https://github.com/mitos-run/mitos/commit/9fb4037b37f71161650d808b6c766812d18c4109))
* **sandboxrpc:** mount Connect handler on :9091 with bearer-token gate and GuestConn exec bridge (Task 3.2, issue [#24](https://github.com/mitos-run/mitos/issues/24)) ([200cd91](https://github.com/mitos-run/mitos/commit/200cd91b06bbfa5e2a239265d9ff9048f423555b))
* **sandboxrpc:** per-sandbox bearer token interceptor, fail-closed ([ed4e35b](https://github.com/mitos-run/mitos/commit/ed4e35b196edc4a97e7183aedce331027dba74b7))
* **sdk:** migrate direct-mode file transport to the Connect Sandbox service ([#24](https://github.com/mitos-run/mitos/issues/24)) ([887a617](https://github.com/mitos-run/mitos/commit/887a617efefaa03fd423d83dac01590afe4ca8f0))
* **sdk:** ride exec and run_code on the Connect ExecStream/RunCodeStream RPCs ([#24](https://github.com/mitos-run/mitos/issues/24)) ([9f36f03](https://github.com/mitos-run/mitos/commit/9f36f03b2651ca3ae8a090d8d4158f9704911227))
* **vsock:** gRPC-over-net.Conn dialer spike (task 4.1) ([c9cb54a](https://github.com/mitos-run/mitos/commit/c9cb54aa83192662834b9af69ab0576765820c0d))


### Bug Fixes

* **agent-rs:** address review findings in per-fork netlink reconfiguration ([#310](https://github.com/mitos-run/mitos/issues/310)) ([548e4cc](https://github.com/mitos-run/mitos/commit/548e4ccf41f1abd2be4d9c4f52c48947a95ff2c7))
* **agent-rs:** address SP1 1.2 review findings in sys/ safety-critical module ([#310](https://github.com/mitos-run/mitos/issues/310)) ([c4e413f](https://github.com/mitos-run/mitos/commit/c4e413fdaf5ff482037a708b3d0db7679b61a45d))
* **agent-rs:** address SP1 review findings in error.rs and env.rs ([#310](https://github.com/mitos-run/mitos/issues/310)) ([ab46ce8](https://github.com/mitos-run/mitos/commit/ab46ce8e185465c55f0314f5cc879065073ee956))
* **agent-rs:** eliminate global WORKSPACE_ROOT, thread root through SandboxService ([#310](https://github.com/mitos-run/mitos/issues/310)) ([79a02a4](https://github.com/mitos-run/mitos/commit/79a02a48a08474d2c78ae142d12f2b42a114b9b9))
* **agent-rs:** harden KernelManager line cap, graceful JSON error, RunCode gRPC conformance tests ([#310](https://github.com/mitos-run/mitos/issues/310)) ([0460845](https://github.com/mitos-run/mitos/commit/04608454cff4c12b6ebf25a8941397da290610a0))
* **agent-rs:** patch path-traversal in path_allowed, cap at 64 MiB, quality fixes ([#310](https://github.com/mitos-run/mitos/issues/310)) ([8ce2315](https://github.com/mitos-run/mitos/commit/8ce231562c5828da81c89269b027baa20db51603))
* **agent-rs:** prompt client-disconnect kill, reader join, signal exit -1, PTY conformance test ([#310](https://github.com/mitos-run/mitos/issues/310)) ([1393cd1](https://github.com/mitos-run/mitos/commit/1393cd13d550b8bbd503f9829d1bb638f64a05f0))
* **ci,docker:** build the Rust agent everywhere the Go agent was built ([#310](https://github.com/mitos-run/mitos/issues/310)) ([01bc11b](https://github.com/mitos-run/mitos/commit/01bc11b249e114865cfddef8f6067d4848b5a1e0))
* **ci:** restore warm.min on husk-mode e2e SandboxPools (v1 migration regression, [#299](https://github.com/mitos-run/mitos/issues/299)) ([8ebf2d3](https://github.com/mitos-run/mitos/commit/8ebf2d3bcd950424b1fd7b7eea8009de69d1edad))
* **ci:** restore warm.min on husk-mode e2e SandboxPools (v1 migration regression, [#299](https://github.com/mitos-run/mitos/issues/299)) ([b758f89](https://github.com/mitos-run/mitos/commit/b758f89aa39d74c756c224e7b2f6f25142348941))
* **console:** pin pnpm so the console image builds reproducibly ([e512c91](https://github.com/mitos-run/mitos/commit/e512c910df96fab1679989b492c50e8b72b59ff3))
* **controller:** bootstrap PKI in the operator's own namespace ([#335](https://github.com/mitos-run/mitos/issues/335)) ([8c21598](https://github.com/mitos-run/mitos/commit/8c2159826f6aa453a21b26b43b1ae07e7fec1a5c))
* **daemon:** enforce the concurrent-stream cap on the Connect exec path ([b4bf662](https://github.com/mitos-run/mitos/commit/b4bf662faa11e24e226bb5062ac66af9c07839a1))
* **daemon:** restore exec_time_ms, single stream slot, full list entries on the gRPC exec path ([#310](https://github.com/mitos-run/mitos/issues/310)) ([5d23efc](https://github.com/mitos-run/mitos/commit/5d23efc04739643180fbd4c3f1523efc8f405e67))
* **docker:** add musl target to the pinned toolchain after entering the crate dir ([#310](https://github.com/mitos-run/mitos/issues/310)) ([44745c3](https://github.com/mitos-run/mitos/commit/44745c39fc56a03516cea1321093f114d8d09aa4))
* **guest-agent-rs:** atomic WriteFile mode and explicit Mkdir 0o755 ([#310](https://github.com/mitos-run/mitos/issues/310)) ([a04b909](https://github.com/mitos-run/mitos/commit/a04b909eec8416bf0468bbe6783f2f1fa15dddfd))
* **guest-agent-rs:** review fixes for notify-forked orchestrator and SIGUSR2 signal ([#310](https://github.com/mitos-run/mitos/issues/310)) ([281b5df](https://github.com/mitos-run/mitos/commit/281b5dfef9f357c8e036a5f76c89ee2e698c7ea5))
* **guest-agent-rs:** signal ResourceExhausted on Watch channel overflow, add event tests ([#310](https://github.com/mitos-run/mitos/issues/310)) ([03c8239](https://github.com/mitos-run/mitos/commit/03c82392f7a034e8bda36fd5eeaa2c8629e1ca79))
* **guest:** bounds-checked integer narrowing in gRPC vitals/process mapping (CodeQL) ([e0bacdf](https://github.com/mitos-run/mitos/commit/e0bacdfcea6dec86cb11702357241ee4c809a9b2))
* **guest:** non-nil vsock net.Addr so the gRPC server cannot kernel-panic the VM ([9c5961e](https://github.com/mitos-run/mitos/commit/9c5961ed36e31efefbf550a610ea8b47beab8b31))
* **husk:** re-apply host-side tar size cap on the gRPC workspace transport ([#310](https://github.com/mitos-run/mitos/issues/310)) ([652ebd5](https://github.com/mitos-run/mitos/commit/652ebd59d64e7a04bfafdaa9b3b90d696657090c))
* **krew:** point plugin homepage at the GitHub repo for the stars badge ([#307](https://github.com/mitos-run/mitos/issues/307)) ([56f48e1](https://github.com/mitos-run/mitos/commit/56f48e1016f2dff310196c14c1883704f6ec8b34))
* revert unconditional sandbox registration, assert honest not-registered 404 in token tests ([#310](https://github.com/mitos-run/mitos/issues/310)) ([b18e60c](https://github.com/mitos-run/mitos/commit/b18e60ccc2723417a8ca07d0444fe8e09aafc8d7))
* **sandbox-server:** serve the Guest-wired Connect service; tokenless id routing ([6a1d67a](https://github.com/mitos-run/mitos/commit/6a1d67a05991a7480b9b8cee315d284bbae2c45c))
* **sandboxrpc:** fix PortForward defer-order deadlock; connectErr chain and cleanups ([67ca13f](https://github.com/mitos-run/mitos/commit/67ca13ff829dfa838497d14fb155d43693ddf000))
* **sandboxrpc:** prevent Upload reader-goroutine leak on early guest error ([761837b](https://github.com/mitos-run/mitos/commit/761837b98ed7ea0e733d4c9e6a766ca416c83d24))
* **sandboxrpc:** reject empty registered token in BearerInterceptor (defense-in-depth) ([2910796](https://github.com/mitos-run/mitos/commit/2910796b026e3e6f3b9b968568a97bbd587c10bc))
* **sandboxrpc:** test RunCode transport-error exit and correct its comment ([18a7325](https://github.com/mitos-run/mitos/commit/18a7325fa27b8807e275a5b73349bb0b9d885347))
* **sandboxrpc:** thread WriteFile mode through GuestConn; comment + test fixups ([b5cb1f3](https://github.com/mitos-run/mitos/commit/b5cb1f3db38a1ceb2e73de2d4da76bba474cd89b))


### Chores

* release 1.0.0 ([#302](https://github.com/mitos-run/mitos/issues/302)) ([24f3f73](https://github.com/mitos-run/mitos/commit/24f3f73f917fdd6d3eee558b883236b50fbb15ff))

## [0.14.0](https://github.com/mitos-run/mitos/compare/v0.13.0...v0.14.0) (2026-06-23)


### Features

* distribution to Artifact Hub, krew, OperatorHub, and Red Hat ([1c65051](https://github.com/mitos-run/mitos/commit/1c6505195be64c99bd850e0d90b330943b05bd29))
* **sdk:** official Go SDK library (direct mode, hosted mitos.run) ([#250](https://github.com/mitos-run/mitos/issues/250)) ([#291](https://github.com/mitos-run/mitos/issues/291)) ([6b02066](https://github.com/mitos-run/mitos/commit/6b0206643a4631273048a0bc41e183705b57b38a))


### Bug Fixes

* **krew:** rename plugin to mitos (kubectl mitos) ([aac976a](https://github.com/mitos-run/mitos/commit/aac976a5080b26ae5e90aea42a8355c2f14ee0e7))
* **krew:** rename the plugin to mitos (kubectl mitos) ([d5b45c3](https://github.com/mitos-run/mitos/commit/d5b45c39843121f7040b85d5849483602c3d89dd))

## [0.13.0](https://github.com/mitos-run/mitos/compare/v0.12.0...v0.13.0) (2026-06-23)


### Features

* **chart:** console + gateway components, one chart for both editions ([6a2e9f0](https://github.com/mitos-run/mitos/commit/6a2e9f00729b54509ab9e8f36e653e76f19b28a0))
* **console:** @mitos/brand package + capability-gated SPA (Phase B) ([09e7850](https://github.com/mitos-run/mitos/commit/09e785091d5f29889e793f376156f2fabda86de3))
* **console:** billing provider abstraction (Stripe first, MoR-ready) ([1d5359d](https://github.com/mitos-run/mitos/commit/1d5359d920df49058b3f1a56006b3f6a48e59587))
* **console:** embed the SPA in cmd/console + capabilities-from-env + Dockerfile ([095e4c6](https://github.com/mitos-run/mitos/commit/095e4c604146dc039cbe38c7acbd4ed34c4ed5e3)), closes [#214](https://github.com/mitos-run/mitos/issues/214)
* **console:** GET /console/billing/portal (provider-neutral manage-subscription link) ([ca3f85c](https://github.com/mitos-run/mitos/commit/ca3f85c7fff5300dced86f7c1f2654401a9d66eb))
* **console:** hosted/self-hosted dashboard: spec + Phase A backend seams ([baca9a7](https://github.com/mitos-run/mitos/commit/baca9a7f1e9590918ab12165204f46b8a1eb46ea))
* **console:** kube SecretStore provider (org-namespaced Secrets) ([4d56aaa](https://github.com/mitos-run/mitos/commit/4d56aaa426f0a2b27401810b1a1f4454777c39ae)), closes [#275](https://github.com/mitos-run/mitos/issues/275)
* **console:** mount the signature-verified billing webhook + portal wiring ([a5f91cc](https://github.com/mitos-run/mitos/commit/a5f91cc57251bd8bd36767bdb8cef8801d92c231))
* **console:** OpenBao/Vault SecretStore provider (per-org KV-v2 path scoping) ([8dea216](https://github.com/mitos-run/mitos/commit/8dea216df3cef3435ef4ba11994d7f5802bedefd)), closes [#275](https://github.com/mitos-run/mitos/issues/275)
* **console:** org-scoped SandboxControl + hard-isolation tenant convention ([#2](https://github.com/mitos-run/mitos/issues/2)) ([#287](https://github.com/mitos-run/mitos/issues/287)) ([586f7aa](https://github.com/mitos-run/mitos/commit/586f7aa276b830d540fc8e85a5d1f5e502d4e6f4))
* **console:** phase 2: billing portal, secret provider selection, billing webhook ([0eb75f1](https://github.com/mitos-run/mitos/commit/0eb75f13f9c9c2fca53e7a6a1cf15eec51b2d321))
* **console:** real go-oidc verifier + /auth login flow ([caa8201](https://github.com/mitos-run/mitos/commit/caa820117125692115c8831ee2222828424c5144)), closes [#214](https://github.com/mitos-run/mitos/issues/214)
* **console:** select the real secret backend in cmd/console ([d156a7d](https://github.com/mitos-run/mitos/commit/d156a7d60bf93701771ec82bb8c9db00b69b39d6))
* **sdk:** official Java SDK (direct mode, hosted mitos.run) ([#250](https://github.com/mitos-run/mitos/issues/250)) ([#290](https://github.com/mitos-run/mitos/issues/290)) ([279da4f](https://github.com/mitos-run/mitos/commit/279da4fc603e8464f814adf58e18fa1dee9d62ba))
* **sdk:** official Rust SDK (direct mode, hosted mitos.run) ([#250](https://github.com/mitos-run/mitos/issues/250)) ([#280](https://github.com/mitos-run/mitos/issues/280)) ([337d9b6](https://github.com/mitos-run/mitos/commit/337d9b691737cb07092e6cf3b86769e3ea9a5de9))


### Bug Fixes

* **deps:** bump go-jose to v4.1.4 (GO-2026-4945) ([329ebd8](https://github.com/mitos-run/mitos/commit/329ebd83b890530782b7412324fd6b6d314b6ab2))

## [0.12.0](https://github.com/mitos-run/mitos/compare/v0.11.0...v0.12.0) (2026-06-22)


### Features

* **api-v2:** Connect Sandbox service foundation with streaming Exec ([#24](https://github.com/mitos-run/mitos/issues/24)) ([#267](https://github.com/mitos-run/mitos/issues/267)) ([691a016](https://github.com/mitos-run/mitos/commit/691a0167e0cd5193c3a86ed91fe52ef5c5a3e302))
* **auth:** one login authenticates the SDKs, mcp, and CLI ([#210](https://github.com/mitos-run/mitos/issues/210)-adjacent) ([#278](https://github.com/mitos-run/mitos/issues/278)) ([0ef2848](https://github.com/mitos-run/mitos/commit/0ef28487795cab1c083fd792fc765ead436da262))
* **cli:** first-class packaging, goreleaser + install.sh + OS-aware docs ([#253](https://github.com/mitos-run/mitos/issues/253)) ([#268](https://github.com/mitos-run/mitos/issues/268)) ([5e7b3de](https://github.com/mitos-run/mitos/commit/5e7b3de03cb5c002d8f22b09acf449da13741b67))
* **controller:** fork-budget attenuation, depth-aggregate + never-widen ([#25](https://github.com/mitos-run/mitos/issues/25)) ([#266](https://github.com/mitos-run/mitos/issues/266)) ([c73313b](https://github.com/mitos-run/mitos/commit/c73313bd3c58c94cdf4b25e90b28cd0224e46445))
* **sandbox-server:** TCP-over-vsock guest port forwarding ([#228](https://github.com/mitos-run/mitos/issues/228)) ([#271](https://github.com/mitos-run/mitos/issues/271)) ([d3408c9](https://github.com/mitos-run/mitos/commit/d3408c99e9208166f45fce8027cb44d6f431ff48))
* **sdk:** official Ruby SDK (direct mode, hosted mitos.run) ([#250](https://github.com/mitos-run/mitos/issues/250)) ([#273](https://github.com/mitos-run/mitos/issues/273)) ([3c0fabf](https://github.com/mitos-run/mitos/commit/3c0fabfe2ece4ce0cae74ec0d031bae2c418a6ad))
* **skill,sdk,mcp:** Agent Skill + default agent surfaces to hosted mitos.run ([#252](https://github.com/mitos-run/mitos/issues/252)) ([#269](https://github.com/mitos-run/mitos/issues/269)) ([182c537](https://github.com/mitos-run/mitos/commit/182c537210fd2d62bf37281708f88e3645062135))

## [0.11.0](https://github.com/mitos-run/mitos/compare/v0.10.0...v0.11.0) (2026-06-22)


### Features

* **chart:** deployable SandboxPool conversion webhook behind a gated value ([#22](https://github.com/mitos-run/mitos/issues/22)) ([#254](https://github.com/mitos-run/mitos/issues/254)) ([4f3706e](https://github.com/mitos-run/mitos/commit/4f3706e20e0d4572ab021801460bc239d0374ca6))
* **sandbox-server:** implement real-mode fork+exec via internal/fork.Engine ([#22](https://github.com/mitos-run/mitos/issues/22)) ([#257](https://github.com/mitos-run/mitos/issues/257)) ([b7a9b85](https://github.com/mitos-run/mitos/commit/b7a9b8503561902d7f1fb3d0254c9bfd8278160a))


### Bug Fixes

* **ci:** give the sdk-conformance mock server a distinct data-dir ([#22](https://github.com/mitos-run/mitos/issues/22)) ([#260](https://github.com/mitos-run/mitos/issues/260)) ([63a91a6](https://github.com/mitos-run/mitos/commit/63a91a643e8f87d6faa3eef38a7dfa8e66f2f9e6))
* **sdk-ts:** send Idempotency-Key on createTemplate and fork ([#22](https://github.com/mitos-run/mitos/issues/22)) ([#251](https://github.com/mitos-run/mitos/issues/251)) ([7c33381](https://github.com/mitos-run/mitos/commit/7c333812308f46019d2bddc312dc1258bc10d633))
* **sdk:** lazy-load kubernetes so direct mode needs only httpx ([#22](https://github.com/mitos-run/mitos/issues/22)) ([#258](https://github.com/mitos-run/mitos/issues/258)) ([9efbd79](https://github.com/mitos-run/mitos/commit/9efbd798864a7bc7e9527c082e67e500ee1a4cdf))

## [0.10.0](https://github.com/mitos-run/mitos/compare/v0.9.0...v0.10.0) (2026-06-22)


### Features

* **fork:** distinct guest MAC per fork; KVM-prove network identity; mark [#3](https://github.com/mitos-run/mitos/issues/3) row 4 done ([#248](https://github.com/mitos-run/mitos/issues/248)) ([788d872](https://github.com/mitos-run/mitos/commit/788d872edf4df46e0a446ca1e950fc2384204bdb))

## [0.9.0](https://github.com/mitos-run/mitos/compare/v0.8.1...v0.9.0) (2026-06-22)


### Features

* **api-v2:** idempotency keys on creating sandbox-server calls and SDK ([3b3b80e](https://github.com/mitos-run/mitos/commit/3b3b80e47f7e804fc169c27ee4b797551bcb0f79))
* **api-v2:** in-guest self-service socket and mitos.guest SDK ([e02646a](https://github.com/mitos-run/mitos/commit/e02646a2f5ece4327748274e970e0e2a22b65b10))
* **api:** add v1alpha2 consolidated three-noun types and SandboxPool conversion ([a9dfc3b](https://github.com/mitos-run/mitos/commit/a9dfc3b3507f80f0122e9ff1179ced811f9f19d2))
* **api:** define sandbox.v1 runtime protocol contract and stubs ([fb7865b](https://github.com/mitos-run/mitos/commit/fb7865b5de9bfaaaea8aa67f9cea71e41aadc02c))
* **apierr:** normative error-code catalogue, doc-sync, static remediation lint ([#28](https://github.com/mitos-run/mitos/issues/28)) ([bfc7c4a](https://github.com/mitos-run/mitos/commit/bfc7c4a0dbf6e8a7faa619270f29b7e219f58665))
* **bench:** real claim-storm pinning measurement, off vs on ([#168](https://github.com/mitos-run/mitos/issues/168)) ([337b25c](https://github.com/mitos-run/mitos/commit/337b25c61c6017c759e93695a2ee8eb5ae656413))
* **captoken:** attenuated capability-token core, budget API, exhaustion error ([0354dd4](https://github.com/mitos-run/mitos/commit/0354dd4c8a6eae130c00419642a71e3278213ac0))
* **controller:** consolidated v1alpha2 Sandbox reconciler over the existing engine ([d79e323](https://github.com/mitos-run/mitos/commit/d79e323b354b97c5807148049ca8bbd7080a0aef))
* **controller:** elide no-op claim status writes under churn ([#163](https://github.com/mitos-run/mitos/issues/163)) ([93151ae](https://github.com/mitos-run/mitos/commit/93151ae7328f9ebcd53c92b8b8b47ef549f666c7))
* **controller:** enforce capability fork budget on self-initiated forks ([#25](https://github.com/mitos-run/mitos/issues/25)) ([#234](https://github.com/mitos-run/mitos/issues/234)) ([485c1a7](https://github.com/mitos-run/mitos/commit/485c1a778ea943c2440e569d2b1fe9ff51d924fd))
* **controller:** wire v1alpha2 scheme, Sandbox reconciler, and pool conversion webhook ([be9a9a2](https://github.com/mitos-run/mitos/commit/be9a9a25cf0f9d705bdd1ed8a92b39d037b4253f))
* **cpupin:** dynamic post-ready CPU pinning + launch scheduling priority ([0ca4a8e](https://github.com/mitos-run/mitos/commit/0ca4a8e8889c1e9ab250fe3a8f26acb3f3f40ad2))
* **deploy:** mitos doctor preflight + host/kernel prereq docs + deploy-layer errors ([7ec9404](https://github.com/mitos-run/mitos/commit/7ec94043a1c829762353e550b52c4016a3fde270))
* **doctor:** userfaultfd preflight check + host-prereq doc ([#174](https://github.com/mitos-run/mitos/issues/174), [#167](https://github.com/mitos-run/mitos/issues/167)) ([4a84e09](https://github.com/mitos-run/mitos/commit/4a84e09b6c711f9ef9c66cf3c91c46ba9a591234))
* **errors:** typed discriminable error hierarchy and deterministic timeouts ([#216](https://github.com/mitos-run/mitos/issues/216)) ([b253dcf](https://github.com/mitos-run/mitos/commit/b253dcf5720e517a6d4ea07473dd8d59918d58b0))
* **fork:** continuous virtio-rng device, jailer cap-list, KVM UUID/TLS/memory assertions ([#3](https://github.com/mitos-run/mitos/issues/3)) ([206725f](https://github.com/mitos-run/mitos/commit/206725f195818d9900872ae896c823dddd69ea6b))
* **forkd:** LLM-legible guest-kernel-missing error; close out [#174](https://github.com/mitos-run/mitos/issues/174) ([#232](https://github.com/mitos-run/mitos/issues/232)) ([c76ab26](https://github.com/mitos-run/mitos/commit/c76ab26cedf2ecd7a148e8d6ab2869ae257915dc))
* **fork:** hugepage-backed guest memory build-time plumbing ([#167](https://github.com/mitos-run/mitos/issues/167)) ([7f51d47](https://github.com/mitos-run/mitos/commit/7f51d47e22290343531e432bc2faa9cbfe89c23d))
* **fork:** snapshot-resume prefetch design, hot-page manifest field, capture seam ([#167](https://github.com/mitos-run/mitos/issues/167)) ([9769d3e](https://github.com/mitos-run/mitos/commit/9769d3e62e5549cefc5e384c0c1093303184c249))
* **fork:** userfaultfd memory backend for hugepage restore + hot-page prefetch ([#167](https://github.com/mitos-run/mitos/issues/167)) ([5484f97](https://github.com/mitos-run/mitos/commit/5484f9765f2b5659e84866631aa812640419a1e8))
* **gc:** reclaim orphan volume backings past OrphanGrace ([#163](https://github.com/mitos-run/mitos/issues/163)) ([bf0ba74](https://github.com/mitos-run/mitos/commit/bf0ba74c966d702b42808dafee66fffed5b7cd84))
* **gc:** typed OrphanReaped condition on a swept terminal claim ([#163](https://github.com/mitos-run/mitos/issues/163)) ([2bdb9b8](https://github.com/mitos-run/mitos/commit/2bdb9b8af9a579a42d9a3ac3146fbc5586693824))
* **gpu:** GPU-aware scheduling, larger sizes, GPU-seconds metering ([#221](https://github.com/mitos-run/mitos/issues/221)) ([d72140a](https://github.com/mitos-run/mitos/commit/d72140ae7294d0ad6a9481a2d732d131ba977254))
* **lifecycle:** live set_timeout, work-aware idle, first-class pause/resume ([bc0d1cb](https://github.com/mitos-run/mitos/commit/bc0d1cbb69ff66a893ec8f2712c360d70f8f934e))
* **metering:** per-sandbox lifetime memory labels; mark [#3](https://github.com/mitos-run/mitos/issues/3) row 5 done ([#244](https://github.com/mitos-run/mitos/issues/244)) ([f72fa2c](https://github.com/mitos-run/mitos/commit/f72fa2c5e29afa7cfe2f951c6a0937780b5a5eca))
* **network:** first-class egress/ingress SDK and API knobs ([#219](https://github.com/mitos-run/mitos/issues/219)) ([29703c7](https://github.com/mitos-run/mitos/commit/29703c7c0ff875beef58eaa3cf2b93ae4779ab26))
* **observability:** Hubble/OpenCost attribution, distribution-lag metric ([#164](https://github.com/mitos-run/mitos/issues/164)) ([48ea8a0](https://github.com/mitos-run/mitos/commit/48ea8a0b9cea54d87191257245f99e02308a9775))
* **observability:** Layer 3 guest telemetry vsock bridge ([#164](https://github.com/mitos-run/mitos/issues/164)) ([de27713](https://github.com/mitos-run/mitos/commit/de27713260a2d3bfc4767e73d3b700be2974c12e))
* **observability:** thread vitals labels through the Fork gRPC ([#164](https://github.com/mitos-run/mitos/issues/164)) ([2160ec5](https://github.com/mitos-run/mitos/commit/2160ec5b7d1de392a01475e9b8e33d2d8d86ba34))
* **paperclip:** map sandbox-provider contract onto SandboxClaims ([#20](https://github.com/mitos-run/mitos/issues/20)) ([6665e6d](https://github.com/mitos-run/mitos/commit/6665e6dcf4d19e682bda2547733035eaadab55ce))
* **preview:** per-sandbox preview URLs with signed expiring tokens and routing ([#126](https://github.com/mitos-run/mitos/issues/126)) ([a733397](https://github.com/mitos-run/mitos/commit/a7333973384aedbb6c21460858909de6f53779eb))
* **saas:** billing-grade usage pipeline + org-scoped usage API ([#211](https://github.com/mitos-run/mitos/issues/211)) ([06405f8](https://github.com/mitos-run/mitos/commit/06405f832763209ec406094289ecf694c3862341))
* **saas:** customer accounts, orgs, scoped API keys, and public gateway ([d6b1d1d](https://github.com/mitos-run/mitos/commit/d6b1d1d3cf315b2c4a5ad73175f1283c71b6beb1))
* **saas:** org-scoped console BFF for the hosted web console ([#214](https://github.com/mitos-run/mitos/issues/214)) ([72731b6](https://github.com/mitos-run/mitos/commit/72731b6c12cff9b0831ef70879700213db457ebf))
* **saas:** quota, rate-limit, egress-tier, and kill-switch abuse controls ([#213](https://github.com/mitos-run/mitos/issues/213)) ([b1f5484](https://github.com/mitos-run/mitos/commit/b1f5484056b57b14c1d927246840b6673ba9fed6))
* **saas:** self-serve onboarding funnel core, gated waitlist vs open ([#215](https://github.com/mitos-run/mitos/issues/215)) ([17978aa](https://github.com/mitos-run/mitos/commit/17978aaa1ba938d35951a7df07a7331653788424))
* **saas:** Stripe metered billing core behind a fake client ([#212](https://github.com/mitos-run/mitos/issues/212)) ([0ee1eb6](https://github.com/mitos-run/mitos/commit/0ee1eb674f35681bae5c42e4d825419b470ee411))
* **scheduler:** node isolation-tier floor + PVM evaluation ([#40](https://github.com/mitos-run/mitos/issues/40)) ([58934b4](https://github.com/mitos-run/mitos/commit/58934b4956c857b564a9b0a572e84006434b9a72))
* **sdk:** add VibeKit and ZenML sandbox-backend adapters ([#205](https://github.com/mitos-run/mitos/issues/205)) ([1d42cec](https://github.com/mitos-run/mitos/commit/1d42cec4f721bc86ac802110ed3675d236cbd1d7))
* **sdk:** E2B-compat shim mitos.e2b over the sandbox-server ([a024943](https://github.com/mitos-run/mitos/commit/a024943a498508f6d635b56d81b2db8b09bdac1b))
* **sdk:** flat API-key-authed one-liner native onboarding ([#217](https://github.com/mitos-run/mitos/issues/217)) ([ad2e0ba](https://github.com/mitos-run/mitos/commit/ad2e0baa33067f44788d4d493214cdabdb6e4e3a))
* **sdk:** LangChain sandbox backend (MitosSandbox) ([9d9fe79](https://github.com/mitos-run/mitos/commit/9d9fe79aa288502ad96e3ee32f7f3f42bf43e3dc))
* **sdk:** OpenAI Agents SDK + Claude Agent SDK sandbox adapters ([#204](https://github.com/mitos-run/mitos/issues/204)) ([165aa4b](https://github.com/mitos-run/mitos/commit/165aa4b45d2b9f672a7046d7305258cf5a2cd453))
* **templates:** code-first declarative builder and cached builds ([#220](https://github.com/mitos-run/mitos/issues/220)) ([cfd13ba](https://github.com/mitos-run/mitos/commit/cfd13ba491547c0fc0d4ca21f2c897a786439f58))


### Bug Fixes

* **api-v2:** do not serve v1alpha2 SandboxPool without conversion webhook ([1535e5a](https://github.com/mitos-run/mitos/commit/1535e5acf7cbb7072d2ab32c71502541c9833570))
* **billing:** Drawdown replay reports the original credit split, not FromCredit:0 ([#212](https://github.com/mitos-run/mitos/issues/212)) ([#237](https://github.com/mitos-run/mitos/issues/237)) ([5d62aae](https://github.com/mitos-run/mitos/commit/5d62aae20a31ed34ee0a9777c7d82a14bb385c76))
* **billing:** webhook returns 500 on internal error, 401 only on bad signature ([#212](https://github.com/mitos-run/mitos/issues/212)) ([#239](https://github.com/mitos-run/mitos/issues/239)) ([06d074c](https://github.com/mitos-run/mitos/commit/06d074c76f402d2d2a9c070635887cd2228f4b11))
* **cas:** validate chunk digests on manifest decode, close a peer-pull path traversal ([#242](https://github.com/mitos-run/mitos/issues/242)) ([1485ef5](https://github.com/mitos-run/mitos/commit/1485ef5052ac30fd90d3f3ec14d69a99a1903ba9))
* **controller:** make workspace dehydrate-on-terminate idempotent ([128059f](https://github.com/mitos-run/mitos/commit/128059fdaad75cd67aace36c3778d6c43c492d22))
* **controller:** status elision must compare Ready observedGeneration ([#25](https://github.com/mitos-run/mitos/issues/25)) ([#241](https://github.com/mitos-run/mitos/issues/241)) ([7d6496e](https://github.com/mitos-run/mitos/commit/7d6496ee8b7838849f1090cee655040dc0a14556))
* **fork:** capture page size from snapshot backing; capture forks skip preload ([#167](https://github.com/mitos-run/mitos/issues/167)) ([e2717a4](https://github.com/mitos-run/mitos/commit/e2717a4250380cf349c05a4c4f5c0a0a3e457436))
* **fork:** do not fail forkd startup on an unstaged guest kernel ([3a8baeb](https://github.com/mitos-run/mitos/commit/3a8baebae6f49cfc6f615348207a56ce97766f96))
* **fork:** drop engine guest-kernel precondition check (regression) ([224d63d](https://github.com/mitos-run/mitos/commit/224d63db23ef030ed887fde0536fbb6b8479293f))
* **fork:** UFFD restore deadlock + page-size parsing (validated on KVM) ([#167](https://github.com/mitos-run/mitos/issues/167)) ([127ae53](https://github.com/mitos-run/mitos/commit/127ae53e2c5fdb489ad47f36233e89529976d77d))
* **onboarding:** Verify recovers from a half-provisioned account ([#215](https://github.com/mitos-run/mitos/issues/215)) ([#238](https://github.com/mitos-run/mitos/issues/238)) ([c92cea5](https://github.com/mitos-run/mitos/commit/c92cea5ebbc6f55bf589f877564ca1ae6711e604))
* **saas:** avoid empty apierr.Error literal in console caller helper ([fe106b3](https://github.com/mitos-run/mitos/commit/fe106b34129b2f811cfb59b83282d84e790ce8bb))
* **sandbox-server:** atomic idempotency-key reserve, close the TOCTOU double-create race ([#22](https://github.com/mitos-run/mitos/issues/22)) ([#240](https://github.com/mitos-run/mitos/issues/240)) ([5fa2666](https://github.com/mitos-run/mitos/commit/5fa2666ee8b13e0e1aa6d589eb856dff5c780a99))
* **security:** bound GPU label parse and guard pause path traversal ([4e274fe](https://github.com/mitos-run/mitos/commit/4e274fe480d4796bb8182fcf5840d37dcdc406d1))
* **security:** use regexp allowlist for pause sandbox-id path guard ([3d65758](https://github.com/mitos-run/mitos/commit/3d65758d44e9c2810b3e3346afde1511e090962c))
* **usage:** reset-aware counter integration, no negative bill on restart ([#211](https://github.com/mitos-run/mitos/issues/211)) ([#236](https://github.com/mitos-run/mitos/issues/236)) ([d158813](https://github.com/mitos-run/mitos/commit/d158813008690de13451f3ee77541a8fee61bf70))

## [0.8.1](https://github.com/mitos-run/mitos/compare/v0.8.0...v0.8.1) (2026-06-19)


### Bug Fixes

* **controller:** adopt an already-active fork child instead of looping forever ([#183](https://github.com/mitos-run/mitos/issues/183)) ([4b5ef38](https://github.com/mitos-run/mitos/commit/4b5ef384f230721172c1d79c32bf726167d2404f))
* **controller:** adopt an already-active fork child instead of looping forever ([#183](https://github.com/mitos-run/mitos/issues/183)) ([d11812e](https://github.com/mitos-run/mitos/commit/d11812ea03fb68022d3493dea8402a4eb3212fb3))
* **controller:** constrain template snapshot build to a pool's placement nodes ([#172](https://github.com/mitos-run/mitos/issues/172)) ([3fc1652](https://github.com/mitos-run/mitos/commit/3fc1652e0190ea29754bc7b65803f5a537624774))
* **controller:** constrain template snapshot build to a pool's placement nodes ([#172](https://github.com/mitos-run/mitos/issues/172)) ([c78ba53](https://github.com/mitos-run/mitos/commit/c78ba53e817a1540e3fa894ea8e7ad0d2766059e))
* **controller:** elide no-op SandboxPool status writes ([#163](https://github.com/mitos-run/mitos/issues/163)) ([3de2122](https://github.com/mitos-run/mitos/commit/3de212278023fff9625042f6967ddb4c91c522f5))
* **controller:** elide no-op SandboxPool status writes ([#163](https://github.com/mitos-run/mitos/issues/163)) ([815b54a](https://github.com/mitos-run/mitos/commit/815b54a3852bc781318cfbe0969c0150fb6bb4a4))
* **forkd:** sample lifetime memory metrics periodically ([#3](https://github.com/mitos-run/mitos/issues/3) fork-correctness Row 5) ([979318c](https://github.com/mitos-run/mitos/commit/979318c263df03c0ad1be3d86d08b09fd5fcbb73))
* **forkd:** sample lifetime memory metrics periodically ([#3](https://github.com/mitos-run/mitos/issues/3) fork-correctness Row 5) ([40492ba](https://github.com/mitos-run/mitos/commit/40492ba7f7326c67577183dcb9e971d564897335))

## [0.8.0](https://github.com/mitos-run/mitos/compare/v0.7.0...v0.8.0) (2026-06-19)


### Features

* **controller:** dedicatedNodes pool placement for hard tenant separation ([#172](https://github.com/mitos-run/mitos/issues/172)) ([10b74ad](https://github.com/mitos-run/mitos/commit/10b74ad2668c29c866b17ec7a0f331c23f178a8a))
* **controller:** restrict husk-pod placement to placement-matching snapshot holders ([#172](https://github.com/mitos-run/mitos/issues/172)) ([de13d95](https://github.com/mitos-run/mitos/commit/de13d95351ba24def937b992beef4aed99bd5596))


### Bug Fixes

* **chart:** make a fresh helm install come up out of the box ([#173](https://github.com/mitos-run/mitos/issues/173)) ([0ee5ccc](https://github.com/mitos-run/mitos/commit/0ee5cccf0f47e5ceda55772fe0dbe200e54271b2))
* **controller:** cross-node husk failover (per-node activation digest + release label on failure) ([#177](https://github.com/mitos-run/mitos/issues/177)) ([d3ed0a7](https://github.com/mitos-run/mitos/commit/d3ed0a7e6c1f6034aeebab83db3fd7f44507d44b))
* **controller:** evict husk pods from a lost node in ~60s, not 300s ([#177](https://github.com/mitos-run/mitos/issues/177)) ([ec7cc72](https://github.com/mitos-run/mitos/commit/ec7cc72228fdf4a6f0a0a142098a95d1678ad7f4))
* **controller:** log the cause of a failed fork-child activation ([#28](https://github.com/mitos-run/mitos/issues/28)) ([91769f2](https://github.com/mitos-run/mitos/commit/91769f29a0de8a3412218287d6bb78610b3a867b))
* **controller:** pin each husk pod to one snapshot node + its own digest ([#175](https://github.com/mitos-run/mitos/issues/175)) ([873f182](https://github.com/mitos-run/mitos/commit/873f182690a5b2656cf5555644688cf3748d9c3e))
* **controller:** reflect backing-pod readiness in a Ready husk claim ([#177](https://github.com/mitos-run/mitos/issues/177)) ([e72c088](https://github.com/mitos-run/mitos/commit/e72c088d660dbdfd6680c707fa897b24ee83c0e3))

## [0.7.0](https://github.com/mitos-run/mitos/compare/v0.6.0...v0.7.0) (2026-06-18)


### Features

* **controller:** narrow controller Secrets RBAC to adopted pool namespaces ([6a57b63](https://github.com/mitos-run/mitos/commit/6a57b63c0f0a4e3d9dad3a003c2f98072ea0c23a))
* **controller:** rotate the per-namespace husk server leaf before expiry ([9d3addd](https://github.com/mitos-run/mitos/commit/9d3adddae4c57c3114b2749387ed5547bdd6c96b))
* **fork:** re-sample live memory in Metering for lifetime accounting ([bfb725b](https://github.com/mitos-run/mitos/commit/bfb725b0c7b66bc8e0dfecfff9d99f897b9c58af))


### Bug Fixes

* **controller:** mirror pool-secrets RBAC to kustomize + make the binding non-fatal ([8abe700](https://github.com/mitos-run/mitos/commit/8abe70087c38f308e7d8644689b47fd9c593f754))
* **rbac:** grant list+watch on rolebindings so the cached informer syncs ([8d60504](https://github.com/mitos-run/mitos/commit/8d60504997c81f8ee1e0aca22979488b6e766f99))

## [0.6.0](https://github.com/mitos-run/mitos/compare/v0.5.0...v0.6.0) (2026-06-18)


### Features

* **controller:** dial husk pods pinning the per-namespace identity ([326533d](https://github.com/mitos-run/mitos/commit/326533db8bd3a9d897e18fd884a4f9deca345ddb))
* **controller:** husk pods serve the per-namespace leaf; stop replicating the forkd key ([63bc29e](https://github.com/mitos-run/mitos/commit/63bc29e1cfabc3303c7ce7cc0a8298fd66dd303a))
* **controller:** issue a per-namespace husk server leaf (mitos-husk-tls) ([3f11ed2](https://github.com/mitos-run/mitos/commit/3f11ed2ddf482e48aaf162aa1cd73eb73c52db13))
* **pki:** ClientTLSConfigFor pins an arbitrary server name; ClientTLSConfig delegates ([3654541](https://github.com/mitos-run/mitos/commit/3654541857d3a5594f8ac6b119938e301dff3557))
* **pki:** issue per-namespace husk server leaves (husk.&lt;ns&gt;.mitos, server-auth) ([d1cac0c](https://github.com/mitos-run/mitos/commit/d1cac0c25b667b9306a9745668ab788695d8ada2))


### Bug Fixes

* **controller:** bind claim spec.serviceAccount to authorization via admission webhook ([5d80d2a](https://github.com/mitos-run/mitos/commit/5d80d2a6e163e2f7c932096568310ab5d4c15936))
* **controller:** require controller owner ref before activating a husk pod ([8682306](https://github.com/mitos-run/mitos/commit/86823068598eca2ff00826ddf45a5ade521f5dc5))
* **deploy:** verify guest kernel integrity and drop SA tokens on privileged pods ([9e0ee4c](https://github.com/mitos-run/mitos/commit/9e0ee4c4ac073cdcd8260c10f81c3165fc8cea18))
* **dnsproxy:** block IPv6-embedded private targets in rebind filter (NAT64/6to4/site-local) ([f50fc03](https://github.com/mitos-run/mitos/commit/f50fc035e4926b90aafbbd8bf955455c442b1473))
* **guest:** fail closed when the fork RNG reseed is not credited (§1) ([58614d6](https://github.com/mitos-run/mitos/commit/58614d6ae8972ce8884e08e529fee2f3eb499a93))
* **husk:** filter guest-to-pod-local traffic on the nftables input hook ([52eb1bf](https://github.com/mitos-run/mitos/commit/52eb1bf14f64572bf5deaf4033af9e2a623c0026))
* security audit remediation (6 findings) + fork-correctness reseed fail-closed ([835f457](https://github.com/mitos-run/mitos/commit/835f4576bc1b1e98350bf4f3b7530120c8955b21))

## [0.5.0](https://github.com/mitos-run/mitos/compare/v0.4.0...v0.5.0) (2026-06-16)


### Features

* **controller:** fleet-observability metrics (husk pod created/lost, node lost, refill latency) ([d1629e3](https://github.com/mitos-run/mitos/commit/d1629e380ebf27941390db7c668049fa312c79d9))
* **controller:** fleet-observability metrics (husk pod created/lost, node lost, refill latency) ([6b79a92](https://github.com/mitos-run/mitos/commit/6b79a92475923c3d90d3a899d80ca602a304611a))
* **deploy:** Helm chart for the mitos control plane ([#37](https://github.com/mitos-run/mitos/issues/37)) ([28b6e8a](https://github.com/mitos-run/mitos/commit/28b6e8a4674fbe3779ff18325b2068f73cb9de42))
* **deploy:** Helm chart for the mitos control plane ([#37](https://github.com/mitos-run/mitos/issues/37)) ([fa95761](https://github.com/mitos-run/mitos/commit/fa957611b51c97ea4e61d7cd88df00aeb92bdd11))

## [0.4.0](https://github.com/mitos-run/mitos/compare/v0.3.0...v0.4.0) (2026-06-16)


### Features

* **controller:** add NET_ADMIN to husk pod for in-pod egress firewall ([23ffe77](https://github.com/mitos-run/mitos/commit/23ffe7772e7254ce8bdef81e6372e92bdd3250cc))
* **controller:** emit best-effort husk NetworkPolicy (default-deny egress) ([4e52c2b](https://github.com/mitos-run/mitos/commit/4e52c2bb4d3b6194eb757064ee9182fd23e89541))
* **controller:** ensure husk NetworkPolicy during pool reconcile ([795000f](https://github.com/mitos-run/mitos/commit/795000ffa38a646df62f5cd580e3d661ad52502b))
* **controller:** thread template egress policy + allowlist into husk activate ([1954a03](https://github.com/mitos-run/mitos/commit/1954a03372b28a2d60395e87e06afa7991f94f53))
* **husk-network:** complete name-based egress datapath (DNS upstream + SNAT) ([8a39a74](https://github.com/mitos-run/mitos/commit/8a39a742bbb99eb48ab9ec9000f5b89acd8ec717))
* **husk-network:** set pod-netns ip_forward via a scoped init container, no node change ([a203c6f](https://github.com/mitos-run/mitos/commit/a203c6f535b56bbe2513b33a7d357c3c64091632))
* **husk-stub:** wire exec netfilter runner + dns upstream flags ([aa34340](https://github.com/mitos-run/mitos/commit/aa34340e116fb61fe335a38cbb72af72000d82dd))
* **husk:** apply in-pod egress filter + DNS proxy at activate ([0fd8929](https://github.com/mitos-run/mitos/commit/0fd8929dce7e4891d270562fbb8f22f824eb71e6))
* **husk:** carry egress policy + allowlist in the activate control message ([347cc26](https://github.com/mitos-run/mitos/commit/347cc265e3448bb19cd22e520e9833874a6178dc))
* **husk:** in-pod egress filter orchestration reusing netconf ([5640778](https://github.com/mitos-run/mitos/commit/5640778968a06fd12c90fd5525eea5120722169d))
* **husk:** per-pod DNS proxy for name-allowlist egress ([4b98c6e](https://github.com/mitos-run/mitos/commit/4b98c6e36162028e057723c962019602151a7263))
* **netconf:** unconditional cloud-metadata drop in every sandbox chain ([381a88f](https://github.com/mitos-run/mitos/commit/381a88fd35bedae20321e3527f1a239afbcab4a6))


### Bug Fixes

* **ci-runner:** grant runner networkpolicies read for the husk-network e2e ([db950fa](https://github.com/mitos-run/mitos/commit/db950fa347423e6e554265fd8677f1c789578679))
* **ci-runner:** grant runner networkpolicies read for the husk-network e2e ([6d95158](https://github.com/mitos-run/mitos/commit/6d951582a68b21e79acceacbd52d33bdc82ca2fd))
* **controller:** drop the terminate finalizer when the bound workspace is gone ([8e5e772](https://github.com/mitos-run/mitos/commit/8e5e772dd7dc0be3243f5ad8d28c764e40f6a27e))
* **deviceplugin:** re-register with the kubelet after it restarts ([5bc2d93](https://github.com/mitos-run/mitos/commit/5bc2d93e86711fcd5a6554eaf641d0879f35ba39))
* **deviceplugin:** start the kubelet.sock watch before registering ([08a4045](https://github.com/mitos-run/mitos/commit/08a404510bd561ec6192233151ee1815b906b872))
* **dnsproxy:** refuse to pin non-public resolved addresses (DNS-rebind defense) ([6b43bcf](https://github.com/mitos-run/mitos/commit/6b43bcf125212c2510d4e5d9fe1e21aacc1ea588))
* **dnsproxy:** refuse to pin non-public resolved addresses (DNS-rebind defense) ([b916d75](https://github.com/mitos-run/mitos/commit/b916d75721269c8621dc33d5aa3f68e6c046777f))
* **husk-network:** bind the in-pod DNS resolver IP to the tap ([9febb1a](https://github.com/mitos-run/mitos/commit/9febb1a4ebbdaa4bdb145ef3b2af8a397ee81390))
* **husk-network:** enable pod-netns ip_forward via kubelet sysctl, fail open-safe ([c9c1616](https://github.com/mitos-run/mitos/commit/c9c161691df82d2d38b3cedf3f8da6bdca93a12d))
* **husk-network:** guest configures eth0 via rtnetlink, not the missing ip binary ([a4a0271](https://github.com/mitos-run/mitos/commit/a4a02714f40e79282b70c2a1026b76d7eaa3fa66))
* **husk:** enable forkd networking so the template bakes the eth0 NIC ([#150](https://github.com/mitos-run/mitos/issues/150)) ([200e348](https://github.com/mitos-run/mitos/commit/200e348230fd67740ceae35613ee3e7dd33acda9))
* **husk:** forkd image needs iproute2 + nftables; re-enable networking; mirror base image ([66bacb3](https://github.com/mitos-run/mitos/commit/66bacb39a34f37e5b7b3873525dc94ab8b193e51))
* **husk:** husk-stub image needs iproute2 + nftables for the in-pod egress filter ([22254e5](https://github.com/mitos-run/mitos/commit/22254e50699ea7078c5a30af186da47bc62b2074))
* **husk:** husk-stub image needs iproute2 + nftables for the in-pod egress filter ([1feb8f8](https://github.com/mitos-run/mitos/commit/1feb8f804812f62f3261e21dc0fe9079712e958b))
* **husk:** readiness probe gates the pod on the dormant control listener ([96c5dcc](https://github.com/mitos-run/mitos/commit/96c5dcc0f646bbeeaaa0fe8c13aad001421d1445))
* **husk:** wait for the template rootfs at Prepare instead of crash-looping ([04c0f42](https://github.com/mitos-run/mitos/commit/04c0f42b7d81726afbac7278df8202156e0a79c1))
* **security:** fail closed when a forked VM does not reseed its RNG ([#137](https://github.com/mitos-run/mitos/issues/137)) ([92a04eb](https://github.com/mitos-run/mitos/commit/92a04eb88ee5e8bebac43c649cba0453fc4f1508))
* **security:** four hardening fixes (husk SA token, gRPC fail-closed, vsock read deadline, clock residual) ([#136](https://github.com/mitos-run/mitos/issues/136)) ([8977aed](https://github.com/mitos-run/mitos/commit/8977aedc77b5654d6452b7b730f666d2b8be8a04))
* **security:** per-fork rootfs CoW on raw-forkd to stop cross-fork write bleed ([#138](https://github.com/mitos-run/mitos/issues/138)) ([e72bd34](https://github.com/mitos-run/mitos/commit/e72bd34cd7a313590de8e2da1094a59f7e44ed64))

## [0.3.0](https://github.com/mitos-run/mitos/compare/v0.2.0...v0.3.0) (2026-06-14)


### Features

* AAAA/IPv6 answers in the name egress allowlist ([314104c](https://github.com/mitos-run/mitos/commit/314104c61ab7589e7007b79e6acc5cc918817c7e))
* add --rootfs-cow-dir and --template-rootfs flags to husk-stub ([d957c7e](https://github.com/mitos-run/mitos/commit/d957c7e4b358f82c6685f82dfc6af1c7d198d2c8))
* add forkd NDJSON exec-stream endpoint and aggregate one-shot exec on it ([51a679d](https://github.com/mitos-run/mitos/commit/51a679dc72550725727a88a127c01c4cf8e28bf5))
* add host vsock ExecStream over a dedicated connection ([1be44f1](https://github.com/mitos-run/mitos/commit/1be44f1d3d96edfe333501c5b87a7d1b382840d9))
* add PatchDrive to the husk vmm interface ([ea8a46a](https://github.com/mitos-run/mitos/commit/ea8a46adf9ae1e91c740e109091690075ea10937))
* add per-pool claim-arrival demand tracker ([0c8d1ff](https://github.com/mitos-run/mitos/commit/0c8d1ff270ba2d12792f70eb6235b3c1a66b7077))
* add pluggable KMS Wrapper with a local AES-256-GCM KEK provider ([0c0709f](https://github.com/mitos-run/mitos/commit/0c0709f4f37c56d1b321a2353c006c2cc0c79bce))
* add Python streaming exec callbacks and background process handle ([bf7a185](https://github.com/mitos-run/mitos/commit/bf7a18516d6dcd9d30b0315b843ddfb0cfbe24e6))
* add TypeScript streaming exec callbacks and background process handle ([3150202](https://github.com/mitos-run/mitos/commit/3150202d43677942d7206e4d1ef162ab22f47222))
* add vsock exec-stream frame protocol types ([7beb8b9](https://github.com/mitos-run/mitos/commit/7beb8b9f57e9bccd24cc24dc4318bb39de4cda0a))
* add warm-pool autoscale metrics (size, in-use, desired, scale events, latency) ([896d353](https://github.com/mitos-run/mitos/commit/896d3538c66b6101f568981b131346a0874db824))
* add warm-pool autoscaling fields to SandboxPool ([fa5f9e2](https://github.com/mitos-run/mitos/commit/fa5f9e2ea73cf27eff83b99b8169f944b4bac9e0))
* add warm-pool desired-count formula with scale-down cooldown ([6d7d4d1](https://github.com/mitos-run/mitos/commit/6d7d4d150e70afeec7aaaa9c88c4ce0a287eeeb1))
* agentrun CLI command tree and Backend interface ([91a9dd8](https://github.com/mitos-run/mitos/commit/91a9dd8cc2867ce3df9e818b95be0926022bf605))
* agentrun dev up/down and cluster backend ([86485fc](https://github.com/mitos-run/mitos/commit/86485fc2118299726441cd4649bbdd9285664083))
* agentrun-mcp binary with an HTTP sandbox backend ([05b8369](https://github.com/mitos-run/mitos/commit/05b8369845896a5a7e2c5aec65676c12232813a5))
* agents.x-k8s.io facade controller maps Sandbox to our husk run path ([cd3fa21](https://github.com/mitos-run/mitos/commit/cd3fa2101b6f77287c1bbef45bc80f5bd941b5f9))
* attach volume drives, placeholder at snapshot, rebind per fork ([cf44c07](https://github.com/mitos-run/mitos/commit/cf44c0791efda14bbd5dc1c63ddabb55f7b8be64))
* autoscale the husk warm pool from claim demand in the pool reconcile ([c5f07c0](https://github.com/mitos-run/mitos/commit/c5f07c03039edf42e3274f9c3f8a21402f763c24))
* benchstat percentile summarization and result formatting ([36c03b6](https://github.com/mitos-run/mitos/commit/36c03b6120789cbfbbfd80f578963829547f86b7))
* bind a sandbox to a workspace and hydrate/dehydrate its revisions ([84aa350](https://github.com/mitos-run/mitos/commit/84aa350ba57efcab224b001331f106639f721849))
* bounded CAS cache with LRU eviction and manifest pinning ([8d0aaaa](https://github.com/mitos-run/mitos/commit/8d0aaaa1bd52eb54f582422276701b684fa6c8f8))
* bulk workspace tar transfer over vsock and CAS hydrate/dehydrate helpers ([041a285](https://github.com/mitos-run/mitos/commit/041a28537302cb1e6be04d87f7a0d4ff219c708c))
* capacity-aware bin-packing node selection ([6f0e3f6](https://github.com/mitos-run/mitos/commit/6f0e3f6f4179ab6ff06ac9f652bff1cfe7334947))
* carry the trace id in the revision.created feed event; docs ([ced246f](https://github.com/mitos-run/mitos/commit/ced246f3c253b9476a6ee8841c1838405ecb8a77))
* CAS transfer interface and HTTP transport for incremental snapshot pull ([2f63ee9](https://github.com/mitos-run/mitos/commit/2f63ee9bb038abc23112696d276e3f2f63b1b5b0))
* claim activates a dormant husk pod in place via the mTLS control channel ([1be9bb1](https://github.com/mitos-run/mitos/commit/1be9bb1f3f2832c33c42a710b47a7723097fc09a))
* claims pend on no capacity and fail cleanly after a bounded wait ([e1d6728](https://github.com/mitos-run/mitos/commit/e1d6728f5cfbc23ee8df194f14f36b0cb55fa78b))
* **cli:** cluster workspace backend ([#21](https://github.com/mitos-run/mitos/issues/21)) ([8dc7289](https://github.com/mitos-run/mitos/commit/8dc728944adedcc51defc7de3eee428bdf295bdb))
* **cli:** mitos ws create|ls|log|diff|fork|revert|rm|bind ([#21](https://github.com/mitos-run/mitos/issues/21)) ([f0458d4](https://github.com/mitos-run/mitos/commit/f0458d4ca6f17e47acd1656bbce268fdc95aac9e))
* **cli:** workspace backend interface and fake ([#21](https://github.com/mitos-run/mitos/issues/21)) ([cf738dd](https://github.com/mitos-run/mitos/commit/cf738dd531c7ba06b33f01d63c217c68eb6e7b34))
* clone per-activation rootfs at husk Prepare ([328712c](https://github.com/mitos-run/mitos/commit/328712c5e394b74fda2b5fc63a96ff79592d3fd6))
* cmd/bench fork-exec and exec round-trip latency driver ([f47453c](https://github.com/mitos-run/mitos/commit/f47453c2016fa8ff2209e532a23440dc8aae87d3))
* complete epic W4 (durable, forkable agent workspaces) prod-grade ([ffbcaef](https://github.com/mitos-run/mitos/commit/ffbcaefcc8af272a1f747e953af5a70f63ff2696))
* controller loads the KEK from --kek-file and injects it into the reconcilers ([f2076a2](https://github.com/mitos-run/mitos/commit/f2076a20199efcab77f41ffc7202c1d6c4998e7f))
* controller owns the per-template encryption key Secret and delivers it ([bd9146a](https://github.com/mitos-run/mitos/commit/bd9146a125cf3bdcfc628a00f589eb44bedfc4e6))
* controller passes template NetworkPolicy to forkd ([44c5703](https://github.com/mitos-run/mitos/commit/44c57034f7b7013fc35a47aab2dd28b259709db8))
* controller wraps the DEK with the KMS and delivers the wrapped DEK over the RPCs ([3723040](https://github.com/mitos-run/mitos/commit/37230408bf60dd979a4e859b89c66a9c9e71d069))
* **controller:** add husk fork-snapshot and remove control clients ([d0875c1](https://github.com/mitos-run/mitos/commit/d0875c1c6fb4e12763a00ea52191a5f6280a973f))
* **controller:** build fork-child husk pods owned by the SandboxFork ([020645f](https://github.com/mitos-run/mitos/commit/020645f37e95a7f020662e0d468c455046ba4290))
* **controller:** live SandboxFork on the husk pod-native path with snapshot GC ([9841e1e](https://github.com/mitos-run/mitos/commit/9841e1e3e236a4fb8ce71099a8c09c6533efc9b3))
* **controller:** mount fork snapshot dir and pin fork child husk pods ([8d1ff8a](https://github.com/mitos-run/mitos/commit/8d1ff8a3121fe6b31bc6f4d85878bf2238a2cacc))
* **controller:** replicate husk PKI secrets into pool namespaces ([30128b2](https://github.com/mitos-run/mitos/commit/30128b27b69daf37f75816397900c16452aecb8b))
* **controller:** replicate husk PKI secrets per pool namespace on reconcile ([731982c](https://github.com/mitos-run/mitos/commit/731982ca5453db1e7e3eb7b874eed8374d9f4949))
* **controller:** set husk pod memory limit with headroom ([1283946](https://github.com/mitos-run/mitos/commit/12839460e01d84bea811d7643b79e77b4bf0470c))
* **controller:** wire husk fork config into the SandboxFork reconciler ([11044e4](https://github.com/mitos-run/mitos/commit/11044e4e1d15884982c101fc462b73264031e434))
* **controller:** wire memory-snapshot seams behind a flag ([#21](https://github.com/mitos-run/mitos/issues/21)) ([b1d3915](https://github.com/mitos-run/mitos/commit/b1d39153b4c1d8675840f4600328a372bd4a0551))
* CoW-aware memory metering counts shared template memory once ([9320294](https://github.com/mitos-run/mitos/commit/9320294be577d283a37755bf7058006fa17b5890))
* daemon stashes the wrapped DEK and KEK id from the mTLS request ([4cfb8b6](https://github.com/mitos-run/mitos/commit/4cfb8b6859773e2f0295ee81febaeb601a7cf124))
* **daemon:** cap concurrent streams per sandbox ([ae8383c](https://github.com/mitos-run/mitos/commit/ae8383cc8157caf96457cdc374493f64f9ea3ea9))
* **daemon:** LLM-legible error envelope with code and remediation ([b8f4c02](https://github.com/mitos-run/mitos/commit/b8f4c0264543917c3578d099f5c58cf5f39d8c82))
* deploy the pod-native default stack (controller husk mode, device plugin, husk-stub image) ([5d13cc0](https://github.com/mitos-run/mitos/commit/5d13cc05198c1f29cc16e053b16d6eb9e6acc32d))
* **deploy:** ship the ghcr-pull image pull secret manifest ([7186314](https://github.com/mitos-run/mitos/commit/7186314fca44ee8131a386a8e260ed435aa919f3))
* **deploy:** stage the guest kernel on KVM nodes via a DaemonSet ([ade4725](https://github.com/mitos-run/mitos/commit/ade4725ea06c1e36f1ef57e7245339be24d1d007))
* dev overlay deploys a mock control plane for agentrun dev up ([a54c778](https://github.com/mitos-run/mitos/commit/a54c778d73b7224e4fb50c57b8ce20de08296c0b))
* encrypt template snapshots at rest in per-scope LUKS containers ([c3d910b](https://github.com/mitos-run/mitos/commit/c3d910b4c3b7cfff11bf42232bfee820755399be))
* engine builds templates from OCI images and runs init in the VM ([1cad6a5](https://github.com/mitos-run/mitos/commit/1cad6a5d06b478721627bdea8a899e5ca8b3363e))
* facade maps Sandbox pause/resume to warm-pool release and fast re-activation ([8e1f92f](https://github.com/mitos-run/mitos/commit/8e1f92f6447c4401b2dfdc0b3bf043062f5e763e))
* facade maps SandboxClaim with warmpool policy to our fork-from-snapshot claim ([e9b21d6](https://github.com/mitos-run/mitos/commit/e9b21d68d42977494039831790e4d40a8d61582a))
* facade maps SandboxTemplate and SandboxWarmPool to our template and pool ([d0d5fbc](https://github.com/mitos-run/mitos/commit/d0d5fbc58bd4180af4b9cd5b5328679d3f3cbe2a))
* feed warm-pool autoscaler from claim arrivals and record claim-wait latency ([cf8d4a0](https://github.com/mitos-run/mitos/commit/cf8d4a0e30f931225283995f1734ee6d4be82064))
* **fork:** add on-disk sandbox journal for crash recovery ([06869e0](https://github.com/mitos-run/mitos/commit/06869e0f367e04b70eaed0f202ae5e8292639c81))
* **fork:** add procfs PID-recycle guard for crash reconcile ([d7d37fc](https://github.com/mitos-run/mitos/commit/d7d37fc4c6edbe76d9414eab6b6f0e7d8a35c470))
* forkd loads the local KEK from --kek-file and fails closed without it ([18ae8e9](https://github.com/mitos-run/mitos/commit/18ae8e9c4d57d5335c3c0218a196f9740a6bd976))
* forkd reports host memory total and per-template capacity estimates ([bf23c94](https://github.com/mitos-run/mitos/commit/bf23c94d37cc4dc55ff9862625ee5db21ea62599))
* forkd runs the DNS proxy and points guests at it for name egress ([7b639fb](https://github.com/mitos-run/mitos/commit/7b639fb3505641ae2ed17d82290f23ae4d9bab1c))
* forkd serves its CAS and pulls templates from a peer ([1979c4e](https://github.com/mitos-run/mitos/commit/1979c4e37bca26301e51fbf3934c6a3f9a413d48))
* forkd takes the encryption key from the mTLS request, not the node ([eaa341c](https://github.com/mitos-run/mitos/commit/eaa341c6518cac2aaf4e677fbc8dd7604385e848))
* forkd unwraps the wrapped DEK via the KMS and zeroizes the plaintext ([a0f1b26](https://github.com/mitos-run/mitos/commit/a0f1b2618f1a1c84ea394dac1e463944e81ad336))
* **forkd:** add POST /v1/run_code/stream NDJSON endpoint ([b253ab9](https://github.com/mitos-run/mitos/commit/b253ab96e03f59bdff514e0f4a4553a88cbde51a))
* **forkd:** add token-gated WebSocket /v1/pty endpoint ([f71fe7f](https://github.com/mitos-run/mitos/commit/f71fe7f228b01bf4f2b22c970a4824c85d9ff789))
* **fork:** enforce MaxSandboxes host-DoS ceiling at Fork ([bfda01f](https://github.com/mitos-run/mitos/commit/bfda01feedf9aabbaf551efc81208aa035cd83eb))
* **fork:** reap or re-adopt pre-crash VMs on forkd startup ([86cfbf4](https://github.com/mitos-run/mitos/commit/86cfbf478acf2e4dd9a2ad517b2f99dc5071f91a))
* git rendezvous pushes workspace repo paths for fork-and-merge ([1ba8931](https://github.com/mitos-run/mitos/commit/1ba8931ed33066088be26723a37956bf6ed87442))
* Grafana dashboard and completed conditions catalogue ([31eb208](https://github.com/mitos-run/mitos/commit/31eb20860b093e4dba276ff926e073d20725f33e))
* guest mounts attached volume drives at their mount paths ([df345e9](https://github.com/mitos-run/mitos/commit/df345e916f8b1e80b6390533d21b13761d1189ba))
* **guest:** add in-guest Jupyter kernel driver for run_code ([b694527](https://github.com/mitos-run/mitos/commit/b694527c7408a7b3cd2a21228b8c856eae43be08))
* **guest:** add kernel manager driving the in-guest run_code kernel ([c48b60b](https://github.com/mitos-run/mitos/commit/c48b60b40876963b6a477123b7725a5c392f9fd2))
* **guest:** allocate PTY shell and pump bidirectional I/O over vsock ([3aeebd0](https://github.com/mitos-run/mitos/commit/3aeebd0859ebe071098d6ee8f08cc4a3307aa176))
* **guest:** route TypeRunCode to a persistent per-sandbox kernel ([072cb4d](https://github.com/mitos-run/mitos/commit/072cb4d8b846c42ece2eb4d396b74c7f7f11f61e))
* husk Activate runs the fork-correctness handshake, fail-closed ([7cc4d1a](https://github.com/mitos-run/mitos/commit/7cc4d1a74dd50954bc11566edc69f119494d7381))
* husk mode builds the snapshot and is the default; raw-forkd behind a flag ([d39b3bd](https://github.com/mitos-run/mitos/commit/d39b3bda4c3b13ca177f7e6c30a420b36ac3634e))
* husk pod PDB, self-heal on delete, claim re-pend on pod loss, drain policy ([dea5f86](https://github.com/mitos-run/mitos/commit/dea5f864d548752b90f658676cd3b3a890d71a99))
* husk pod satisfies PSA restricted minus documented exceptions; networking reconciliation ([778b09b](https://github.com/mitos-run/mitos/commit/778b09b0a6dac11fe61a3a57b9c31714e7052ad8))
* husk pod spec and warm-pool lifecycle controller behind a flag ([a421bbc](https://github.com/mitos-run/mitos/commit/a421bbc01d7db2b5c9dec5e49e4d22ad751d9dc3))
* husk stub mTLS network control server and controller activation client ([c105902](https://github.com/mitos-run/mitos/commit/c105902fef312389956fe31881d30c2f15754e65))
* husk-probe measures CoW page sharing across cgroup v2 memcgs ([cac40ad](https://github.com/mitos-run/mitos/commit/cac40ad7bf049a092e2917c91b90e9a5b32b8ba7))
* **husk-stub:** add fork-snapshot control client mode for CI ([78714c5](https://github.com/mitos-run/mitos/commit/78714c53b1b68e7f2b2d29046c20de5e10b12913))
* **husk:** add fork-snapshot control messages and codecs ([4034e36](https://github.com/mitos-run/mitos/commit/4034e36b11d73800b973c0d984dd9589c1d84c91))
* **husk:** dispatch fork-snapshot and remove ops over mTLS control ([5d4cd34](https://github.com/mitos-run/mitos/commit/5d4cd34ca88dd6fc4d25f351dbfdd7c428945051))
* **husk:** extend vmm interface with Pause and CreateSnapshot ([f50074b](https://github.com/mitos-run/mitos/commit/f50074b93cf506be86a04917a46c5aad03b35de0))
* **husk:** live SandboxFork on the husk pod-native default path ([fffb2a4](https://github.com/mitos-run/mitos/commit/fffb2a400517ec6843980f5d306708e58991f62e))
* **husk:** snapshot the running source VM in place (fork-snapshot op) ([fada0b4](https://github.com/mitos-run/mitos/commit/fada0b4740dd0a77b7147acdc04bdd6e6b07b36d))
* **image:** bake ipykernel and the run_code driver into the base image ([1927dfa](https://github.com/mitos-run/mitos/commit/1927dfa30e182aef9f7aa5e03c7f7e80ec586b3a))
* internal/cas content-addressed snapshot store with dedup ([ef119ee](https://github.com/mitos-run/mitos/commit/ef119ee967876811c8acb5a8be9326104777865b))
* internal/dnsproxy resolves allowlisted names and pins resolved IPs ([a902f71](https://github.com/mitos-run/mitos/commit/a902f712237e3a57e08cc9c8339d50e8ecc50a9e))
* internal/husk dormant-VMM stub with in-place activation ([83b7188](https://github.com/mitos-run/mitos/commit/83b7188fc8af6ba888b99d2b7b31a79bfd05cc3b))
* internal/mcp server, tool definitions, SandboxBackend interface ([edb3c29](https://github.com/mitos-run/mitos/commit/edb3c29e6f6893a4aacdaf410114b988d468983e))
* internal/network Linux tap and nftables egress manager ([c227f5c](https://github.com/mitos-run/mitos/commit/c227f5c50ff36d69e74b27d3f96c854199dd10d8))
* internal/ociroot pulls and flattens OCI images into an ext4 rootfs ([91d44ed](https://github.com/mitos-run/mitos/commit/91d44ed48d70e67ab1480aa33f3a061cdade9136))
* internal/storecrypt per-scope LUKS containers with crypto-shred ([b0dbb94](https://github.com/mitos-run/mitos/commit/b0dbb946a287de04bac4017203fa43c3ef00c363))
* internal/volume node backend with Fresh and reflink Snapshot policies ([785e7ef](https://github.com/mitos-run/mitos/commit/785e7ef3e0c60b811cf959f9a5ca27e8593f5218))
* kubectl sandbox logs and exec; Box competitor positioning ([7e7de26](https://github.com/mitos-run/mitos/commit/7e7de267fd4015aecdb3211c988364c792f0a0ac))
* kubectl sandbox plugin with ls and ps ([d6f2e07](https://github.com/mitos-run/mitos/commit/d6f2e07ec435af604edc6dbb4501c3d0d029d21b))
* kubectl sandbox tree and top operator verbs ([19a1b51](https://github.com/mitos-run/mitos/commit/19a1b51a8a37848dba7a4e34689b9d4c55775ea9))
* kvm device plugin advertises agentrun.dev/kvm and injects /dev/kvm ([25ac7bb](https://github.com/mitos-run/mitos/commit/25ac7bb1fad297c014f05b0511109c473b33dc43))
* memory-snapshot pairing makes a workspace head resumable ([543a537](https://github.com/mitos-run/mitos/commit/543a537fda345e4bdbeea94a3606f2f020cc50b0))
* metering endpoint, CoW disk accounting, corrected metrics ([7702738](https://github.com/mitos-run/mitos/commit/77027383629fc32fc0298c7e872a44222af28927))
* mount writable rootfs CoW dir and pass clone flags to husk pod ([a3ead1c](https://github.com/mitos-run/mitos/commit/a3ead1c57cbec7119fce0c5d142767ba7aa2966e))
* netconf identity allocator, nftables rendering, command builders ([7d899be](https://github.com/mitos-run/mitos/commit/7d899beedcf376cb135f24597232b1836527f4c7))
* OpenTelemetry tracing across the claim and fork path ([51651d7](https://github.com/mitos-run/mitos/commit/51651d7c098a030782180b110080bb90350a08f9))
* pending-claims, orphan-sweep, and claim-error metrics ([a400fa2](https://github.com/mitos-run/mitos/commit/a400fa29c53f1529621473e993e910c73e3d7f9a))
* per-sandbox network identity and NIC attach wired into the engine ([3834ec3](https://github.com/mitos-run/mitos/commit/3834ec30a2525622b2e42fae250729fb3833d49d))
* per-sandbox nftables dynamic allow set for resolved names ([58c45dd](https://github.com/mitos-run/mitos/commit/58c45ddf47f93a6986eab59ae0da6bc99a1e456b))
* plumb template volumes and fork policies through to forkd ([f5331b9](https://github.com/mitos-run/mitos/commit/f5331b90bd3a4271325efe9fedc8d34b5cb91e14))
* pool reconciler builds a template once and distributes by pull ([128222f](https://github.com/mitos-run/mitos/commit/128222f49f1e1d3191c865563de936022170d22f))
* production deploy manifests with RBAC and a kustomize base ([1f13978](https://github.com/mitos-run/mitos/commit/1f139788b4de173c1488ba59af50be983778b240))
* PrometheusRule alerts and runbooks for the exported metrics ([20e4527](https://github.com/mitos-run/mitos/commit/20e45278e1a01a1d905ca3665e50219a196edc96))
* proto carries the wrapped DEK and its KEK id ([ddaa12b](https://github.com/mitos-run/mitos/commit/ddaa12b7d9905d731c6a261618831ce2fa7ef6cd))
* rebind rootfs drive to per-activation clone at husk Activate ([8f29a7e](https://github.com/mitos-run/mitos/commit/8f29a7eeda04f322710d79cb197c0f2ba2ab9620))
* register per-sandbox stream path in forkd and sandbox-server fork paths ([e60814a](https://github.com/mitos-run/mitos/commit/e60814a9b873fe4209c8ff3ccdc622ab61fd2740))
* remove per-activation rootfs clone on husk teardown ([eb43a79](https://github.com/mitos-run/mitos/commit/eb43a790c72aa66d47739ba4f19dee71dc426837))
* **rendezvous:** authenticated git-http rendezvous server ([#21](https://github.com/mitos-run/mitos/issues/21)) ([2976086](https://github.com/mitos-run/mitos/commit/29760868533adbd2075e9ec9199272e3dec4ccc0))
* **sandbox-server:** mount the PTY WebSocket route ([9422834](https://github.com/mitos-run/mitos/commit/94228349bc84b4e7918883ac69dc8ffe879b9344))
* SandboxServer and cluster AgentRun TypeScript clients ([035c497](https://github.com/mitos-run/mitos/commit/035c497e77faabeaf77715440bf6b19fdaaadbde))
* **sdk-python:** add async create_pty on AsyncSandbox ([3057a97](https://github.com/mitos-run/mitos/commit/3057a972acae996bdacdd179de45635a00c91b60))
* **sdk-python:** add Execution/Result/ExecutionError types ([f32e8f1](https://github.com/mitos-run/mitos/commit/f32e8f1b5ea8063780e84f4fa60f65d922a31b59))
* **sdk-python:** add run_code with streaming callbacks ([729ef7d](https://github.com/mitos-run/mitos/commit/729ef7d0e371443f39ee285ea0afd72f77cdf137))
* **sdk-python:** add sandbox.pty interactive terminal handle (sync + async) ([075dc1a](https://github.com/mitos-run/mitos/commit/075dc1a8b5519f72dd8d5d05cf782b57f2e008df))
* **sdk-python:** AsyncAgentRun and AsyncSandbox for the hot paths ([9667bfc](https://github.com/mitos-run/mitos/commit/9667bfca899b46807bff7d595f0535fc98f259fa))
* **sdk-python:** one-liner sandbox(image=...) with a lazy default pool ([b7b312b](https://github.com/mitos-run/mitos/commit/b7b312b72b543a3665f87b36982f55289f91ba91))
* **sdk-python:** structured AgentRunError parsed from the server envelope ([a2b3999](https://github.com/mitos-run/mitos/commit/a2b39999fbf7d6b7e117fd13bda417d35aaeb3f2))
* **sdk-python:** wait_until_ready() and from_name() durable handles ([313e762](https://github.com/mitos-run/mitos/commit/313e76258d779091252abc97b9bd8484fb4cba42))
* **sdk-python:** Workspace handle and git verbs ([#21](https://github.com/mitos-run/mitos/issues/21)) ([be8bc85](https://github.com/mitos-run/mitos/commit/be8bc853fb405edf166e748f677ff6bdf2f55111))
* **sdk-ts:** add Execution/Result/ExecutionError types ([69d43d0](https://github.com/mitos-run/mitos/commit/69d43d0e89aec74f6da6a4f02a66f36f95baddd5))
* **sdk-ts:** add runCode with streaming callbacks ([997dfb6](https://github.com/mitos-run/mitos/commit/997dfb68a0803adca66e83a41a96e98d11e4fae6))
* **sdk-ts:** add sandbox PTY interactive terminal client ([b5fe0d5](https://github.com/mitos-run/mitos/commit/b5fe0d569c05a546d8ddb6ffd7c9c18c7facb486))
* **sdk-ts:** parse the server error envelope; sandbox(image) and fromName ([cc1fddd](https://github.com/mitos-run/mitos/commit/cc1fddddc66dd0bcfbb1d272d2b10e9cef3beb45))
* **sdk-ts:** Workspace handle and git verbs ([#21](https://github.com/mitos-run/mitos/issues/21)) ([23e325c](https://github.com/mitos-run/mitos/commit/23e325ce6e81c08079122e7d01d520097e27087e))
* snapshot format version and compatibility contract (snapcompat) ([3d99f8e](https://github.com/mitos-run/mitos/commit/3d99f8e59e0c6a505cea1f31c43d3b37f07dc778))
* stamp and enforce snapshot compatibility on load ([43fcf81](https://github.com/mitos-run/mitos/commit/43fcf81b6b6c1dec0c1f827ae76caa67b406b45d))
* stamp the reconcile trace id onto the workspace revision; dehydrate span ([541c840](https://github.com/mitos-run/mitos/commit/541c840b990460e73ecff80cb1cc0bb1c1624c26))
* stream guest exec stdout/stderr over vsock with pgroup kill ([34b5861](https://github.com/mitos-run/mitos/commit/34b586153c5c3f5a9a1a8fe167430b7746607123))
* Talos machine configs for KVM-capable worker nodes ([21ce7bb](https://github.com/mitos-run/mitos/commit/21ce7bb3138b5337815c8b17af55f29b627cf006))
* toggleable structured audit log of exec and file operations ([3d0aad4](https://github.com/mitos-run/mitos/commit/3d0aad433108e4d7de69527cb6a8a5ae7fe71359))
* TypeScript SDK package, types, HTTP transport, Sandbox surface ([00e7f01](https://github.com/mitos-run/mitos/commit/00e7f019acfa4c9de71c283c7d0605eee08f16ac))
* verify-on-load snapshot integrity with digest in pool status ([#9](https://github.com/mitos-run/mitos/issues/9)) ([78f4ac9](https://github.com/mitos-run/mitos/commit/78f4ac9a4a24df11a1c553c209339eb428d15ca1))
* **vsock:** add bidirectional PTY methods to StreamConn ([0dabdc4](https://github.com/mitos-run/mitos/commit/0dabdc4390d3c84d82dc4be196280f6da14c15a2))
* **vsock:** add host-side RunCode streaming client method ([bd5ee8f](https://github.com/mitos-run/mitos/commit/bd5ee8faa3015ff950496a5b5beb251deb957a7a))
* **vsock:** add PTY request and frame protocol types ([37ab5c0](https://github.com/mitos-run/mitos/commit/37ab5c04b8b2e208a7538cd122eba270736e919c))
* **vsock:** add TypeRunCode and result/error stream frames ([91a0395](https://github.com/mitos-run/mitos/commit/91a03952e027caa895acb5825497bda8b7489ca0))
* wildcard suffix names in the egress allowlist with anchored matching ([1f2fac5](https://github.com/mitos-run/mitos/commit/1f2fac57a5dd557e2338bb38b07e692be2718a74))
* Workspace and WorkspaceRevision CRD types ([2113f67](https://github.com/mitos-run/mitos/commit/2113f67dddf3ff27f7a669fba04452585eb275ff))
* Workspace controller with revision lineage, retention, and status ([b89f77f](https://github.com/mitos-run/mitos/commit/b89f77f37a045e81cc782538fdf0419ed55e918f))
* workspace outputs extraction with path filter and revision diff ([97d1c22](https://github.com/mitos-run/mitos/commit/97d1c22aa740128da907468c4ca36448d158eb47))
* workspace revision change feed via CloudEvents and Kubernetes Events ([b11d33c](https://github.com/mitos-run/mitos/commit/b11d33c60935ae6600c984b9b0f3eae2a6925c8d))
* **workspace:** fork/revert verbs with LLM-legible rejection ([#21](https://github.com/mitos-run/mitos/issues/21), [#28](https://github.com/mitos-run/mitos/issues/28)) ([a7253f0](https://github.com/mitos-run/mitos/commit/a7253f0436a64406f33c46c569b44b4fad1d0173))
* **workspace:** per-workspace encryption key ([#31](https://github.com/mitos-run/mitos/issues/31), [#21](https://github.com/mitos-run/mitos/issues/21)) ([b84e751](https://github.com/mitos-run/mitos/commit/b84e751e8db722e17a49f55bd121583ae5521453))
* **workspace:** S3 object-store backend ([#21](https://github.com/mitos-run/mitos/issues/21)) ([10e2b18](https://github.com/mitos-run/mitos/commit/10e2b187c060f3debac03e7341921505ced5ce9b))
* **workspace:** Secret-backed git rendezvous credentials ([#21](https://github.com/mitos-run/mitos/issues/21)) ([3d610d5](https://github.com/mitos-run/mitos/commit/3d610d504567c027d6b27a0eb11786bc3d235940))
* **workspace:** wire live husk workspace hydrate/dehydrate transport ([#21](https://github.com/mitos-run/mitos/issues/21)) ([3316ace](https://github.com/mitos-run/mitos/commit/3316acea9ed8de836a91551b19ed23dbd0ab18b8))


### Bug Fixes

* accurate NoCapacity condition per re-pend cause; document husk hard-node-loss latency ([46c2fc2](https://github.com/mitos-run/mitos/commit/46c2fc2a3d3ec467402c70ace8c74bb1b8a5c0b7))
* agentrun help works without a kubeconfig ([a46ef4a](https://github.com/mitos-run/mitos/commit/a46ef4ac372680f76b69f6b3fcee281a0702c66b))
* bench measures fork to first exec, teardown excluded ([913ae5e](https://github.com/mitos-run/mitos/commit/913ae5e7ecce840fc16e21972a368ca8c15f5026))
* CAS CI phase uses guaranteed real files; chmod kvm in snapshot step ([ec6f687](https://github.com/mitos-run/mitos/commit/ec6f687fe4d011ee1d57683f3bf39a5949e310ad))
* CAS removes partial output on verify failure, single-pass PutSnapshot ([71613f5](https://github.com/mitos-run/mitos/commit/71613f564ee2451cb41ebe376dfee1bbc2c819db))
* **ci-runner:** e2e namespace must be PSA enforce: privileged for husk hostPaths ([995dffe](https://github.com/mitos-run/mitos/commit/995dffe966716a26134e9de08557c4cf749b4168))
* **ci-runner:** e2e namespace PSA enforce privileged (husk hostPaths) ([cd78401](https://github.com/mitos-run/mitos/commit/cd7840145efd308426feab3020811b96a0f4aa03))
* **ci-runner:** grant runner daemonsets get/patch for forkd deploy-under-test ([d40c082](https://github.com/mitos-run/mitos/commit/d40c0828536a680e015301779e008c6cc4a9c28b))
* **ci-runner:** grant runner workspaces/workspacerevisions for the W4 e2e ([34c461e](https://github.com/mitos-run/mitos/commit/34c461ef0a4414f00f58a2ee7111c2f018e7f23d))
* **ci-runner:** grant the runner daemonsets get/patch (forkd deploy-under-test) ([48d6590](https://github.com/mitos-run/mitos/commit/48d6590247b637d44cd6293d6d5439f8d941769d))
* **ci-runner:** make the self-hosted runner + cluster-e2e actually work (verified live) ([3daa57f](https://github.com/mitos-run/mitos/commit/3daa57f626fedf905ab72be1369f70197ea53fc4))
* **ci-runner:** registration entrypoint, runAsUser pin, ghcr pull secret ([2b9fba4](https://github.com/mitos-run/mitos/commit/2b9fba438760403c843d3d91e298383d389c066a))
* conflict-tolerant facade test spec updates ([67aa819](https://github.com/mitos-run/mitos/commit/67aa819387017135391ad8cebb764c1554f11cf6))
* conflict-tolerant facade test spec updates ([7dcb7b9](https://github.com/mitos-run/mitos/commit/7dcb7b9454e90cf41fadaeba377d6b517153a1a2))
* **controller:** clean up per-pool demand entry and metric labels on pool delete ([3af993f](https://github.com/mitos-run/mitos/commit/3af993f7dc9542165c58dece6b1e5b4798eee690))
* **controller:** do not let GC node-loss fail a recoverable husk claim ([6dfd6dd](https://github.com/mitos-run/mitos/commit/6dfd6dd33f72e0a2371770af391bda3ed3b3743b))
* **controller:** enforce MaxSandboxes count ceiling at schedule time ([6b63af6](https://github.com/mitos-run/mitos/commit/6b63af6addddacf77828871c6ccd03d4af4edbda))
* **controller:** re-pend raw-forkd claim on ResourceExhausted/Unavailable ([6e9e4ad](https://github.com/mitos-run/mitos/commit/6e9e4ad985793ba744bacdbd8b413f3231286180))
* **controller:** settle an unplaceable husk claim so it stops hot-looping ([#130](https://github.com/mitos-run/mitos/issues/130)) ([4b92e6c](https://github.com/mitos-run/mitos/commit/4b92e6c16effdc1e2c6c354ec9ce9df242c8f273))
* **controller:** tie node health to a forkd liveness probe ([a5f6a1c](https://github.com/mitos-run/mitos/commit/a5f6a1c117de6c4d910ed7b19a76b86fbcb6b0a9))
* **cow:** keep the template mount read-write so snapshot load opens the baked rootfs ([646a15d](https://github.com/mitos-run/mitos/commit/646a15d711e5ae6473d2f207994d1956895e13be))
* default controller namespace to mitos (was mitos-system, inconsistent with the deploy namespace + namespace.yaml after the rename) ([7529d7f](https://github.com/mitos-run/mitos/commit/7529d7f440dad83722cc9cdada3c7fd65d7dc8c6))
* **deploy:** enforce privileged PodSecurity on pool namespaces ([56110f3](https://github.com/mitos-run/mitos/commit/56110f31bcbb5e837c592e13b23eedef3b8f2b21))
* **deploy:** enforce privileged PodSecurity on the mitos namespace ([4d7e2c7](https://github.com/mitos-run/mitos/commit/4d7e2c7fad531b0e27db8c2e443c1c15b58fa067))
* **deploy:** forkd agent-bin, privileged, DOCKER_CONFIG, drop jailer args ([ffe8592](https://github.com/mitos-run/mitos/commit/ffe8592226259d5852e58c30cb99c476612631f3))
* **deploy:** grant leases to the dev mock controller for leader election ([3ef03e4](https://github.com/mitos-run/mitos/commit/3ef03e44841d39a4f99057ae292322896f53bb54))
* **deploy:** wire ghcr-pull onto the controller serviceaccount ([6db590d](https://github.com/mitos-run/mitos/commit/6db590db22645ebe5a9d6733fb369f9cb6a4ed62))
* device-plugin e2e proves /dev/kvm injection on the kvm-capable runner ([7f179b5](https://github.com/mitos-run/mitos/commit/7f179b5f4607465bc1b4f68205e42c18975af8ea))
* dnsproxy refuses when the source guest has no tap mapping ([12dbc96](https://github.com/mitos-run/mitos/commit/12dbc96f1ba4751fb1bf04e632111d57561857fe))
* drop husk-pod reuse so an evicted claim recovers onto a fresh pod ([c190523](https://github.com/mitos-run/mitos/commit/c190523599e8f72c7485d88865d26caeb04a36eb))
* drop husk-pod reuse so an evicted claim recovers onto a fresh pod ([868f235](https://github.com/mitos-run/mitos/commit/868f2350a64ba4675a8ec589e2ed9357e1d615c5))
* **e2e:** thread fork timeout in SDK; make husk-e2e PTY stage best-effort ([a731016](https://github.com/mitos-run/mitos/commit/a73101642f41cca4e5b042928bbf023079e59cbc))
* **e2e:** thread fork timeout in SDK; make husk-e2e PTY stage best-effort ([95bf424](https://github.com/mitos-run/mitos/commit/95bf424e3db7486f2a8a5cf1bb5763651bf42e93))
* emit phase.changed from an uncached read so the event is never dropped ([617808d](https://github.com/mitos-run/mitos/commit/617808d4b79821da3d6590382dd1041b614ecb14))
* encryption cleanup on failed build, destroy in-memory key on shred, serialize container open ([0fc2843](https://github.com/mitos-run/mitos/commit/0fc284353f5e2ae25fb6e6b64e662a08cabdd140))
* enforce run_code timeout so a runaway cell cannot wedge the kernel ([95821bf](https://github.com/mitos-run/mitos/commit/95821bfc7fda1a8fb53bddf8fdf2176c63625a29))
* error on truncated run_code stream in both SDKs ([515562f](https://github.com/mitos-run/mitos/commit/515562f506213c80c612c3ba6e8db5d71d489a87))
* facade warmpool status selector matches husk pod labels; document podTemplate metadata exceptions ([2964cfd](https://github.com/mitos-run/mitos/commit/2964cfdf674b556b7b0d3ced5b0e8f077248c2d1))
* **fork:** close MaxSandboxes admission TOCTOU with an atomic slot reservation ([bc5ec29](https://github.com/mitos-run/mitos/commit/bc5ec293ff0f8b0a94f387ca3fd92ab5d6bfea4d))
* **forkd:** build the guest agent into the image at /usr/local/bin/agent ([47a573d](https://github.com/mitos-run/mitos/commit/47a573d7ce3094f3204bc6c946f45e6a4b467a85))
* **fork:** re-verify pid before killing a re-adopted VM (TOCTOU) ([0336421](https://github.com/mitos-run/mitos/commit/0336421087fd346f9261b2d2075cca380fdb71ef))
* grant the dev mock controller workspace RBAC ([0508896](https://github.com/mitos-run/mitos/commit/0508896a09ba510c2b9478710d639db8e56e877c))
* **guest:** thread dispatcher scanner into PTY handler to preserve coalesced input ([c6336f1](https://github.com/mitos-run/mitos/commit/c6336f1819062478613fe766e1e2c3f5a47707e6))
* husk stub verifies the snapshot (digest + snapcompat) on activate, fail-closed ([d175d6b](https://github.com/mitos-run/mitos/commit/d175d6b7125703b7c6a93419ca70b4b5c2bad92b))
* husk warm pool self-heals independent of the snapshot build ([f37251e](https://github.com/mitos-run/mitos/commit/f37251e196854193a577da5a3d1a3e846966b6c7))
* husk-stub keeps the activated VM alive until shutdown ([183c99c](https://github.com/mitos-run/mitos/commit/183c99c6c827394741ecc01aeacfd2e7e9b3fec9))
* **husk:** clone fork child rootfs from source, snapshot source once ([5146bb3](https://github.com/mitos-run/mitos/commit/5146bb3e78ac29ad1d1ff8b3842bdd05b775bd84))
* **husk:** define the --forks-dir flag the controller emits to husk-stub ([e1dbbd5](https://github.com/mitos-run/mitos/commit/e1dbbd559b9c3f00087a23094366d6b38a9583ab))
* **husk:** gate husk-stub sandbox API on the token, not a fixed id ([12f7273](https://github.com/mitos-run/mitos/commit/12f72731b7ea6b9906e6dcc850ff3676418fae1f))
* **husk:** gate husk-stub sandbox API on the token, not a fixed id ([ecc5be1](https://github.com/mitos-run/mitos/commit/ecc5be1bf015594ba316d0ca7533e1a087f06c0e))
* **husk:** make husk fork child creation idempotent fixed-slot set ([c4379ae](https://github.com/mitos-run/mitos/commit/c4379ae40f82c55e8e47c6732fb83c409cbaa9e5))
* **husk:** mount husk PKI TLS Secrets on fork child pods ([887a7ee](https://github.com/mitos-run/mitos/commit/887a7ee70e5c7ad3a69e12001150671076785426))
* kernel driver enforces timeout and reports kernel death ([88a8020](https://github.com/mitos-run/mitos/commit/88a802032bc97d6f4e00e4a29f09262d4b65370d))
* kvm device plugin container starts under read-only /dev; e2e diagnostics ([8a87301](https://github.com/mitos-run/mitos/commit/8a87301d04aeee1a260f4ace2dbad1b29167a474))
* leader election + warm-pool refill/recycle/reuse ([f2dd2b6](https://github.com/mitos-run/mitos/commit/f2dd2b6e1198b70340e60eeb811ce77d7bf22df9))
* make husk activation work on real KVM (bare-metal validation) ([e322fb5](https://github.com/mitos-run/mitos/commit/e322fb55c125182b9413e34e25713c19bba682f5))
* MCP server ctx-cancel shutdown, empty-file writes, id path safety, fork partial ids ([9881e93](https://github.com/mitos-run/mitos/commit/9881e93be7151ae38d880832ea4b01916c4cdd3b))
* **netconf:** pin exact /30 block on crash re-adoption ([c78e68b](https://github.com/mitos-run/mitos/commit/c78e68b6e3492192bb9377b1a6289c4ec2e4ec82))
* nolint the deprecated GetEventRecorderFor in the feed wiring ([16b2728](https://github.com/mitos-run/mitos/commit/16b2728827777ae687ee2574a4221109cdf36022))
* optimistic-lock husk pod claim; serve token-gated sandbox API in the husk stub ([de9ff7a](https://github.com/mitos-run/mitos/commit/de9ff7aedaafabd9b7bbaa2acd2d712f44673bdf))
* per-pod husk VM id and read-only template mount ([0ab3f5e](https://github.com/mitos-run/mitos/commit/0ab3f5e9d9d7c36965e13c06a106d6e6daa912cb))
* per-sandbox nftables dispatch chains, ForkRunning fails closed on networking ([87d7bca](https://github.com/mitos-run/mitos/commit/87d7bca0bd0d7ece9ac384bd139280d83101f6ff))
* prevent git argument injection in workspace rendezvous (-- separator, ref + scheme guards) ([183be91](https://github.com/mitos-run/mitos/commit/183be9121b3b36f6faebd9fdc8218182d5c8351d))
* re-assert the validateVMID barrier at TemplateManager entry points ([fe0c003](https://github.com/mitos-run/mitos/commit/fe0c003189367af4465ee1fc1ced2ba1ebdfe8c3))
* rebind husk rootfs drive while paused, before resume ([2c4416b](https://github.com/mitos-run/mitos/commit/2c4416bd05bc42ffdb0192e1cb8fa48136f2d7df))
* refuse to deliver the encryption key over a non-mTLS channel ([0c6e455](https://github.com/mitos-run/mitos/commit/0c6e4552d903712a983de7553d2edb972b868db7))
* reliable phase.changed emit (uncached read) and conflict-tolerant test setup ([870a93a](https://github.com/mitos-run/mitos/commit/870a93a69287e4f0bd654c55222a658458d856f4))
* safe-join archive extraction against parent symlink traversal (codeql) ([b15b827](https://github.com/mitos-run/mitos/commit/b15b82795a322f954e8aba91095bd102923cfce3))
* scope husk rootfs CoW clone to a per-pod VM id ([4069942](https://github.com/mitos-run/mitos/commit/4069942aeb488bb83c1e88fcc5fb3902f1de2a20))
* **sdk:** kill() deterministically tears down the background stream ([dac810b](https://github.com/mitos-run/mitos/commit/dac810bb4c3b212e0e517e1f3a07d46239faea4e))
* **sdk:** lazy-import optional async websockets; e2e tests the checked-out SDK ([5672ed4](https://github.com/mitos-run/mitos/commit/5672ed4c3397b56056246cbfff19f060fdfc8cab))
* **sdk:** lazy-import the optional async websockets; e2e installs the checked-out SDK ([81321f1](https://github.com/mitos-run/mitos/commit/81321f1958a032ab041d6cca9c36a1beabd8dd96))
* **sdk:** truncated stream, TS abort, Python background+kill scoping ([1d1fd85](https://github.com/mitos-run/mitos/commit/1d1fd853024411fca784ceed817b1b37c1205e60))
* **sdk:** verify image on default-pool reuse and harden slug ([f4df9e0](https://github.com/mitos-run/mitos/commit/f4df9e0951a678f1b72241fbdcec36310f5400b7))
* serve CAS on a separate TLS listener; peer token via env; traversal test ([9db4d7b](https://github.com/mitos-run/mitos/commit/9db4d7b389bffc820de7ce3c32d46d40e2c7dfb0))
* validate CAS digests to block path traversal (codeql) ([07c67b6](https://github.com/mitos-run/mitos/commit/07c67b6ed163df1ec2864a693a7b24f10fedd86b))
* validate volume names and bake read-only for Share volumes ([c6013f1](https://github.com/mitos-run/mitos/commit/c6013f15f3eb3d5b74231108f68490c2ba6f6710))
* validateVMID barrier at TemplateManager entry points ([f6c3634](https://github.com/mitos-run/mitos/commit/f6c363473ecf7b8a7594a1e6ab5862a647553832))
* vol-smoke seeds the snapshot volume via mkfs -d, no host mount ([fb5a2da](https://github.com/mitos-run/mitos/commit/fb5a2daf767f4fae432093edb558965ca0a229e6))
* wait for agent readiness before snapshot, plumb Spec.Init through the controller ([0f2aca3](https://github.com/mitos-run/mitos/commit/0f2aca38ee2af9401218730a1439c67b2ca89646))
* warm-pool refills per claim + claim release recycles the husk pod ([12d5a5b](https://github.com/mitos-run/mitos/commit/12d5a5b1ab117bda5daf68009adffa0c715ab868))
* **workspace:** allow cross-workspace fork lineage so forks commit + advance the head ([1adb8a5](https://github.com/mitos-run/mitos/commit/1adb8a5ead0a3eb9d492192ded6caad8c2e6d8e0))
* **workspace:** reject userinfo in git rendezvous remote URL ([8f5f9af](https://github.com/mitos-run/mitos/commit/8f5f9af157d0def1c2b4d318251b3e7670709a3d))
* **workspace:** wire husk diff + best-effort git on dehydrate-on-terminate ([b405f04](https://github.com/mitos-run/mitos/commit/b405f04735cf3073b5d644c8feb281eb5c57bcd6))

## [0.2.0](https://github.com/mitos-run/mitos/compare/sandbox-v0.1.0...sandbox-v0.2.0) (2026-06-13)


### Features

* AAAA/IPv6 answers in the name egress allowlist ([314104c](https://github.com/mitos-run/mitos/commit/314104c61ab7589e7007b79e6acc5cc918817c7e))
* add --rootfs-cow-dir and --template-rootfs flags to husk-stub ([d957c7e](https://github.com/mitos-run/mitos/commit/d957c7e4b358f82c6685f82dfc6af1c7d198d2c8))
* add forkd NDJSON exec-stream endpoint and aggregate one-shot exec on it ([51a679d](https://github.com/mitos-run/mitos/commit/51a679dc72550725727a88a127c01c4cf8e28bf5))
* add ForkRunning to ForkEngine interface and MockEngine ([c1366a5](https://github.com/mitos-run/mitos/commit/c1366a5d6ce4df805eb213d3ec3e467bbece7d6b))
* add host vsock ExecStream over a dedicated connection ([1be44f1](https://github.com/mitos-run/mitos/commit/1be44f1d3d96edfe333501c5b87a7d1b382840d9))
* add PatchDrive to the husk vmm interface ([ea8a46a](https://github.com/mitos-run/mitos/commit/ea8a46adf9ae1e91c740e109091690075ea10937))
* add pluggable KMS Wrapper with a local AES-256-GCM KEK provider ([0c0709f](https://github.com/mitos-run/mitos/commit/0c0709f4f37c56d1b321a2353c006c2cc0c79bce))
* add Python streaming exec callbacks and background process handle ([bf7a185](https://github.com/mitos-run/mitos/commit/bf7a18516d6dcd9d30b0315b843ddfb0cfbe24e6))
* add TypeScript streaming exec callbacks and background process handle ([3150202](https://github.com/mitos-run/mitos/commit/3150202d43677942d7206e4d1ef162ab22f47222))
* add vsock exec-stream frame protocol types ([7beb8b9](https://github.com/mitos-run/mitos/commit/7beb8b9f57e9bccd24cc24dc4318bb39de4cda0a))
* agentrun CLI command tree and Backend interface ([91a9dd8](https://github.com/mitos-run/mitos/commit/91a9dd8cc2867ce3df9e818b95be0926022bf605))
* agentrun dev up/down and cluster backend ([86485fc](https://github.com/mitos-run/mitos/commit/86485fc2118299726441cd4649bbdd9285664083))
* agentrun-mcp binary with an HTTP sandbox backend ([05b8369](https://github.com/mitos-run/mitos/commit/05b8369845896a5a7e2c5aec65676c12232813a5))
* agents.x-k8s.io facade controller maps Sandbox to our husk run path ([cd3fa21](https://github.com/mitos-run/mitos/commit/cd3fa2101b6f77287c1bbef45bc80f5bd941b5f9))
* attach volume drives, placeholder at snapshot, rebind per fork ([cf44c07](https://github.com/mitos-run/mitos/commit/cf44c0791efda14bbd5dc1c63ddabb55f7b8be64))
* benchstat percentile summarization and result formatting ([36c03b6](https://github.com/mitos-run/mitos/commit/36c03b6120789cbfbbfd80f578963829547f86b7))
* bind a sandbox to a workspace and hydrate/dehydrate its revisions ([84aa350](https://github.com/mitos-run/mitos/commit/84aa350ba57efcab224b001331f106639f721849))
* bounded CAS cache with LRU eviction and manifest pinning ([8d0aaaa](https://github.com/mitos-run/mitos/commit/8d0aaaa1bd52eb54f582422276701b684fa6c8f8))
* bulk workspace tar transfer over vsock and CAS hydrate/dehydrate helpers ([041a285](https://github.com/mitos-run/mitos/commit/041a28537302cb1e6be04d87f7a0d4ff219c708c))
* capacity-aware bin-packing node selection ([6f0e3f6](https://github.com/mitos-run/mitos/commit/6f0e3f6f4179ab6ff06ac9f652bff1cfe7334947))
* carry the trace id in the revision.created feed event; docs ([ced246f](https://github.com/mitos-run/mitos/commit/ced246f3c253b9476a6ee8841c1838405ecb8a77))
* CAS transfer interface and HTTP transport for incremental snapshot pull ([2f63ee9](https://github.com/mitos-run/mitos/commit/2f63ee9bb038abc23112696d276e3f2f63b1b5b0))
* claim activates a dormant husk pod in place via the mTLS control channel ([1be9bb1](https://github.com/mitos-run/mitos/commit/1be9bb1f3f2832c33c42a710b47a7723097fc09a))
* claim finalizer reaps the backing VM on delete ([a4a2fba](https://github.com/mitos-run/mitos/commit/a4a2fbad7a3067bc9ae02332db2983792a5a62cf))
* claims on lost nodes transition to a terminal NodeLost condition ([5f41d75](https://github.com/mitos-run/mitos/commit/5f41d754ccf7951a2962e6cea6a5744a438e32a1))
* claims pend on no capacity and fail cleanly after a bounded wait ([e1d6728](https://github.com/mitos-run/mitos/commit/e1d6728f5cfbc23ee8df194f14f36b0cb55fa78b))
* clone per-activation rootfs at husk Prepare ([328712c](https://github.com/mitos-run/mitos/commit/328712c5e394b74fda2b5fc63a96ff79592d3fd6))
* cmd/bench fork-exec and exec round-trip latency driver ([f47453c](https://github.com/mitos-run/mitos/commit/f47453c2016fa8ff2209e532a23440dc8aae87d3))
* configure message on the vsock protocol ([180afaa](https://github.com/mitos-run/mitos/commit/180afaa21224ae331c1dd789383121819dde7b44))
* controller calls forkd over gRPC for Fork and ForkRunning ([cabc81c](https://github.com/mitos-run/mitos/commit/cabc81cd1912b754adb0e613d73e22211d593d7e))
* controller loads the KEK from --kek-file and injects it into the reconcilers ([f2076a2](https://github.com/mitos-run/mitos/commit/f2076a20199efcab77f41ffc7202c1d6c4998e7f))
* controller owns the per-template encryption key Secret and delivers it ([bd9146a](https://github.com/mitos-run/mitos/commit/bd9146a125cf3bdcfc628a00f589eb44bedfc4e6))
* controller passes template NetworkPolicy to forkd ([44c5703](https://github.com/mitos-run/mitos/commit/44c57034f7b7013fc35a47aab2dd28b259709db8))
* controller PKI bootstrap and mTLS dialing to forkd ([26d8209](https://github.com/mitos-run/mitos/commit/26d820964934dc6339d2b25b6f5a70f1b08fc28b))
* controller wraps the DEK with the KMS and delivers the wrapped DEK over the RPCs ([3723040](https://github.com/mitos-run/mitos/commit/37230408bf60dd979a4e859b89c66a9c9e71d069))
* **controller:** replicate husk PKI secrets into pool namespaces ([30128b2](https://github.com/mitos-run/mitos/commit/30128b27b69daf37f75816397900c16452aecb8b))
* **controller:** replicate husk PKI secrets per pool namespace on reconcile ([731982c](https://github.com/mitos-run/mitos/commit/731982ca5453db1e7e3eb7b874eed8374d9f4949))
* CoW-aware memory metering counts shared template memory once ([9320294](https://github.com/mitos-run/mitos/commit/9320294be577d283a37755bf7058006fa17b5890))
* daemon stashes the wrapped DEK and KEK id from the mTLS request ([4cfb8b6](https://github.com/mitos-run/mitos/commit/4cfb8b6859773e2f0295ee81febaeb601a7cf124))
* deploy the pod-native default stack (controller husk mode, device plugin, husk-stub image) ([5d13cc0](https://github.com/mitos-run/mitos/commit/5d13cc05198c1f29cc16e053b16d6eb9e6acc32d))
* **deploy:** ship the ghcr-pull image pull secret manifest ([7186314](https://github.com/mitos-run/mitos/commit/7186314fca44ee8131a386a8e260ed435aa919f3))
* **deploy:** stage the guest kernel on KVM nodes via a DaemonSet ([ade4725](https://github.com/mitos-run/mitos/commit/ade4725ea06c1e36f1ef57e7245339be24d1d007))
* dev overlay deploys a mock control plane for agentrun dev up ([a54c778](https://github.com/mitos-run/mitos/commit/a54c778d73b7224e4fb50c57b8ce20de08296c0b))
* encrypt template snapshots at rest in per-scope LUKS containers ([c3d910b](https://github.com/mitos-run/mitos/commit/c3d910b4c3b7cfff11bf42232bfee820755399be))
* engine builds templates from OCI images and runs init in the VM ([1cad6a5](https://github.com/mitos-run/mitos/commit/1cad6a5d06b478721627bdea8a899e5ca8b3363e))
* facade maps Sandbox pause/resume to warm-pool release and fast re-activation ([8e1f92f](https://github.com/mitos-run/mitos/commit/8e1f92f6447c4401b2dfdc0b3bf043062f5e763e))
* facade maps SandboxClaim with warmpool policy to our fork-from-snapshot claim ([e9b21d6](https://github.com/mitos-run/mitos/commit/e9b21d68d42977494039831790e4d40a8d61582a))
* facade maps SandboxTemplate and SandboxWarmPool to our template and pool ([d0d5fbc](https://github.com/mitos-run/mitos/commit/d0d5fbc58bd4180af4b9cd5b5328679d3f3cbe2a))
* forkd activity tracking and ListSandboxes RPC ([48a537d](https://github.com/mitos-run/mitos/commit/48a537dc83712f932061d5f1a0d964fdb7194c26))
* forkd delivers claim env+secrets to the guest, strict on real engines ([5433dff](https://github.com/mitos-run/mitos/commit/5433dffba4c3966a574f74d3bc67ab646bcb4bdb))
* forkd gRPC requires controller mTLS identity when TLS is configured ([9c127aa](https://github.com/mitos-run/mitos/commit/9c127aab5e751178c7ee6aed37a9b142faf7ac2e))
* forkd loads the local KEK from --kek-file and fails closed without it ([18ae8e9](https://github.com/mitos-run/mitos/commit/18ae8e9c4d57d5335c3c0218a196f9740a6bd976))
* forkd notifies guests on fork; restore without reseed fails closed ([527d8a8](https://github.com/mitos-run/mitos/commit/527d8a8b0374d5875f82cd71c8a976b4581592a0))
* forkd pod discovery with capacity heartbeats ([706b857](https://github.com/mitos-run/mitos/commit/706b857403418458ec84c22dfa5505680f19448b))
* forkd reports host memory total and per-template capacity estimates ([bf23c94](https://github.com/mitos-run/mitos/commit/bf23c94d37cc4dc55ff9862625ee5db21ea62599))
* forkd runs Firecracker under the jailer; daemonset drops privileged ([f7c51fc](https://github.com/mitos-run/mitos/commit/f7c51fc26b075e727ee8e243212705bbbac5115e))
* forkd runs the DNS proxy and points guests at it for name egress ([7b639fb](https://github.com/mitos-run/mitos/commit/7b639fb3505641ae2ed17d82290f23ae4d9bab1c))
* forkd serves its CAS and pulls templates from a peer ([1979c4e](https://github.com/mitos-run/mitos/commit/1979c4e37bca26301e51fbf3934c6a3f9a413d48))
* forkd takes the encryption key from the mTLS request, not the node ([eaa341c](https://github.com/mitos-run/mitos/commit/eaa341c6518cac2aaf4e677fbc8dd7604385e848))
* forkd unwraps the wrapped DEK via the KMS and zeroizes the plaintext ([a0f1b26](https://github.com/mitos-run/mitos/commit/a0f1b2618f1a1c84ea394dac1e463944e81ad336))
* GC reconciler terminates orphan VMs and reconciles after controller restart ([dba061f](https://github.com/mitos-run/mitos/commit/dba061fee6619bf917194155d8949b9f914c48fe))
* generate forkd gRPC code from proto ([5abceba](https://github.com/mitos-run/mitos/commit/5abceba829d62f48411663a58a911dd297d79693))
* git rendezvous pushes workspace repo paths for fork-and-merge ([1ba8931](https://github.com/mitos-run/mitos/commit/1ba8931ed33066088be26723a37956bf6ed87442))
* Grafana dashboard and completed conditions catalogue ([31eb208](https://github.com/mitos-run/mitos/commit/31eb20860b093e4dba276ff926e073d20725f33e))
* guest agent applies configured env+secrets to exec sessions ([ce56697](https://github.com/mitos-run/mitos/commit/ce56697fbc5ff0f90859a6322824edb5fd4068c0))
* guest mounts attached volume drives at their mount paths ([df345e9](https://github.com/mitos-run/mitos/commit/df345e916f8b1e80b6390533d21b13761d1189ba))
* guest NotifyForked reseeds RNG, steps clock, signals userspace ([769e400](https://github.com/mitos-run/mitos/commit/769e400b7d820c5659f43cb1ab916fa525433bab))
* guestenv.Merge with base&lt;configured&lt;request precedence ([c9882b7](https://github.com/mitos-run/mitos/commit/c9882b707d724ab233e3a196c380efd1f3e5a5c7))
* husk Activate runs the fork-correctness handshake, fail-closed ([7cc4d1a](https://github.com/mitos-run/mitos/commit/7cc4d1a74dd50954bc11566edc69f119494d7381))
* husk mode builds the snapshot and is the default; raw-forkd behind a flag ([d39b3bd](https://github.com/mitos-run/mitos/commit/d39b3bda4c3b13ca177f7e6c30a420b36ac3634e))
* husk pod PDB, self-heal on delete, claim re-pend on pod loss, drain policy ([dea5f86](https://github.com/mitos-run/mitos/commit/dea5f864d548752b90f658676cd3b3a890d71a99))
* husk pod satisfies PSA restricted minus documented exceptions; networking reconciliation ([778b09b](https://github.com/mitos-run/mitos/commit/778b09b0a6dac11fe61a3a57b9c31714e7052ad8))
* husk pod spec and warm-pool lifecycle controller behind a flag ([a421bbc](https://github.com/mitos-run/mitos/commit/a421bbc01d7db2b5c9dec5e49e4d22ad751d9dc3))
* husk stub mTLS network control server and controller activation client ([c105902](https://github.com/mitos-run/mitos/commit/c105902fef312389956fe31881d30c2f15754e65))
* husk-probe measures CoW page sharing across cgroup v2 memcgs ([cac40ad](https://github.com/mitos-run/mitos/commit/cac40ad7bf049a092e2917c91b90e9a5b32b8ba7))
* implement forkd gRPC service over ForkEngine ([fc9007b](https://github.com/mitos-run/mitos/commit/fc9007b9c91f13d966a8e4b71fe9a12073dbb386))
* internal PKI with mTLS configs and peer identity extraction ([2f61329](https://github.com/mitos-run/mitos/commit/2f613293dd9d43f0449a78640c7c9def76a72484))
* internal/cas content-addressed snapshot store with dedup ([ef119ee](https://github.com/mitos-run/mitos/commit/ef119ee967876811c8acb5a8be9326104777865b))
* internal/dnsproxy resolves allowlisted names and pins resolved IPs ([a902f71](https://github.com/mitos-run/mitos/commit/a902f712237e3a57e08cc9c8339d50e8ecc50a9e))
* internal/husk dormant-VMM stub with in-place activation ([83b7188](https://github.com/mitos-run/mitos/commit/83b7188fc8af6ba888b99d2b7b31a79bfd05cc3b))
* internal/mcp server, tool definitions, SandboxBackend interface ([edb3c29](https://github.com/mitos-run/mitos/commit/edb3c29e6f6893a4aacdaf410114b988d468983e))
* internal/network Linux tap and nftables egress manager ([c227f5c](https://github.com/mitos-run/mitos/commit/c227f5c50ff36d69e74b27d3f96c854199dd10d8))
* internal/ociroot pulls and flattens OCI images into an ext4 rootfs ([91d44ed](https://github.com/mitos-run/mitos/commit/91d44ed48d70e67ab1480aa33f3a061cdade9136))
* internal/storecrypt per-scope LUKS containers with crypto-shred ([b0dbb94](https://github.com/mitos-run/mitos/commit/b0dbb946a287de04bac4017203fa43c3ef00c363))
* internal/volume node backend with Fresh and reflink Snapshot policies ([785e7ef](https://github.com/mitos-run/mitos/commit/785e7ef3e0c60b811cf959f9a5ca27e8593f5218))
* jailer launch path with per-VM uid, chroot, and path translation ([b1ccf4e](https://github.com/mitos-run/mitos/commit/b1ccf4e230e4276671ad6af244cf58d41dda5b5c))
* kubectl sandbox logs and exec; Box competitor positioning ([7e7de26](https://github.com/mitos-run/mitos/commit/7e7de267fd4015aecdb3211c988364c792f0a0ac))
* kubectl sandbox plugin with ls and ps ([d6f2e07](https://github.com/mitos-run/mitos/commit/d6f2e07ec435af604edc6dbb4501c3d0d029d21b))
* kubectl sandbox tree and top operator verbs ([19a1b51](https://github.com/mitos-run/mitos/commit/19a1b51a8a37848dba7a4e34689b9d4c55775ea9))
* kvm device plugin advertises agentrun.dev/kvm and injects /dev/kvm ([25ac7bb](https://github.com/mitos-run/mitos/commit/25ac7bb1fad297c014f05b0511109c473b33dc43))
* live forks of secret-holding sandboxes require explicit opt-in ([8f0f0ee](https://github.com/mitos-run/mitos/commit/8f0f0eee2283f567743c043c7021223f345c504b))
* maxLifetime and idleTimeout reap claims to a terminal Terminated phase ([d13d337](https://github.com/mitos-run/mitos/commit/d13d337d9b4f0981f00096fb4b9e9b359620a930))
* memory-snapshot pairing makes a workspace head resumable ([543a537](https://github.com/mitos-run/mitos/commit/543a537fda345e4bdbeea94a3606f2f020cc50b0))
* metering endpoint, CoW disk accounting, corrected metrics ([7702738](https://github.com/mitos-run/mitos/commit/77027383629fc32fc0298c7e872a44222af28927))
* mount writable rootfs CoW dir and pass clone flags to husk pod ([a3ead1c](https://github.com/mitos-run/mitos/commit/a3ead1c57cbec7119fce0c5d142767ba7aa2966e))
* netconf identity allocator, nftables rendering, command builders ([7d899be](https://github.com/mitos-run/mitos/commit/7d899beedcf376cb135f24597232b1836527f4c7))
* NodeInfo.HTTPEndpoint and NodesWithTemplate ([f08d680](https://github.com/mitos-run/mitos/commit/f08d680bd060804dfedd97f97d5083a22324a464))
* OpenTelemetry tracing across the claim and fork path ([51651d7](https://github.com/mitos-run/mitos/commit/51651d7c098a030782180b110080bb90350a08f9))
* pending-claims, orphan-sweep, and claim-error metrics ([a400fa2](https://github.com/mitos-run/mitos/commit/a400fa29c53f1529621473e993e910c73e3d7f9a))
* per-sandbox bearer tokens on the forkd sandbox API ([39bd36b](https://github.com/mitos-run/mitos/commit/39bd36bb82cbd6071f8c5f980c98e821fa11d84f))
* per-sandbox network identity and NIC attach wired into the engine ([3834ec3](https://github.com/mitos-run/mitos/commit/3834ec30a2525622b2e42fae250729fb3833d49d))
* per-sandbox nftables dynamic allow set for resolved names ([58c45dd](https://github.com/mitos-run/mitos/commit/58c45ddf47f93a6986eab59ae0da6bc99a1e456b))
* plumb template volumes and fork policies through to forkd ([f5331b9](https://github.com/mitos-run/mitos/commit/f5331b90bd3a4271325efe9fedc8d34b5cb91e14))
* pool controller tracks and creates snapshots via forkd ([dbfa1bf](https://github.com/mitos-run/mitos/commit/dbfa1bfec2401e39ad0678f698540ee2b5a920b8))
* pool reconciler builds a template once and distributes by pull ([128222f](https://github.com/mitos-run/mitos/commit/128222f49f1e1d3191c865563de936022170d22f))
* production deploy manifests with RBAC and a kustomize base ([1f13978](https://github.com/mitos-run/mitos/commit/1f139788b4de173c1488ba59af50be983778b240))
* PrometheusRule alerts and runbooks for the exported metrics ([20e4527](https://github.com/mitos-run/mitos/commit/20e45278e1a01a1d905ca3665e50219a196edc96))
* proto carries the wrapped DEK and its KEK id ([ddaa12b](https://github.com/mitos-run/mitos/commit/ddaa12b7d9905d731c6a261618831ce2fa7ef6cd))
* rebind rootfs drive to per-activation clone at husk Activate ([8f29a7e](https://github.com/mitos-run/mitos/commit/8f29a7eeda04f322710d79cb197c0f2ba2ab9620))
* register per-sandbox stream path in forkd and sandbox-server fork paths ([e60814a](https://github.com/mitos-run/mitos/commit/e60814a9b873fe4209c8ff3ccdc622ab61fd2740))
* remove per-activation rootfs clone on husk teardown ([eb43a79](https://github.com/mitos-run/mitos/commit/eb43a790c72aa66d47739ba4f19dee71dc426837))
* SandboxServer and cluster AgentRun TypeScript clients ([035c497](https://github.com/mitos-run/mitos/commit/035c497e77faabeaf77715440bf6b19fdaaadbde))
* snapshot format version and compatibility contract (snapcompat) ([3d99f8e](https://github.com/mitos-run/mitos/commit/3d99f8e59e0c6a505cea1f31c43d3b37f07dc778))
* stamp and enforce snapshot compatibility on load ([43fcf81](https://github.com/mitos-run/mitos/commit/43fcf81b6b6c1dec0c1f827ae76caa67b406b45d))
* stamp the reconcile trace id onto the workspace revision; dehydrate span ([541c840](https://github.com/mitos-run/mitos/commit/541c840b990460e73ecff80cb1cc0bb1c1624c26))
* stream guest exec stdout/stderr over vsock with pgroup kill ([34b5861](https://github.com/mitos-run/mitos/commit/34b586153c5c3f5a9a1a8fe167430b7746607123))
* Talos machine configs for KVM-capable worker nodes ([21ce7bb](https://github.com/mitos-run/mitos/commit/21ce7bb3138b5337815c8b17af55f29b627cf006))
* toggleable structured audit log of exec and file operations ([3d0aad4](https://github.com/mitos-run/mitos/commit/3d0aad433108e4d7de69527cb6a8a5ae7fe71359))
* TTL cleanup of finished claims for etcd hygiene ([c8b29e8](https://github.com/mitos-run/mitos/commit/c8b29e89ae20f04c5d45aaf3052d42eae578d50c))
* TypeScript SDK package, types, HTTP transport, Sandbox surface ([00e7f01](https://github.com/mitos-run/mitos/commit/00e7f019acfa4c9de71c283c7d0605eee08f16ac))
* verify-on-load snapshot integrity with digest in pool status ([#9](https://github.com/mitos-run/mitos/issues/9)) ([78f4ac9](https://github.com/mitos-run/mitos/commit/78f4ac9a4a24df11a1c553c209339eb428d15ca1))
* wildcard suffix names in the egress allowlist with anchored matching ([1f2fac5](https://github.com/mitos-run/mitos/commit/1f2fac57a5dd557e2338bb38b07e692be2718a74))
* Workspace and WorkspaceRevision CRD types ([2113f67](https://github.com/mitos-run/mitos/commit/2113f67dddf3ff27f7a669fba04452585eb275ff))
* Workspace controller with revision lineage, retention, and status ([b89f77f](https://github.com/mitos-run/mitos/commit/b89f77f37a045e81cc782538fdf0419ed55e918f))
* workspace outputs extraction with path filter and revision diff ([97d1c22](https://github.com/mitos-run/mitos/commit/97d1c22aa740128da907468c4ca36448d158eb47))
* workspace revision change feed via CloudEvents and Kubernetes Events ([b11d33c](https://github.com/mitos-run/mitos/commit/b11d33c60935ae6600c984b9b0f3eae2a6925c8d))


### Bug Fixes

* agentrun help works without a kubeconfig ([a46ef4a](https://github.com/mitos-run/mitos/commit/a46ef4ac372680f76b69f6b3fcee281a0702c66b))
* bench measures fork to first exec, teardown excluded ([913ae5e](https://github.com/mitos-run/mitos/commit/913ae5e7ecce840fc16e21972a368ca8c15f5026))
* bounded, unhealthy-tolerant termination so claim deletion never wedges ([97eeeaf](https://github.com/mitos-run/mitos/commit/97eeeafd33754ac4fff0df92a036de88d0a161bc))
* CAS CI phase uses guaranteed real files; chmod kvm in snapshot step ([ec6f687](https://github.com/mitos-run/mitos/commit/ec6f687fe4d011ee1d57683f3bf39a5949e310ad))
* CAS removes partial output on verify failure, single-pass PutSnapshot ([71613f5](https://github.com/mitos-run/mitos/commit/71613f564ee2451cb41ebe376dfee1bbc2c819db))
* CI go-test installs envtest assets for the controller suite ([421688f](https://github.com/mitos-run/mitos/commit/421688f057d59d36db46c3c74b62cfd0af8acab0))
* CI lint timeout + SDK readme; add API spec v2 ([8f59b0e](https://github.com/mitos-run/mitos/commit/8f59b0e2fed6ff4ec9c6e50d89776be3ff4775f6))
* conflict-tolerant facade test spec updates ([67aa819](https://github.com/mitos-run/mitos/commit/67aa819387017135391ad8cebb764c1554f11cf6))
* conflict-tolerant facade test spec updates ([7dcb7b9](https://github.com/mitos-run/mitos/commit/7dcb7b9454e90cf41fadaeba377d6b517153a1a2))
* **cow:** keep the template mount read-write so snapshot load opens the baked rootfs ([646a15d](https://github.com/mitos-run/mitos/commit/646a15d711e5ae6473d2f207994d1956895e13be))
* default controller namespace to mitos (was mitos-system, inconsistent with the deploy namespace + namespace.yaml after the rename) ([7529d7f](https://github.com/mitos-run/mitos/commit/7529d7f440dad83722cc9cdada3c7fd65d7dc8c6))
* **deploy:** enforce privileged PodSecurity on pool namespaces ([56110f3](https://github.com/mitos-run/mitos/commit/56110f31bcbb5e837c592e13b23eedef3b8f2b21))
* **deploy:** enforce privileged PodSecurity on the mitos namespace ([4d7e2c7](https://github.com/mitos-run/mitos/commit/4d7e2c7fad531b0e27db8c2e443c1c15b58fa067))
* **deploy:** forkd agent-bin, privileged, DOCKER_CONFIG, drop jailer args ([ffe8592](https://github.com/mitos-run/mitos/commit/ffe8592226259d5852e58c30cb99c476612631f3))
* **deploy:** grant leases to the dev mock controller for leader election ([3ef03e4](https://github.com/mitos-run/mitos/commit/3ef03e44841d39a4f99057ae292322896f53bb54))
* **deploy:** wire ghcr-pull onto the controller serviceaccount ([6db590d](https://github.com/mitos-run/mitos/commit/6db590db22645ebe5a9d6733fb369f9cb6a4ed62))
* device-plugin e2e proves /dev/kvm injection on the kvm-capable runner ([7f179b5](https://github.com/mitos-run/mitos/commit/7f179b5f4607465bc1b4f68205e42c18975af8ea))
* discovery data race, conn carry-forward, test-only fake forkd helper ([089c133](https://github.com/mitos-run/mitos/commit/089c1336ec1f43f42c0ec2bec711f7daca4fe037))
* dnsproxy refuses when the source guest has no tap mapping ([12dbc96](https://github.com/mitos-run/mitos/commit/12dbc96f1ba4751fb1bf04e632111d57561857fe))
* drop husk-pod reuse so an evicted claim recovers onto a fresh pod ([c190523](https://github.com/mitos-run/mitos/commit/c190523599e8f72c7485d88865d26caeb04a36eb))
* drop husk-pod reuse so an evicted claim recovers onto a fresh pod ([868f235](https://github.com/mitos-run/mitos/commit/868f2350a64ba4675a8ec589e2ed9357e1d615c5))
* emit phase.changed from an uncached read so the event is never dropped ([617808d](https://github.com/mitos-run/mitos/commit/617808d4b79821da3d6590382dd1041b614ecb14))
* encryption cleanup on failed build, destroy in-memory key on shred, serialize container open ([0fc2843](https://github.com/mitos-run/mitos/commit/0fc284353f5e2ae25fb6e6b64e662a08cabdd140))
* facade warmpool status selector matches husk pod labels; document podTemplate metadata exceptions ([2964cfd](https://github.com/mitos-run/mitos/commit/2964cfdf674b556b7b0d3ced5b0e8f077248c2d1))
* **forkd:** build the guest agent into the image at /usr/local/bin/agent ([47a573d](https://github.com/mitos-run/mitos/commit/47a573d7ce3094f3204bc6c946f45e6a4b467a85))
* ForkRunning metrics parity, agent-registration logging, GetConnection race ([33c8076](https://github.com/mitos-run/mitos/commit/33c8076f2c374d373f4f6769a0d4e2f6b341b87c))
* GC respects live claims by name and TTLs early-failed claims ([0630043](https://github.com/mitos-run/mitos/commit/06300436fb93cfa06fbc5ebb4d391a026f758fa1))
* grant the dev mock controller workspace RBAC ([0508896](https://github.com/mitos-run/mitos/commit/0508896a09ba510c2b9478710d639db8e56e877c))
* guestenv passes through base entries without '='; note additive configure merge ([22c025e](https://github.com/mitos-run/mitos/commit/22c025e379f35bb78e8bd6c5bfd274d25c46f298))
* husk stub verifies the snapshot (digest + snapcompat) on activate, fail-closed ([d175d6b](https://github.com/mitos-run/mitos/commit/d175d6b7125703b7c6a93419ca70b4b5c2bad92b))
* husk warm pool self-heals independent of the snapshot build ([f37251e](https://github.com/mitos-run/mitos/commit/f37251e196854193a577da5a3d1a3e846966b6c7))
* husk-stub keeps the activated VM alive until shutdown ([183c99c](https://github.com/mitos-run/mitos/commit/183c99c6c827394741ecc01aeacfd2e7e9b3fec9))
* kvm device plugin container starts under read-only /dev; e2e diagnostics ([8a87301](https://github.com/mitos-run/mitos/commit/8a87301d04aeee1a260f4ace2dbad1b29167a474))
* leader election + warm-pool refill/recycle/reuse ([f2dd2b6](https://github.com/mitos-run/mitos/commit/f2dd2b6e1198b70340e60eeb811ce77d7bf22df9))
* make husk activation work on real KVM (bare-metal validation) ([e322fb5](https://github.com/mitos-run/mitos/commit/e322fb55c125182b9413e34e25713c19bba682f5))
* MCP server ctx-cancel shutdown, empty-file writes, id path safety, fork partial ids ([9881e93](https://github.com/mitos-run/mitos/commit/9881e93be7151ae38d880832ea4b01916c4cdd3b))
* NodeRegistry zero-value safety; use constructor everywhere ([d1aedd6](https://github.com/mitos-run/mitos/commit/d1aedd6c7c5f5cdae36be3f1ae3f77fbb7042a37))
* nolint the deprecated GetEventRecorderFor in the feed wiring ([16b2728](https://github.com/mitos-run/mitos/commit/16b2728827777ae687ee2574a4221109cdf36022))
* optimistic-lock husk pod claim; serve token-gated sandbox API in the husk stub ([de9ff7a](https://github.com/mitos-run/mitos/commit/de9ff7aedaafabd9b7bbaa2acd2d712f44673bdf))
* per-pod husk VM id and read-only template mount ([0ab3f5e](https://github.com/mitos-run/mitos/commit/0ab3f5e9d9d7c36965e13c06a106d6e6daa912cb))
* per-sandbox nftables dispatch chains, ForkRunning fails closed on networking ([87d7bca](https://github.com/mitos-run/mitos/commit/87d7bca0bd0d7ece9ac384bd139280d83101f6ff))
* prevent git argument injection in workspace rendezvous (-- separator, ref + scheme guards) ([183be91](https://github.com/mitos-run/mitos/commit/183be9121b3b36f6faebd9fdc8218182d5c8351d))
* Python SDK k8s mode speaks the forkd /v1 sandbox API ([9435333](https://github.com/mitos-run/mitos/commit/943533363bd3b9e481b5c1601a7a9479c8c4dc98))
* re-assert the validateVMID barrier at TemplateManager entry points ([fe0c003](https://github.com/mitos-run/mitos/commit/fe0c003189367af4465ee1fc1ced2ba1ebdfe8c3))
* rebind husk rootfs drive while paused, before resume ([2c4416b](https://github.com/mitos-run/mitos/commit/2c4416bd05bc42ffdb0192e1cb8fa48136f2d7df))
* refuse to deliver the encryption key over a non-mTLS channel ([0c6e455](https://github.com/mitos-run/mitos/commit/0c6e4552d903712a983de7553d2edb972b868db7))
* regexp allowlist barrier for vm ids clears codeql path-injection ([252443d](https://github.com/mitos-run/mitos/commit/252443d19850c31131d91aed1cb8e12831f9895a))
* reject parent-directory traversal in jailer paths (codeql path-injection) ([c1558b9](https://github.com/mitos-run/mitos/commit/c1558b974f74f7971a8628fbe94828f6188dd82a))
* relative vsock uds path so forks do not collide; CI fork-correctness phase ([c41e014](https://github.com/mitos-run/mitos/commit/c41e014b1ea13ad66ba31982c4a09e8a6072c241))
* reliable phase.changed emit (uncached read) and conflict-tolerant test setup ([870a93a](https://github.com/mitos-run/mitos/commit/870a93a69287e4f0bd654c55222a658458d856f4))
* safe-join archive extraction against parent symlink traversal (codeql) ([b15b827](https://github.com/mitos-run/mitos/commit/b15b82795a322f954e8aba91095bd102923cfce3))
* scope husk rootfs CoW clone to a per-pod VM id ([4069942](https://github.com/mitos-run/mitos/commit/4069942aeb488bb83c1e88fcc5fb3902f1de2a20))
* **sdk:** kill() deterministically tears down the background stream ([dac810b](https://github.com/mitos-run/mitos/commit/dac810bb4c3b212e0e517e1f3a07d46239faea4e))
* **sdk:** truncated stream, TS abort, Python background+kill scoping ([1d1fd85](https://github.com/mitos-run/mitos/commit/1d1fd853024411fca784ceed817b1b37c1205e60))
* secrets in dedicated proto field, threat-model/roadmap truth pass, gofmt ([747cb36](https://github.com/mitos-run/mitos/commit/747cb360b7f9644626a077ffe47a0c07f4a33226))
* serve CAS on a separate TLS listener; peer token via env; traversal test ([9db4d7b](https://github.com/mitos-run/mitos/commit/9db4d7b389bffc820de7ce3c32d46d40e2c7dfb0))
* stream interceptor, verified-only peer identity, per-identity EKUs ([acaaeb5](https://github.com/mitos-run/mitos/commit/acaaeb5409380cf1ecf4f1d08808aa167d164df4))
* transient NotFound handling, locked node lookup, bounded template builds ([b0ef739](https://github.com/mitos-run/mitos/commit/b0ef73996ca5b1f79849c2f4f7fa4de1f0cf6b15))
* validate CAS digests to block path traversal (codeql) ([07c67b6](https://github.com/mitos-run/mitos/commit/07c67b6ed163df1ec2864a693a7b24f10fedd86b))
* validate sandbox ids, contain chroot paths, reap before uid release, add SYS_CHROOT ([1cd75a7](https://github.com/mitos-run/mitos/commit/1cd75a72975dbec8aef2fa7588b29e2ce4f7648f))
* validate volume names and bake read-only for Share volumes ([c6013f1](https://github.com/mitos-run/mitos/commit/c6013f15f3eb3d5b74231108f68490c2ba6f6710))
* validateVMID barrier at TemplateManager entry points ([f6c3634](https://github.com/mitos-run/mitos/commit/f6c363473ecf7b8a7594a1e6ab5862a647553832))
* vol-smoke seeds the snapshot volume via mkfs -d, no host mount ([fb5a2da](https://github.com/mitos-run/mitos/commit/fb5a2daf767f4fae432093edb558965ca0a229e6))
* wait for agent readiness before snapshot, plumb Spec.Init through the controller ([0f2aca3](https://github.com/mitos-run/mitos/commit/0f2aca38ee2af9401218730a1439c67b2ca89646))
* warm-pool refills per claim + claim release recycles the husk pod ([12d5a5b](https://github.com/mitos-run/mitos/commit/12d5a5b1ab117bda5daf68009adffa0c715ab868))
* zero golangci-lint findings, kind-e2e config file ([a72ac0d](https://github.com/mitos-run/mitos/commit/a72ac0d6ee9a73df7a488f698a363efafb212764))
