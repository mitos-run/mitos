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
const ROLLOUTS = {
  python: `\
from mitos import AgentRun

sb = AgentRun().sandbox("python", ready=True)
swarm = sb.fork(8)
for run in swarm:
    run.exec(["python", "rollout.py"])
`,
  typescript: `\
import { AgentRun } from "mitos"

const sb = await new AgentRun().sandbox("python", { ready: true })
const swarm = await sb.fork(8)
await Promise.all(swarm.map((run) => run.exec(["python", "rollout.py"])))
`,
  cli: `\
mitos sandbox create --ready python
mitos fork <sandbox-id> --count 8 \\
  --exec "python rollout.py"
`,
}

// Code execution: boot a warm microVM and exec a single command.
const CODE_EXEC = {
  python: `\
from mitos import AgentRun

sb = AgentRun().sandbox("python", ready=True)
result = sb.exec(["python", "-c", "print('hello from the sandbox')"])
print(result.stdout)
`,
  typescript: `\
import { AgentRun } from "mitos"

const sb = await new AgentRun().sandbox("python", { ready: true })
const result = await sb.exec(["python", "-c", "print('hello from the sandbox')"])
console.log(result.stdout)
`,
  cli: `\
mitos sandbox create --ready python
mitos exec <sandbox-id> python -c "print('hello from the sandbox')"
`,
}

// Evals: fork one sandbox per test case, no shared state.
const EVALS = {
  python: `\
from mitos import AgentRun

sb = AgentRun().sandbox("python", ready=True)
cases = sb.fork(len(prompts))
for run, prompt in zip(cases, prompts):
    run.exec(["python", "eval_one.py", "--prompt", prompt])
`,
  typescript: `\
import { AgentRun } from "mitos"

const sb = await new AgentRun().sandbox("python", { ready: true })
const cases = await sb.fork(prompts.length)
await Promise.all(
  cases.map((run, i) => run.exec(["python", "eval_one.py", "--prompt", prompts[i]]))
)
`,
  cli: `\
mitos sandbox create --ready python
mitos fork <sandbox-id> --count <n> \\
  --exec "python eval_one.py --prompt <prompt>"
`,
}

// Default: generic swarm pattern.
const DEFAULT_SNIPPETS = {
  python: `\
from mitos import AgentRun

sb = AgentRun().sandbox("python", ready=True)
swarm = sb.fork(4)
for run in swarm:
    run.exec(["python", "task.py"])
`,
  typescript: `\
import { AgentRun } from "mitos"

const sb = await new AgentRun().sandbox("python", { ready: true })
const swarm = await sb.fork(4)
await Promise.all(swarm.map((run) => run.exec(["python", "task.py"])))
`,
  cli: `\
mitos sandbox create --ready python
mitos fork <sandbox-id> --count 4 \\
  --exec "python task.py"
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
