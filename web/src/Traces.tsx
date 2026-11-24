import { useMemo, useState } from 'react'
import type { LogEntry, LogQuery, Trace, TraceStep } from './api'
import { dayOf, timeOf, useOlder } from './Logs'

type Props = {
  traces: LogEntry[] // live, oldest first
  project: string | null
  onlyProject: boolean
  setOnlyProject: (v: boolean) => void
  onOpenSource: (path: string, line: number) => void // in the selected project
}

// Traces lists handler runs, newest first: what triggered each one, what it
// logged and called, how long it took and whether it failed.
export function Traces({ traces, project, onlyProject, setOnlyProject, onOpenSource }: Props) {
  const [failed, setFailed] = useState(false)
  const [query, setQuery] = useState('')
  const [expanded, setExpanded] = useState<number | null>(null)

  const byProject = onlyProject && project ? project : undefined
  const text = query.trim().toLowerCase()
  const filter = useMemo<LogQuery>(
    () => ({ traces: true, project: byProject, level: failed ? 'ERROR' : undefined, q: text || undefined }),
    [byProject, failed, text],
  )
  const { older, done, loading, load } = useOlder(traces, filter)

  const shown = useMemo(() => {
    const match = (l: LogEntry) =>
      (!byProject || l.project === byProject) && (!failed || l.level === 'ERROR') && (!text || JSON.stringify(l.trace).toLowerCase().includes(text))
    return older.concat(traces.filter(match)).reverse()
  }, [older, traces, byProject, failed, text])

  let day = ''
  return (
    <section className="gha-logs">
      <header className="gha-panel-header gha-sub-header gha-log-filters">
        <label className="gha-check">
          <input type="checkbox" checked={onlyProject} disabled={!project} onChange={(e) => setOnlyProject(e.target.checked)} />
          only {project ?? 'selected project'}
        </label>
        <label className="gha-check">
          <input type="checkbox" checked={failed} onChange={(e) => setFailed(e.target.checked)} />
          only failed
        </label>
        <input className="gha-search gha-log-search" placeholder="Search, e.g. light.kitchen or turn_on" value={query} onChange={(e) => setQuery(e.target.value)} />
      </header>
      <div className="gha-log-lines">
        {shown.map((l) => {
          const t = l.trace!
          const d = dayOf(l.time)
          const newDay = d !== day
          day = d
          const open = expanded === l.seq
          const src = l.project === project ? onOpenSource : undefined
          return (
            <div key={l.seq}>
              {newDay && <div className="gha-log-day">{d}</div>}
              <div className={`gha-trace${t.failed ? ' gha-trace-failed' : ''}${open ? ' gha-open' : ''}`} onClick={() => setExpanded(open ? null : l.seq)}>
                <span className="gha-var-arrow">{open ? '▾' : '▸'}</span>
                <span className="gha-log-time">{timeOf(l.time)}</span>
                <span className="gha-log-project">{l.project}</span>
                <span className="gha-trace-title">{title(t)}</span>
                <span className="gha-trace-meta">
                  {t.failed ? 'failed · ' : ''}
                  {summary(t)} · {duration(t.dur_us)}
                </span>
              </div>
              {open && (
                <div className="gha-trace-steps">
                  <div className="gha-trace-step">
                    <span className="gha-trace-at">start</span>
                    <span className="gha-trace-msg">
                      {trigger(t)} <Source src={t.src} onOpen={src} />
                    </span>
                  </div>
                  {(t.steps ?? []).map((s, i) => (
                    <Step key={i} step={s} onOpen={src} />
                  ))}
                  <div className="gha-trace-step">
                    <span className="gha-trace-at">+{duration(t.dur_us)}</span>
                    <span className="gha-trace-msg gha-muted">{t.failed ? 'ended with an error' : 'done'}</span>
                  </div>
                </div>
              )}
            </div>
          )
        })}
        {shown.length === 0 && (
          <div className="gha-empty">
            {failed || text
              ? 'No matching traces.'
              : 'No traces yet. Every run of a handler (OnChange, OnEvent, Go) appears here with its log lines and service calls.'}
          </div>
        )}
        <div className="gha-log-older">
          {done ? (
            <span className="gha-muted">Start of the trace history</span>
          ) : (
            <button className="gha-ghost gha-small" disabled={loading} onClick={load}>
              {loading ? 'Loading…' : 'Load older'}
            </button>
          )}
        </div>
      </div>
    </section>
  )
}

function Step({ step: s, onOpen }: { step: TraceStep; onOpen?: (path: string, line: number) => void }) {
  return (
    <div className={`gha-trace-step gha-step-${s.kind}${s.err ? ' gha-step-failed' : ''}`}>
      <span className="gha-trace-at">+{duration(s.at_us)}</span>
      <span className="gha-trace-msg">
        <span className="gha-step-kind">{s.kind}</span> {s.msg}
        {s.kind === 'call' && s.dur_us !== undefined && <span className="gha-muted"> ({duration(s.dur_us)})</span>} <Source src={s.src} onOpen={onOpen} />
        {s.err && <pre className="gha-log-err">{s.err}</pre>}
        {s.data != null && compact(s.data) && <pre className="gha-trace-data">{compact(s.data)}</pre>}
      </span>
    </div>
  )
}

// Source is "main.go:12"; a click opens the line if the file is in the selected project.
function Source({ src, onOpen }: { src?: string; onOpen?: (path: string, line: number) => void }) {
  const m = src ? /^(.*):(\d+)$/.exec(src) : null
  if (!m) return null
  if (!onOpen) return <span className="gha-muted">{src}</span>
  return (
    <a
      className="gha-src"
      onClick={(e) => {
        e.stopPropagation()
        onOpen(m[1], +m[2])
      }}
    >
      {src}
    </a>
  )
}

function title(t: Trace) {
  switch (t.trigger) {
    case 'state':
      if (t.from === undefined) return `${t.name}: appeared as ${t.to}`
      if (t.to === undefined) return `${t.name}: removed`
      return t.from === t.to ? `${t.name}: ${t.to} (attributes changed)` : `${t.name}: ${t.from} → ${t.to}`
    case 'event':
      return `event ${t.name}`
    case 'go':
      return `scheduled run${t.src ? ` (${t.src})` : ''}`
    default:
      return `${t.name} outside a handler`
  }
}

function trigger(t: Trace) {
  switch (t.trigger) {
    case 'state':
      return 'state change handler'
    case 'event':
      return 'event handler'
    case 'go':
      return 'function passed to Go'
    default:
      return 'called from'
  }
}

function summary(t: Trace) {
  const calls = (t.steps ?? []).filter((s) => s.kind === 'call').length
  return calls === 0 ? 'no calls' : calls === 1 ? '1 call' : `${calls} calls`
}

function duration(us: number) {
  if (us < 1000) return `${us} µs`
  if (us < 1_000_000) return `${(us / 1000).toFixed(us < 10_000 ? 1 : 0)} ms`
  return `${(us / 1_000_000).toFixed(1)} s`
}

// compact prints the data of a step: a stack trace as it is, call data
// ({target: {entity_id: [...]}, data: {...}}) on one line without empty parts.
function compact(data: unknown): string {
  if (typeof data === 'string') return data
  const prune = (v: unknown): unknown => {
    if (v === null || typeof v !== 'object') return v ?? undefined
    if (Array.isArray(v)) return v.length ? v.map(prune) : undefined
    const entries = Object.entries(v).map(([k, x]) => [k, prune(x)]).filter(([, x]) => x !== undefined)
    return entries.length ? Object.fromEntries(entries) : undefined
  }
  const pruned = prune(data)
  return pruned === undefined ? '' : JSON.stringify(pruned, null, 1).replace(/\n\s*/g, ' ').replace(/"(\w+)":/g, '$1:')
}
