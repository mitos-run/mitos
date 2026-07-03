// Projects view: create form (labelled name + description inputs) and project list.
// Supports loading, empty, and error states.
import { useState } from 'react'
import { Link } from '@tanstack/react-router'
import { useProjects, useCreateProject } from '../data/org'
import { Skeleton } from '../ui/Skeleton'
import { EmptyState } from '../ui/EmptyState'
import { useToast } from '../ui/Toast'
import { PageHeader } from '../ui/PageHeader'

export function Projects() {
  const { data: projects = [], isLoading, isError } = useProjects()
  const createProject = useCreateProject()
  const { notify } = useToast()

  const [name, setName] = useState('')
  const [description, setDescription] = useState('')

  function onSubmit(e: React.FormEvent) {
    e.preventDefault()
    createProject.mutate(
      { name, description },
      {
        onSuccess: () => {
          setName('')
          setDescription('')
          notify('Project created', 'ok')
        },
        onError: () => notify('Failed to create project', 'error'),
      },
    )
  }

  return (
    <section>
      <PageHeader title="Projects" lede="Projects group sandboxes and workspaces within the org." />

      <form onSubmit={onSubmit} style={{ marginBottom: 'var(--space-6)' }}>
        <div style={{ display: 'flex', gap: 'var(--space-3)', flexWrap: 'wrap', alignItems: 'flex-end' }}>
          <div>
            <label htmlFor="project-name" style={{ display: 'block', marginBottom: 'var(--space-1)', fontSize: 'var(--step--1)' }}>
              Name
            </label>
            <input
              id="project-name"
              className="mono"
              placeholder="e.g. infra"
              value={name}
              onChange={(e) => setName(e.target.value)}
              required
            />
          </div>

          <div>
            <label htmlFor="project-description" style={{ display: 'block', marginBottom: 'var(--space-1)', fontSize: 'var(--step--1)' }}>
              Description
            </label>
            <input
              id="project-description"
              placeholder="Short description"
              value={description}
              onChange={(e) => setDescription(e.target.value)}
            />
          </div>

          <button
            type="submit"
            className="btn"
            disabled={!name || createProject.isPending}
          >
            Create project
          </button>
        </div>
      </form>

      {isLoading ? (
        <Skeleton rows={3} />
      ) : isError ? (
        <p className="t-dim">Failed to load projects. Please refresh.</p>
      ) : projects.length === 0 ? (
        <EmptyState title="No projects" body="Create your first project to group sandboxes and workspaces." />
      ) : (
        <ul style={{ listStyle: 'none', padding: 0, display: 'flex', flexDirection: 'column', gap: 'var(--space-3)' }}>
          {projects.map((p) => (
            <li
              key={p.id}
              style={{
                padding: 'var(--space-3) var(--space-4)',
                borderRadius: '6px',
                border: '1px solid var(--hairline)',
              }}
            >
              <div style={{ fontWeight: 600 }}>
                <Link to="/projects/$id" params={{ id: p.id }}>{p.name}</Link>
              </div>
              {p.description && (
                <div className="t-dim" style={{ fontSize: 'var(--step--1)', marginTop: 'var(--space-1)' }}>
                  {p.description}
                </div>
              )}
              <div className="t-dim" style={{ fontSize: 'var(--step--1)', marginTop: 'var(--space-1)' }}>
                Created {new Date(p.created_at).toLocaleDateString()}
              </div>
            </li>
          ))}
        </ul>
      )}
    </section>
  )
}
