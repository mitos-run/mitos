<!--
Required PR title: conventional commit form, e.g.
  feat(controller): confine sandbox placement to tainted KVM pool
  fix(husk): tear down the tap on partial egress-filter failure
Types: feat, fix, docs, ci, chore, refactor, test.
Describe the user-visible change, not the implementation detail.
-->

## Thinking Path

<!--
Required. Trace your reasoning from the top of the project down to this change,
so a reviewer (or a future agent) can reconstruct WHY this exists, not just what
it does. Blockquote style, 5-8 steps. End with the change and its benefit.
-->

> - Mitos boots Firecracker microVMs and forks them via copy-on-write snapshots, exposed through CRDs.
> - [Which component or subsystem is involved: controller, forkd, guest agent, sandbox-server, SDK]
> - [What problem, gap, or hazard exists today]
> - [Why it needs to be addressed now, and which ROADMAP.md section it serves]
> - This pull request ...
> - The benefit is ...

## Linked Issues or Issue Description

<!--
Required. Pick ONE path:

(A) An issue exists: tag it with `Closes #NNN`, `Fixes #NNN`, or `Related: #NNN`.
(B) No issue exists: describe the problem here following the relevant issue form
    fields (.github/ISSUE_TEMPLATE/bug_report.yml or feature_request.yml).

Only reference PUBLIC github.com/mitos-run/mitos issues and PRs. Do NOT paste
internal or instance-local references that other contributors cannot open:
Paperclip ticket ids (PAP-123 / PAPA-123), `agent://` links, or
localhost/tailnet URLs. See CONTRIBUTING.md, "No internal references".
-->

-

## What Changed

<!-- Bullet list of concrete changes, one bullet per logical unit. -->

-

## Verification

<!--
Required. How can a reviewer confirm this works? List the exact commands run and
suites passed. Per the no-unverified-claims rule, every public number must be
reproducible from bench/.
-->

- [ ] <!-- e.g. make test-unit, make test-controller, golangci-lint run (both GOOS) -->

## Risks

<!-- What could go wrong: crash, node loss, slow etcd, capacity exhaustion,
migration safety, breaking change. Or "Low risk" if genuinely minor. -->

-

## Model Used

<!--
Required. Which AI model produced or assisted this change? Include:
  - Provider and model name (e.g. Claude Opus, GPT, Gemini)
  - Exact model id or version (e.g. claude-opus-4-8)
  - Context window if relevant (e.g. 1M context)
  - Reasoning/thinking mode if applicable
If no AI model was used, write "None, human-authored".
-->

-

## Checklist

- [ ] PR title is a conventional commit (feat, fix, docs, ci, chore, refactor, test)
- [ ] Thinking Path traces from project context to this change
- [ ] Model Used is filled in (with version and capability details)
- [ ] Tests added for behavior changes, in the same commit (TDD)
- [ ] Docs updated in the same PR
- [ ] Threat-model delta (docs/threat-model.md) included if the security surface moved
- [ ] Benchmark run (bench/) included if the hot path was touched
- [ ] No em or en dashes introduced anywhere
- [ ] Secret values never logged, in errors, in condition messages, or on host paths
- [ ] No internal/instance-local references (only public `#NNN` / mitos-run/mitos URLs)
- [ ] Every commit carries a `Signed-off-by` trailer (`git commit -s`)
