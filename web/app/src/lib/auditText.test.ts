import { describe, it, expect } from 'vitest'
import { renderAuditSentence } from './auditText'
import type { AuditEvent } from '../api'

function ev(overrides: Partial<AuditEvent>): AuditEvent {
  return {
    org_id: 'o1',
    actor_id: 'acct-1',
    actor_name: '',
    actor_type: 'user',
    action: 'key.create',
    target: 't1',
    target_type: '',
    target_name: '',
    detail: '',
    at: '2026-06-25T12:00:00Z',
    ...overrides,
  }
}

describe('renderAuditSentence', () => {
  it('substitutes "You" when the event actor is the viewing account', () => {
    const s = renderAuditSentence(ev({ actor_id: 'me', actor_name: 'Alice' }), 'me')
    expect(s.actor).toBe('You')
  })

  it('uses the actor display name for other accounts, falling back to the raw id', () => {
    const named = renderAuditSentence(ev({ actor_id: 'acct-2', actor_name: 'Bob' }), 'me')
    expect(named.actor).toBe('Bob')
    const unnamed = renderAuditSentence(ev({ actor_id: 'acct-3', actor_name: '' }), 'me')
    expect(unnamed.actor).toBe('acct-3')
  })

  it('renders key.create with the key name as the object', () => {
    const s = renderAuditSentence(ev({ action: 'key.create', target: 'k1', target_name: 'ci-key' }), 'me')
    expect(s.verb.toLowerCase()).toContain('created')
    expect(s.object).toBe('ci-key')
  })

  it('falls back to the raw target id when target_name is unset', () => {
    const s = renderAuditSentence(ev({ action: 'key.revoke', target: 'k1', target_name: '' }), 'me')
    expect(s.object).toBe('k1')
  })

  it('renders member.role as "changed X\'s role"', () => {
    const s = renderAuditSentence(ev({ action: 'member.role', target: 'acct-9', target_name: 'Carol' }), 'me')
    expect(`${s.verb} ${s.object}`).toBe("changed Carol's role")
  })

  it('renders session.revoke_all with no object ("You signed out of all sessions")', () => {
    const s = renderAuditSentence(ev({ actor_id: 'me', actor_name: 'Alice', action: 'session.revoke_all', target: '' }), 'me')
    expect(s.actor).toBe('You')
    expect(s.verb).toBe('signed out of all sessions')
    expect(s.object).toBe('')
  })

  it('renders profile.update with no object and correct grammar for "You"', () => {
    const s = renderAuditSentence(ev({ actor_id: 'me', actor_name: 'Alice', action: 'profile.update', target: '' }), 'me')
    expect(s.object).toBe('')
    expect(`${s.actor} ${s.verb}`).toBe('You updated the account profile')
  })

  it('renders session.revoke as "revoked session <label>"', () => {
    const s = renderAuditSentence(ev({ action: 'session.revoke', target: 's1', target_name: 'browser' }), 'me')
    expect(`${s.verb} ${s.object}`).toBe('revoked session browser')
  })

  it('falls back to the raw action code for an unknown action (e.g. a future invite.* action)', () => {
    const s = renderAuditSentence(ev({ action: 'invite.create', target: 't1', target_name: '' }), 'me')
    expect(s.verb).toBe('invite.create')
    expect(s.object).toBe('')
  })

  it('covers every action the console currently emits with a real template', () => {
    const actions = [
      'key.create',
      'key.revoke',
      'member.role',
      'profile.update',
      'project.create',
      'sandbox.terminate',
      'session.revoke',
      'session.revoke_all',
      'secret.create',
      'secret.rotate',
      'secret.delete',
      'audit.sink.create',
      'audit.sink.delete',
    ]
    for (const action of actions) {
      const s = renderAuditSentence(ev({ action }), 'me')
      expect(s.verb, `action ${action} should not fall back to the raw code`).not.toBe(action)
    }
  })
})
