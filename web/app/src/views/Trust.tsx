// Trust and compliance view. Renders an honest security posture:
// - always: the no-external-review banner with threat-model and vulnerability links
// - self-hosted: data residency and operator control statement
// - hosted: compliance artifacts with honest status labels (never "Certified")
import { useCapabilities } from '../data/query'

const THREAT_MODEL_URL = 'https://github.com/mitos-run/mitos/blob/main/docs/threat-model.md'
const SECURITY_URL = 'https://github.com/mitos-run/mitos/blob/main/SECURITY.md'

const COMPLIANCE_ITEMS = [
  { name: 'SOC2 Type II', status: 'In progress' },
  { name: 'ISO 27001', status: 'In progress' },
  { name: 'HIPAA + BAA', status: 'Available on request' },
  { name: 'DPA', status: 'Available on request' },
  { name: 'Sub-processor list', status: 'Available on request' },
]

function SecurityReviewBanner() {
  return (
    <div
      className="trust-banner"
      role="note"
      aria-label="Security review status"
    >
      <p style={{ margin: 0, marginBottom: 'var(--space-3)', fontWeight: 600 }}>
        No external security review has been performed yet.
      </p>
      <p style={{ margin: 0, marginBottom: 'var(--space-4)', fontSize: 'var(--step--1)' }} className="t-dim">
        The threat model documents the exact per-boundary security status for each component and data path.
      </p>
      <div className="trust-banner-links">
        <a
          href={THREAT_MODEL_URL}
          target="_blank"
          rel="noopener noreferrer"
          style={{ fontSize: 'var(--step--1)' }}
        >
          Threat model
        </a>
        <a
          href={SECURITY_URL}
          target="_blank"
          rel="noopener noreferrer"
          style={{ fontSize: 'var(--step--1)' }}
        >
          Report a vulnerability
        </a>
      </div>
    </div>
  )
}

function SelfHostedPosture() {
  return (
    <section className="trust-section">
      <h3>Data residency</h3>
      <p style={{ fontWeight: 600, marginBottom: 'var(--space-3)' }}>Deployed in the operator cluster</p>
      <p style={{ fontSize: 'var(--step--1)' }} className="t-dim">
        Sandbox state, secrets, and audit events remain inside the operator cluster and are never
        transmitted to external services. The operator controls residency, retention policy, and the
        identity provider (IdP).
      </p>
    </section>
  )
}

function HostedCompliance() {
  return (
    <section className="trust-section">
      <h3>Compliance</h3>
      <p style={{ fontSize: 'var(--step--1)', marginBottom: 'var(--space-5)' }} className="t-dim">
        These are operated and delivered artifacts. Statuses reflect current availability, not completed
        certification cycles.
      </p>
      <div style={{ overflowX: 'auto', marginBottom: 'var(--space-6)' }}>
        <table className="tbl" aria-label="Compliance artifacts">
          <thead>
            <tr>
              <th scope="col">Document</th>
              <th scope="col">Status</th>
            </tr>
          </thead>
          <tbody>
            {COMPLIANCE_ITEMS.map((item) => (
              <tr key={item.name}>
                <td>{item.name}</td>
                <td className="t-dim">{item.status}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <a href="mailto:security@mitos.run" style={{ fontSize: 'var(--step--1)' }}>
        Request compliance documentation
      </a>
    </section>
  )
}

export function Trust() {
  const { data: caps } = useCapabilities()

  return (
    <div>
      <h2>Trust</h2>
      <p style={{ fontSize: 'var(--step--1)', marginBottom: 'var(--space-6)' }} className="t-dim">
        Honest security posture and compliance status for this deployment.
      </p>

      <SecurityReviewBanner />

      {caps?.ownership === 'self-hosted' && <SelfHostedPosture />}
      {caps?.ownership === 'hosted' && <HostedCompliance />}
    </div>
  )
}
