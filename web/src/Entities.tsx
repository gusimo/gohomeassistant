import { useEffect, useMemo, useState } from 'react'
import { api, type EntityState } from './api'

const REFRESH_MS = 10_000
const MAX_ROWS = 200

type Props = { onInsert: (text: string) => void; onClose: () => void }

export function Entities({ onInsert, onClose }: Props) {
  const [entities, setEntities] = useState<EntityState[]>([])
  const [error, setError] = useState<string | null>(null)
  const [query, setQuery] = useState('')
  const [expanded, setExpanded] = useState<string | null>(null)
  const [copied, setCopied] = useState<string | null>(null)

  useEffect(() => {
    let stop = false
    const load = () =>
      api
        .entities()
        .then((e) => !stop && (setEntities(e), setError(null)))
        .catch((e) => !stop && setError(String(e)))
    load()
    const t = setInterval(load, REFRESH_MS)
    return () => {
      stop = true
      clearInterval(t)
    }
  }, [])

  const matches = useMemo(() => {
    const terms = query.toLowerCase().split(/\s+/).filter(Boolean)
    return entities.filter((e) => {
      const hay = `${e.entity_id} ${e.attributes.friendly_name ?? ''} ${e.ref}`.toLowerCase()
      return terms.every((t) => hay.includes(t))
    })
  }, [entities, query])

  const copy = (text: string) => {
    copyText(text)
    setCopied(text)
    setTimeout(() => setCopied((c) => (c === text ? null : c)), 1200)
  }

  return (
    <aside className="gha-entities">
      <header className="gha-panel-header">
        <span>Entities</span>
        <span className="gha-muted">{matches.length}</span>
        <span className="gha-spacer" />
        <button className="gha-ghost" onClick={onClose} title="Close">×</button>
      </header>
      <input
        className="gha-search"
        placeholder="Search, e.g. light kitchen"
        value={query}
        autoFocus
        onChange={(e) => setQuery(e.target.value)}
      />
      {error && <div className="gha-error-banner">{error}</div>}
      <ul className="gha-entity-list">
        {matches.slice(0, MAX_ROWS).map((e) => {
          const open = expanded === e.entity_id
          const unit = e.attributes.unit_of_measurement
          return (
            <li key={e.entity_id} className={open ? 'gha-open' : ''}>
              <div className="gha-entity-row" onClick={() => setExpanded(open ? null : e.entity_id)}>
                <div className="gha-entity-names">
                  <span className="gha-entity-name">{String(e.attributes.friendly_name ?? e.entity_id)}</span>
                  <span className="gha-entity-id">{e.entity_id}</span>
                </div>
                <span className={`gha-entity-state${e.state === 'unavailable' ? ' gha-muted' : ''}`}>
                  {e.state}
                  {unit ? ` ${unit}` : ''}
                </span>
              </div>
              {open && (
                <div className="gha-entity-detail">
                  {e.ref && (
                    <div className="gha-entity-actions">
                      <code>{e.ref}</code>
                      <span className="gha-spacer" />
                      <button className="gha-small" onClick={() => onInsert(e.ref)}>Insert</button>
                      <button className="gha-small gha-ghost" onClick={() => copy(e.ref)}>
                        {copied === e.ref ? 'Copied' : 'Copy'}
                      </button>
                    </div>
                  )}
                  <table className="gha-attrs">
                    <tbody>
                      <tr>
                        <th>entity_id</th>
                        <td>
                          {e.entity_id}{' '}
                          <button className="gha-small gha-ghost" onClick={() => copy(e.entity_id)}>
                            {copied === e.entity_id ? 'Copied' : 'Copy'}
                          </button>
                        </td>
                      </tr>
                      <tr>
                        <th>last_changed</th>
                        <td>{new Date(e.last_changed).toLocaleString()}</td>
                      </tr>
                      {Object.entries(e.attributes).map(([k, v]) => (
                        <tr key={k}>
                          <th>{k}</th>
                          <td>{typeof v === 'string' ? v : JSON.stringify(v)}</td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              )}
            </li>
          )
        })}
        {matches.length > MAX_ROWS && (
          <li className="gha-more">{matches.length - MAX_ROWS} more, refine the search</li>
        )}
      </ul>
    </aside>
  )
}

// copyText also works on plain http (no secure context), where the
// Clipboard API is unavailable.
export function copyText(text: string) {
  if (navigator.clipboard && window.isSecureContext) {
    navigator.clipboard.writeText(text).catch(() => fallbackCopy(text))
  } else fallbackCopy(text)
}

function fallbackCopy(text: string) {
  const ta = document.createElement('textarea')
  ta.value = text
  ta.style.position = 'fixed'
  ta.style.opacity = '0'
  document.body.appendChild(ta)
  ta.select()
  document.execCommand('copy')
  ta.remove()
}
