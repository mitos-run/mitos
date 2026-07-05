// content.ts: first-run content registry, keyed by use-case slug.
// Pure data + a getter. No UI logic here.
//
// Voice rules: concrete numbers only (~27 ms, ~3 MiB, Firecracker <125 ms).
// No em or en dashes. No unverified statistics.

export type Runtime = 'python' | 'typescript' | 'cli'

export const RUNTIMES: { id: Runtime; label: string }[] = [
  { id: 'python', label: 'Python' },
  { id: 'typescript', label: 'TypeScript' },
  { id: 'cli', label: 'CLI' },
]

export type FirstRunContent = {
  slug: string
  title: string
  lede: string
  snippets: Record<Runtime, string>
  watchFor: string
}

// Rollouts: warm sandbox forked into a swarm, one task per fork.
// Hosted surface: mitos.create / SandboxServer resolve MITOS_API_KEY and
// default to https://api.mitos.run; the cluster-mode AgentRun client needs a
// kubeconfig a hosted signup does not have.
const ROLLOUTS = {
  python: `\
import mitos

sb = mitos.create("python")
swarm = sb.fork(8)
for i, run in enumerate(swarm):
    print(run.exec(f"python3 -c 'print({i} ** 2)'").stdout.strip())
`,
  typescript: `\
import { SandboxServer } from "@mitos/sdk"

const sb = await new SandboxServer().fork("python")
const swarm = await sb.fork(8)
const results = await Promise.all(
  swarm.map((run, i) => run.exec(\`python3 -c 'print(\${i} ** 2)'\`))
)
results.forEach((r) => console.log(r.stdout.trim()))
`,
  cli: `\
mitos sandbox create --pool python
mitos fork <sandbox-id> --count 8
mitos sandbox exec <fork-id> python3 -c 'print(42)'
`,
}

// Code execution: boot a warm microVM and exec a single command.
const CODE_EXEC = {
  python: `\
import mitos

sb = mitos.create("python")
result = sb.exec("python3 -c 'print(40 + 2)'")
print(result.stdout)
`,
  typescript: `\
import { SandboxServer } from "@mitos/sdk"

const sb = await new SandboxServer().fork("python")
const result = await sb.exec("python3 -c 'print(40 + 2)'")
console.log(result.stdout)
`,
  cli: `\
mitos sandbox create --pool python
mitos sandbox exec <sandbox-id> python3 -c 'print(40 + 2)'
`,
}

// Evals: fork one sandbox per test case, no shared state.
const EVALS = {
  python: `\
import mitos

prompts = ["2 + 2", "10 * 4.2"]
sb = mitos.create("python")
cases = sb.fork(len(prompts))
for run, prompt in zip(cases, prompts):
    print(run.exec(f"python3 -c 'print({prompt})'").stdout.strip())
`,
  typescript: `\
import { SandboxServer } from "@mitos/sdk"

const prompts = ["2 + 2", "10 * 4.2"]
const sb = await new SandboxServer().fork("python")
const cases = await sb.fork(prompts.length)
const results = await Promise.all(
  cases.map((run, i) => run.exec(\`python3 -c 'print(\${prompts[i]})'\`))
)
results.forEach((r) => console.log(r.stdout.trim()))
`,
  cli: `\
mitos sandbox create --pool python
mitos fork <sandbox-id> --count <n>
mitos sandbox exec <fork-id> python3 -c 'print(2 + 2)'
`,
}

// Default: generic swarm pattern.
const DEFAULT_SNIPPETS = {
  python: `\
import mitos

sb = mitos.create("python")
swarm = sb.fork(4)
for i, run in enumerate(swarm):
    print(run.exec(f"python3 -c 'print({i} ** 2)'").stdout.strip())
`,
  typescript: `\
import { SandboxServer } from "@mitos/sdk"

const sb = await new SandboxServer().fork("python")
const swarm = await sb.fork(4)
const results = await Promise.all(
  swarm.map((run, i) => run.exec(\`python3 -c 'print(\${i} ** 2)'\`))
)
results.forEach((r) => console.log(r.stdout.trim()))
`,
  cli: `\
mitos sandbox create --pool python
mitos fork <sandbox-id> --count 4
mitos sandbox exec <fork-id> python3 -c 'print(42)'
`,
}

export const FIRST_RUN: FirstRunContent[] = [
  {
    slug: 'rollouts',
    title: 'Fork your first swarm of rollouts',
    lede:
      'One warm sandbox forks into eight isolated microVMs in ~27 ms each, carrying only a ~3 MiB copy-on-write delta. Every rollout runs in its own clean environment.',
    snippets: ROLLOUTS,
    watchFor:
      'Your fork tree and live metrics light up here as the forks appear.',
  },
  {
    slug: 'code-execution',
    title: 'Run code in an isolated microVM',
    lede:
      'Each sandbox is a full Firecracker microVM. Boot a warm one in under 125 ms and exec any command with a clean filesystem and network.',
    snippets: CODE_EXEC,
    watchFor: 'Output from your exec call appears here when the run completes.',
  },
  {
    slug: 'evals',
    title: 'Fork a fresh environment for every eval case',
    lede:
      'Fork one warm sandbox into as many isolated runs as you have test cases. Each case gets its own microVM; no shared state, no ordering surprises.',
    snippets: EVALS,
    watchFor:
      'Each eval run streams its result here as it finishes.',
  },
  {
    slug: 'default',
    title: 'Fork your first swarm',
    lede:
      'One warm sandbox forks into many isolated microVMs in ~27 ms each. Every fork is independent; tear them all down when the task is done.',
    snippets: DEFAULT_SNIPPETS,
    watchFor:
      'Your fork tree and live metrics light up here as the forks appear.',
  },
]

const DEFAULT_ENTRY = FIRST_RUN.find((e) => e.slug === 'default')!

/**
 * Returns the FirstRunContent for the given use-case slug.
 * Falls back to the generic default entry for undefined or unknown slugs.
 */
export function getFirstRun(uc?: string): FirstRunContent {
  if (uc === undefined) return DEFAULT_ENTRY
  return FIRST_RUN.find((e) => e.slug === uc) ?? DEFAULT_ENTRY
}

// Synthetic trigger: shown in the 90 second troubleshooting panel so a stuck
// user has something to copy-paste that proves the wiring works, independent
// of whichever runtime tab they picked above. The CLI one-liner is `mitos run
// <command>` (create, exec, terminate, documented in docs/cli.md).
//
// No exec RPC on the surface is plain-curl-able: every one is a connect-go
// streaming RPC (Exec is bidi, ExecStream is server-streaming) and rejects a
// plain curl application/json body with a 415, since streaming RPCs need a
// Connect-enveloped body, not flat JSON. The only true unary JSON route is
// POST /v1/fork (fields: template, id, per internal/mcp/httpbackend.go),
// which itself creates a Running sandbox and flips the first-activity signal
// on its own, so it stands alone as the raw-HTTP alternative below.
export const SYNTHETIC_TRIGGER = {
  cli: 'mitos run "echo hello"',
  curl: `\
curl -s -X POST https://api.mitos.run/v1/fork \\
  -H "Authorization: Bearer $MITOS_API_KEY" -H "Content-Type: application/json" \\
  -d '{"template":"python","id":"trigger-'$(date +%s)'"}'`,
  note: 'Exec streaming needs the CLI or an SDK; a successful fork is already enough to light up your first activity.',
}
