// Renders an AuditEvent as a natural-language sentence (actor + verb +
// optional object) for the sentence-style surfaces: the Instruments overview's
// recent-activity panel and (for the actor/target cells) the Audit table. A
// per-action template covers every action code the console currently emits;
// an action this map doesn't recognise (e.g. a future invite.* action) falls
// back to showing the raw action code as the verb with no object, so the UI
// never silently drops or garbles an event type it doesn't understand yet.
import type { AuditEvent } from '../api'

export type AuditSentence = {
  actor: string
  verb: string
  object: string
}

type Template = (e: AuditEvent) => { verb: string; object: string }

// targetLabel prefers the resolved target_name; it falls back to the raw
// target id when no name was available at record time (an older event, or a
// lookup miss), matching the Audit table's own target-column fallback.
function targetLabel(e: AuditEvent): string {
  return e.target_name || e.target
}

const TEMPLATES: Record<string, Template> = {
  'key.create': (e) => ({ verb: 'created API key', object: targetLabel(e) }),
  'key.revoke': (e) => ({ verb: 'revoked API key', object: targetLabel(e) }),
  'member.role': (e) => ({ verb: 'changed', object: `${targetLabel(e)}'s role` }),
  'profile.update': () => ({ verb: 'updated the account profile', object: '' }),
  'project.create': (e) => ({ verb: 'created project', object: targetLabel(e) }),
  'sandbox.terminate': (e) => ({ verb: 'terminated sandbox', object: targetLabel(e) }),
  'session.revoke': (e) => ({ verb: 'revoked session', object: targetLabel(e) }),
  'session.revoke_all': () => ({ verb: 'signed out of all sessions', object: '' }),
  'secret.create': (e) => ({ verb: 'created secret', object: targetLabel(e) }),
  'secret.rotate': (e) => ({ verb: 'rotated secret', object: targetLabel(e) }),
  'secret.delete': (e) => ({ verb: 'deleted secret', object: targetLabel(e) }),
  'audit.sink.create': (e) => ({ verb: 'added an audit sink', object: targetLabel(e) }),
  'audit.sink.delete': (e) => ({ verb: 'removed an audit sink', object: targetLabel(e) }),
}

/** renderAuditSentence renders e as {actor, verb, object} for sentence-style
 * display. The actor is "You" when e.actor_id is the viewing account
 * (selfAccountID); otherwise it is the actor's resolved display name, falling
 * back to the raw actor id (an older event, or a lookup miss). An action not
 * in the template map falls back to showing the raw action code as the verb,
 * with no object. */
export function renderAuditSentence(e: AuditEvent, selfAccountID: string): AuditSentence {
  const actor = e.actor_id === selfAccountID ? 'You' : e.actor_name || e.actor_id
  const template = TEMPLATES[e.action]
  if (!template) {
    return { actor, verb: e.action, object: '' }
  }
  const { verb, object } = template(e)
  return { actor, verb, object }
}
