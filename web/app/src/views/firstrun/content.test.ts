// content.test.ts: tests for the first-run content registry.
// TDD: this file was written before content.ts existed.

import { describe, it, expect } from 'vitest'
import { FIRST_RUN, RUNTIMES, getFirstRun, SYNTHETIC_TRIGGER } from './content'

const DASH_RE = /[–—]/

describe('RUNTIMES constant', () => {
  it('exports ids in order python, typescript, cli', () => {
    expect(RUNTIMES.map((r) => r.id)).toEqual(['python', 'typescript', 'cli'])
  })

  it('has a non-empty label for each runtime', () => {
    for (const r of RUNTIMES) {
      expect(r.label.length).toBeGreaterThan(0)
    }
  })
})

describe('getFirstRun', () => {
  it('returns the rollouts entry for slug "rollouts"', () => {
    const entry = getFirstRun('rollouts')
    expect(entry.slug).toBe('rollouts')
  })

  it('rollouts snippets.python contains fork(', () => {
    const entry = getFirstRun('rollouts')
    expect(entry.snippets.python).toContain('fork(')
  })

  it('rollouts snippets.typescript contains fork(', () => {
    const entry = getFirstRun('rollouts')
    expect(entry.snippets.typescript).toContain('fork(')
  })

  it('rollouts snippets.cli contains mitos', () => {
    const entry = getFirstRun('rollouts')
    expect(entry.snippets.cli).toContain('mitos')
  })

  it('getFirstRun(undefined) returns the generic default with all runtimes non-empty', () => {
    const entry = getFirstRun(undefined)
    expect(entry.slug).toBe('default')
    expect(entry.title.length).toBeGreaterThan(0)
    expect(entry.snippets.python.length).toBeGreaterThan(0)
    expect(entry.snippets.typescript.length).toBeGreaterThan(0)
    expect(entry.snippets.cli.length).toBeGreaterThan(0)
  })

  it('getFirstRun("nope") falls back to the generic default with all runtimes non-empty', () => {
    const entry = getFirstRun('nope')
    expect(entry.slug).toBe('default')
    expect(entry.title.length).toBeGreaterThan(0)
    expect(entry.snippets.python.length).toBeGreaterThan(0)
    expect(entry.snippets.typescript.length).toBeGreaterThan(0)
    expect(entry.snippets.cli.length).toBeGreaterThan(0)
  })
})

describe('FIRST_RUN entries: no em or en dashes', () => {
  it.each(FIRST_RUN)('$slug has no em/en dash in any text field', (entry) => {
    expect(DASH_RE.test(entry.title)).toBe(false)
    expect(DASH_RE.test(entry.lede)).toBe(false)
    expect(DASH_RE.test(entry.snippets.python)).toBe(false)
    expect(DASH_RE.test(entry.snippets.typescript)).toBe(false)
    expect(DASH_RE.test(entry.snippets.cli)).toBe(false)
    expect(DASH_RE.test(entry.watchFor)).toBe(false)
  })
})

describe('snippets use the hosted surface a new signup can actually run', () => {
  // AgentRun is the CLUSTER-mode client: it loads a kubeconfig and cannot work
  // for a hosted signup, whose only credential is MITOS_API_KEY. The first-run
  // snippet must use the hosted entry points (mitos.create / SandboxServer) or
  // every new user's first copy-paste dies on ConfigException.
  it('python snippets never use the cluster-mode AgentRun client', () => {
    for (const entry of FIRST_RUN) {
      expect(entry.snippets.python).not.toContain('AgentRun')
      expect(entry.snippets.python).toContain('mitos.create(')
    }
  })

  it('typescript snippets use SandboxServer from the @mitos/sdk package', () => {
    for (const entry of FIRST_RUN) {
      expect(entry.snippets.typescript).not.toContain('AgentRun')
      expect(entry.snippets.typescript).toContain('SandboxServer')
      expect(entry.snippets.typescript).toContain('@mitos/sdk')
    }
  })

  it('cli snippets only use flags the mitos CLI actually has', () => {
    for (const entry of FIRST_RUN) {
      // sandbox create has --pool, not --ready; fork has --count, not --exec.
      expect(entry.snippets.cli).not.toContain('--ready')
      expect(entry.snippets.cli).not.toContain('--exec')
      expect(entry.snippets.cli).toContain('--pool')
      // There is no top-level `mitos exec` verb; the real one is
      // `mitos sandbox exec` (internal/agentcli/cli.go dispatch).
      expect(entry.snippets.cli).not.toMatch(/\nmitos exec /)
    }
  })

  it('snippets are self-contained: no references to files the snippet never creates', () => {
    // A fresh sandbox has no task.py / rollout.py / eval_one.py, and hosted
    // fork re-forks the template, so a file staged in the parent does not
    // reach the children either. A pasted snippet must produce real output,
    // not a swarm of "can't open file" exits.
    for (const entry of FIRST_RUN) {
      for (const runtime of ['python', 'typescript', 'cli'] as const) {
        expect(entry.snippets[runtime]).not.toMatch(/\w+\.(py|js)\b/)
      }
    }
  })
})

describe('SYNTHETIC_TRIGGER curl is a real, plain-curl-able unary route', () => {
  // Every exec RPC on the surface is streaming (Exec is bidi, ExecStream is
  // server-streaming) and rejects plain curl's application/json body with a
  // 415; the only true unary JSON route is POST /v1/fork, which itself
  // creates a Running sandbox and flips the first-activity signal. The curl
  // snippet must stick to that route and never regress to a dead
  // Connect-streaming endpoint.
  it('targets /v1/fork', () => {
    expect(SYNTHETIC_TRIGGER.curl).toContain('/v1/fork')
  })

  it('never references the sandbox.v1.Sandbox Connect service', () => {
    expect(SYNTHETIC_TRIGGER.curl).not.toContain('/sandbox.v1.Sandbox/')
  })
})
